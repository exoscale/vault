package diagnose

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

const (
	warningEventName        = "warning"
	actionKey               = "actionKey"
	spotCheckOkEventName    = "spot-check-ok"
	spotCheckWarnEventName  = "spot-check-warn"
	spotCheckErrorEventName = "spot-check-error"
	errorMessageKey         = attribute.Key("error.message")
	nameKey                 = attribute.Key("name")
	messageKey              = attribute.Key("message")
)

var diagnoseSession = struct{}{}
var noopTracer = trace.NewNoopTracerProvider().Tracer("vault-diagnose")

type Session struct {
	tc     *TelemetryCollector
	tracer trace.Tracer
	tp     *sdktrace.TracerProvider
}

// New initializes a Diagnose tracing session.  In particular this wires a TelemetryCollector, which
// synchronously receives and tracks OpenTelemetry spans in order to provide a tree structure of results
// when the outermost span ends.
func New() *Session {
	tc := NewTelemetryCollector()
	//so, _ := stdout.NewExporter(stdout.WithPrettyPrint())
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		//sdktrace.WithSpanProcessor(sdktrace.NewSimpleSpanProcessor(so)),
		sdktrace.WithSpanProcessor(tc),
	)
	tracer := tp.Tracer("vault-diagnose")
	sess := &Session{
		tp:     tp,
		tc:     tc,
		tracer: tracer,
	}
	return sess
}

// Context returns a new context with a defined diagnose session
func Context(ctx context.Context, sess *Session) context.Context {
	return context.WithValue(ctx, diagnoseSession, sess)
}

// Finalize ends the Diagnose session, returning the root of the result tree.  This will be empty until
// the outermost span ends.
func (s *Session) Finalize(ctx context.Context) *Result {
	s.tp.ForceFlush(ctx)
	return s.tc.RootResult
}

// StartSpan starts a "diagnose" span, which is really just an OpenTelemetry Tracing span.
func StartSpan(ctx context.Context, spanName string, options ...trace.SpanOption) (context.Context, trace.Span) {
	sessionCtxVal := ctx.Value(diagnoseSession)
	if sessionCtxVal != nil {

		session := sessionCtxVal.(*Session)
		return session.tracer.Start(ctx, spanName, options...)
	} else {
		return noopTracer.Start(ctx, spanName, options...)
	}
}

// Fail records a failure in the current span
func Fail(ctx context.Context, message string) {
	span := trace.SpanFromContext(ctx)
	span.SetStatus(codes.Error, message)
}

// Error records an error in the current span (but unlike Fail, doesn't set the overall span status to Error)
func Error(ctx context.Context, err error, options ...trace.EventOption) error {
	span := trace.SpanFromContext(ctx)
	span.RecordError(err, options...)
	return err
}

// Warn records a warning on the current span
func Warn(ctx context.Context, msg string) {
	span := trace.SpanFromContext(ctx)
	span.AddEvent(warningEventName, trace.WithAttributes(messageKey.String(msg)))
}

// SpotOk adds an Ok result without adding a new Span.  This should be used for instantaneous checks with no
// possible sub-spans
func SpotOk(ctx context.Context, checkName, message string) {
	addSpotCheckResult(ctx, spotCheckOkEventName, checkName, message)
}

// SpotWarn adds a Warning result without adding a new Span.  This should be used for instantaneous checks with no
// possible sub-spans
func SpotWarn(ctx context.Context, checkName, message string) {
	addSpotCheckResult(ctx, spotCheckWarnEventName, checkName, message)
}

// SpotError adds an Error result without adding a new Span.  This should be used for instantaneous checks with no
// possible sub-spans
func SpotError(ctx context.Context, checkName string, err error) error {
	var message string
	if err != nil {
		message = err.Error()
	}
	addSpotCheckResult(ctx, spotCheckErrorEventName, checkName, message)
	return err
}

func addSpotCheckResult(ctx context.Context, eventName, checkName, message string) {
	span := trace.SpanFromContext(ctx)
	attrs := []trace.EventOption{trace.WithAttributes(nameKey.String(checkName))}
	if message != "" {
		attrs = append(attrs, trace.WithAttributes(messageKey.String(message)))
	}
	span.AddEvent(eventName, attrs...)
}

func SpotCheck(ctx context.Context, checkName string, f func() error) error {
	err := f()
	if err != nil {
		SpotError(ctx, checkName, err)
		return err
	} else {
		SpotOk(ctx, checkName, "")
	}
	return nil
}

// Test creates a new named span, and executes the provided function within it.  If the function returns an error,
// the span is considered to have failed.
func Test(ctx context.Context, spanName string, function func(context.Context) error, options ...trace.SpanOption) error {
	ctx, span := StartSpan(ctx, spanName, options...)
	defer span.End()

	err := function(ctx)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
	}
	return err
}

// WithTimeout wraps a context consuming function, and when called, returns an error if the sub-function does not
// complete within the timeout, e.g.
//
// diagnose.Test(ctx, "my-span", diagnose.WithTimeout(5 * time.Second, myTestFunc))
func WithTimeout(d time.Duration, f func(context.Context) error) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		rch := make(chan error)
		t := time.NewTimer(d)
		defer t.Stop()
		go f(ctx)
		select {
		case <-t.C:
			return fmt.Errorf("timed out after %s", d.String())
		case err := <-rch:
			return err
		}
	}
}
