package zipkin

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/openzipkin/zipkin-go/idgenerator"
	"github.com/openzipkin/zipkin-go/model"
	"github.com/openzipkin/zipkin-go/propagation"
	"github.com/openzipkin/zipkin-go/reporter"
)

// Tracer is our Zipkin tracer implementation.
type Tracer struct {
	defaultTags          map[string]string
	extractFailurePolicy ExtractFailurePolicy
	sampler              Sampler
	generate             idgenerator.IDGenerator
	reporter             reporter.Reporter
	localEndpoint        *model.Endpoint
	noop                 int32 // used as atomic bool (1 = true, 0 = false)
	sharedSpans          bool
	unsampledNoop        bool
}

// NewTracer returns a new Zipkin Tracer.
func NewTracer(reporter reporter.Reporter, options ...TracerOption) (*Tracer, error) {
	// set default tracer options
	t := &Tracer{
		defaultTags:          make(map[string]string),
		extractFailurePolicy: ExtractFailurePolicyRestart,
		sampler:              alwaysSample,
		generate:             idgenerator.NewRandom64(),
		reporter:             reporter,
		localEndpoint:        nil,
		noop:                 0,
		sharedSpans:          true,
		unsampledNoop:        false,
	}

	// process functional options
	for _, option := range options {
		if err := option(t); err != nil {
			return nil, err
		}
	}

	return t, nil
}

// StartSpanFromContext creates and starts a span using the span found in
// context as parent. If no parent span is found a root span is created.
func (t *Tracer) StartSpanFromContext(ctx context.Context, name string, options ...SpanOption) (Span, context.Context) {
	if parentSpan := SpanFromContext(ctx); parentSpan != nil {
		options = append(options, Parent(parentSpan.Context()))
	}
	span := t.StartSpan(name, options...)
	return span, NewContext(ctx, span)
}

// StartSpan creates and starts a span.
func (t *Tracer) StartSpan(name string, options ...SpanOption) Span {
	if atomic.LoadInt32(&t.noop) == 1 {
		return &noopSpan{}
	}
	s := &spanImpl{
		SpanModel: model.SpanModel{
			Kind:          model.Undetermined,
			Name:          name,
			LocalEndpoint: t.localEndpoint,
			Annotations:   make([]model.Annotation, 0),
			Tags:          make(map[string]string),
		},
		flushOnFinish: true,
		tracer:        t,
	}

	// add default tracer tags to span
	for k, v := range t.defaultTags {
		s.Tag(k, v)
	}

	// handle provided functional options
	for _, option := range options {
		option(t, s)
	}

	if s.TraceID.Empty() {
		// create root span
		s.SpanContext.TraceID = t.generate.TraceID()
		s.SpanContext.ID = t.generate.SpanID(s.SpanContext.TraceID)
	} else {
		// valid parent context found
		if t.sharedSpans && s.Kind == model.Server {
			// join span
			s.Shared = true
		} else {
			// regular child span
			parentID := s.SpanContext.ID
			s.SpanContext.ParentID = &parentID
			s.SpanContext.ID = t.generate.SpanID(model.TraceID{})
		}
	}

	if !s.SpanContext.Debug && s.Sampled == nil {
		// deferred sampled context found, invoke sampler
		sampled := t.sampler(s.SpanContext.TraceID.Low)
		s.SpanContext.Sampled = &sampled
		if sampled {
			s.mustCollect = 1
		}
	} else {
		if s.SpanContext.Debug || *s.Sampled {
			s.mustCollect = 1
		}
	}

	if t.unsampledNoop && s.mustCollect == 0 {
		// trace not being sampled and noop requested
		return &noopSpan{
			SpanContext: s.SpanContext,
		}
	}

	// add start time
	if s.Timestamp.IsZero() {
		s.Timestamp = time.Now()
	}

	return s
}

// Extract extracts a SpanContext using the provided Extractor function.
func (t *Tracer) Extract(extractor propagation.Extractor) (sc model.SpanContext) {
	if atomic.LoadInt32(&t.noop) == 1 {
		return
	}
	psc, err := extractor()
	if psc != nil {
		sc = *psc
	}
	sc.Err = err
	return
}

// SetNoop allows for killswitch behavior. If set to true the tracer will return
// noopSpans and all data is dropped. This allows operators to stop tracing in
// risk scenarios. Set back to false to resume tracing.
func (t *Tracer) SetNoop(noop bool) {
	if noop {
		atomic.CompareAndSwapInt32(&t.noop, 0, 1)
	} else {
		atomic.CompareAndSwapInt32(&t.noop, 1, 0)
	}
}

// LocalEndpoint returns a copy of the currently set local endpoint of the
// tracer instance.
func (t *Tracer) LocalEndpoint() *model.Endpoint {
	if t.localEndpoint == nil {
		return nil
	}
	ep := *t.localEndpoint
	return &ep
}
