package queryrange

import (
	"context"
	"fmt"
	reflect "reflect"

	"github.com/go-kit/log/level"
	"github.com/grafana/dskit/tenant"
	"github.com/opentracing/opentracing-go"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/grafana/loki/pkg/loghttp"
	"github.com/grafana/loki/pkg/logql"
	"github.com/grafana/loki/pkg/logqlmodel"
	"github.com/grafana/loki/pkg/querier/queryrange/queryrangebase"
	"github.com/grafana/loki/pkg/util/spanlogger"
)

const (
	DefaultDownstreamConcurrency = 128
)

type DownstreamHandler struct {
	limits Limits
	next   queryrangebase.Handler
}

func ParamsToLokiRequest(params logql.Params, shards logql.Shards) queryrangebase.Request {
	if logql.GetRangeType(params) == logql.InstantType {
		return &LokiInstantRequest{
			Query:     params.Query(),
			Limit:     params.Limit(),
			TimeTs:    params.Start(),
			Direction: params.Direction(),
			Path:      "/loki/api/v1/query", // TODO(owen-d): make this derivable
			Shards:    shards.Encode(),
		}
	}
	return &LokiRequest{
		Query:     params.Query(),
		Limit:     params.Limit(),
		Step:      params.Step().Milliseconds(),
		Interval:  params.Interval().Milliseconds(),
		StartTs:   params.Start(),
		EndTs:     params.End(),
		Direction: params.Direction(),
		Path:      "/loki/api/v1/query_range", // TODO(owen-d): make this derivable
		Shards:    shards.Encode(),
	}
}

// Note: After the introduction of the LimitedRoundTripper,
// bounding concurrency in the downstreamer is mostly redundant
// The reason we don't remove it is to prevent malicious queries
// from creating an unreasonably large number of goroutines, such as
// the case of a query like `a / a / a / a / a ..etc`, which could try
// to shard each leg, quickly dispatching an unreasonable number of goroutines.
// In the future, it's probably better to replace this with a channel based API
// so we don't have to do all this ugly edge case handling/accounting
func (h DownstreamHandler) Downstreamer(ctx context.Context) logql.Downstreamer {
	p := DefaultDownstreamConcurrency

	// We may increase parallelism above the default,
	// ensure we don't end up bottlenecking here.
	if user, err := tenant.TenantID(ctx); err == nil {
		if x := h.limits.MaxQueryParallelism(ctx, user); x > 0 {
			p = x
		}
	}

	locks := make(chan struct{}, p)
	for i := 0; i < p; i++ {
		locks <- struct{}{}
	}
	return &instance{
		parallelism: p,
		locks:       locks,
		handler:     h.next,
	}
}

// instance is an intermediate struct for controlling concurrency across a single query
type instance struct {
	parallelism int
	locks       chan struct{}
	handler     queryrangebase.Handler
}

func (in instance) Downstream(ctx context.Context, queries []logql.DownstreamQuery) ([]logqlmodel.Result, error) {
	return in.For(ctx, queries, func(qry logql.DownstreamQuery) (logqlmodel.Result, error) {
		req := ParamsToLokiRequest(qry.Params, qry.Shards).WithQuery(qry.Expr.String())
		sp, ctx := opentracing.StartSpanFromContext(ctx, "DownstreamHandler.instance")
		defer sp.Finish()
		logger := spanlogger.FromContext(ctx)
		defer logger.Finish()
		level.Debug(logger).Log("shards", fmt.Sprintf("%+v", qry.Shards), "query", req.GetQuery(), "step", req.GetStep(), "handler", reflect.TypeOf(in.handler))

		res, err := in.handler.Do(ctx, req)
		if err != nil {
			return logqlmodel.Result{}, err
		}
		return ResponseToResult(res)
	})
}

// For runs a function against a list of queries, collecting the results or returning an error. The indices are preserved such that input[i] maps to output[i].
func (in instance) For(
	ctx context.Context,
	queries []logql.DownstreamQuery,
	fn func(logql.DownstreamQuery) (logqlmodel.Result, error),
) ([]logqlmodel.Result, error) {
	type resp struct {
		i   int
		res logqlmodel.Result
		err error
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ch := make(chan resp)

	// Make one goroutine to dispatch the other goroutines, bounded by instance parallelism
	go func() {
		for i := 0; i < len(queries); i++ {
			select {
			case <-ctx.Done():
				break
			case <-in.locks:
				go func(i int) {
					// release lock back into pool
					defer func() {
						in.locks <- struct{}{}
					}()

					res, err := fn(queries[i])
					response := resp{
						i:   i,
						res: res,
						err: err,
					}

					// Feed the result into the channel unless the work has completed.
					select {
					case <-ctx.Done():
					case ch <- response:
					}
				}(i)
			}
		}
	}()

	results := make([]logqlmodel.Result, len(queries))
	for i := 0; i < len(queries); i++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case resp := <-ch:
			if resp.err != nil {
				return nil, resp.err
			}
			results[resp.i] = resp.res
		}
	}
	return results, nil
}

// convert to matrix
func sampleStreamToMatrix(streams []queryrangebase.SampleStream) parser.Value {
	xs := make(promql.Matrix, 0, len(streams))
	for _, stream := range streams {
		x := promql.Series{}
		x.Metric = make(labels.Labels, 0, len(stream.Labels))
		for _, l := range stream.Labels {
			x.Metric = append(x.Metric, labels.Label(l))
		}

		x.Points = make([]promql.Point, 0, len(stream.Samples))
		for _, sample := range stream.Samples {
			x.Points = append(x.Points, promql.Point{
				T: sample.TimestampMs,
				V: sample.Value,
			})
		}

		xs = append(xs, x)
	}
	return xs
}

func sampleStreamToVector(streams []queryrangebase.SampleStream) parser.Value {
	xs := make(promql.Vector, 0, len(streams))
	for _, stream := range streams {
		x := promql.Sample{}
		x.Metric = make(labels.Labels, 0, len(stream.Labels))
		for _, l := range stream.Labels {
			x.Metric = append(x.Metric, labels.Label(l))
		}

		x.Point = promql.Point{
			T: stream.Samples[0].TimestampMs,
			V: stream.Samples[0].Value,
		}

		xs = append(xs, x)
	}
	return xs
}

func ResponseToResult(resp queryrangebase.Response) (logqlmodel.Result, error) {
	switch r := resp.(type) {
	case *LokiResponse:
		if r.Error != "" {
			return logqlmodel.Result{}, fmt.Errorf("%s: %s", r.ErrorType, r.Error)
		}

		streams := make(logqlmodel.Streams, 0, len(r.Data.Result))

		for _, stream := range r.Data.Result {
			streams = append(streams, stream)
		}

		return logqlmodel.Result{
			Statistics: r.Statistics,
			Data:       streams,
			Headers:    resp.GetHeaders(),
		}, nil

	case *LokiPromResponse:
		if r.Response.Error != "" {
			return logqlmodel.Result{}, fmt.Errorf("%s: %s", r.Response.ErrorType, r.Response.Error)
		}
		if r.Response.Data.ResultType == loghttp.ResultTypeVector {
			return logqlmodel.Result{
				Statistics: r.Statistics,
				Data:       sampleStreamToVector(r.Response.Data.Result),
				Headers:    resp.GetHeaders(),
			}, nil
		}
		return logqlmodel.Result{
			Statistics: r.Statistics,
			Data:       sampleStreamToMatrix(r.Response.Data.Result),
			Headers:    resp.GetHeaders(),
		}, nil

	default:
		return logqlmodel.Result{}, fmt.Errorf("cannot decode (%T)", resp)
	}
}
