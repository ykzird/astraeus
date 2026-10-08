// Package tracing emits OpenTelemetry traces over OTLP/HTTP.
//
// The wire format is the OpenTelemetry standard, so any OTLP backend - a
// collector, Jaeger, Tempo - can receive these spans. What is hand-rolled is
// only the encoder and the batcher, because the project already hand-rolls its
// Prometheus exposition for the same reason: the protocol is the contract, and
// the binary's dependency list stays short instead of taking on the full SDK
// with its protobuf and gRPC trees.
//
// What is deliberately not here, and would be the first things to add: OTLP over
// gRPC, metrics or logs over OTLP (metrics stay on /metrics, logs stay on
// stderr), sampling policies beyond the parent's sampled flag, and baggage.
//
// Tracing is off unless an endpoint is configured, and every entry point is
// safe to call when it is: a disabled tracer returns no-op spans rather than
// making callers guard with an Enabled check.
package tracing

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/ykzird/astraeus/internal/observability"
)

// SpanKind is how a span relates to its neighbours, in the OTLP enumeration.
type SpanKind int

const (
	// SpanKindInternal is work inside the process.
	SpanKindInternal SpanKind = 1
	// SpanKindServer is a request this process received.
	SpanKindServer SpanKind = 2
	// SpanKindClient is a request this process made.
	SpanKindClient SpanKind = 3
)

// StatusCode is a span's outcome, in the OTLP enumeration.
type StatusCode int

const (
	// StatusUnset is the default: the span completed without an opinion.
	StatusUnset StatusCode = 0
	// StatusOK is an explicit success. It is rarely worth setting.
	StatusOK StatusCode = 1
	// StatusError marks a span whose work failed.
	StatusError StatusCode = 2
)

// Attribute is one span attribute. The value's type selects the OTLP encoding:
// string, int64, float64 or bool.
type Attribute struct {
	Key   string
	Value any
}

// String records a string attribute.
func String(key, value string) Attribute { return Attribute{Key: key, Value: value} }

// Int records an integer attribute.
func Int(key string, value int64) Attribute { return Attribute{Key: key, Value: value} }

// Float records a floating-point attribute.
func Float(key string, value float64) Attribute { return Attribute{Key: key, Value: value} }

// Bool records a boolean attribute.
func Bool(key string, value bool) Attribute { return Attribute{Key: key, Value: value} }

// Config configures a Tracer. An empty Endpoint disables tracing.
type Config struct {
	// ServiceName is the OTLP resource's service.name.
	ServiceName string
	// Version is the server version, recorded on the resource.
	Version string
	// Endpoint is the base URL of an OTLP/HTTP receiver, for example
	// http://127.0.0.1:4318. The /v1/traces path is appended if absent.
	Endpoint string
	// BatchSize is how many spans one request carries. Zero means 64.
	BatchSize int
	// FlushInterval bounds how long a span waits for company. Zero means 5s.
	FlushInterval time.Duration
	// QueueSize is how many finished spans may wait to be exported. Zero means
	// 2048. A full queue drops spans and counts them rather than slowing a
	// request that has already finished.
	QueueSize int

	HTTPClient *http.Client
	Logger     *slog.Logger
	Metrics    *observability.Metrics
}

// Tracer creates spans and exports them.
type Tracer struct {
	enabled  bool
	exporter *exporter
	logger   *slog.Logger
}

// New builds a Tracer. With no endpoint it returns a disabled tracer, which is
// the default: a media server should not need a collector to start.
func New(cfg Config) (*Tracer, error) {
	if cfg.ServiceName == "" {
		cfg.ServiceName = "astraeus-media"
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 64
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = 5 * time.Second
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = 2048
	}

	if cfg.Endpoint == "" {
		return &Tracer{enabled: false, logger: cfg.Logger}, nil
	}
	if err := validateEndpoint(cfg.Endpoint); err != nil {
		return nil, err
	}
	return &Tracer{enabled: true, exporter: newExporter(cfg), logger: cfg.Logger}, nil
}

// Enabled reports whether spans are being exported.
func (t *Tracer) Enabled() bool { return t != nil && t.enabled }

// Shutdown flushes what is buffered and stops the exporter.
func (t *Tracer) Shutdown(ctx context.Context) error {
	if !t.Enabled() {
		return nil
	}
	t.exporter.shutdown(ctx)
	return nil
}

/* ---- span contexts ---- */

type spanKey struct{}
type remoteParentKey struct{}

// SpanFromContext returns the span a context belongs to, if any.
func SpanFromContext(ctx context.Context) *Span {
	if ctx == nil {
		return nil
	}
	span, _ := ctx.Value(spanKey{}).(*Span)
	return span
}

func contextWithSpan(ctx context.Context, span *Span) context.Context {
	return context.WithValue(ctx, spanKey{}, span)
}

func contextWithRemoteParent(ctx context.Context, parent spanContext) context.Context {
	return context.WithValue(ctx, remoteParentKey{}, parent)
}

// parentFromContext finds what a new span should hang from: the span already in
// the context, or failing that a parent carried in on a request.
func parentFromContext(ctx context.Context) (spanContext, bool) {
	if span := SpanFromContext(ctx); span != nil && span.export {
		return span.context, true
	}
	if parent, ok := ctx.Value(remoteParentKey{}).(spanContext); ok {
		return parent, true
	}
	return spanContext{}, false
}

/* ---- spans ---- */

// Span is one unit of traced work.
type Span struct {
	name   string
	kind   SpanKind
	parent [8]byte

	context  spanContext
	start    time.Time
	attrs    []attributeJSON
	status   statusJSON
	export   bool
	finished bool
	tracer   *Tracer
}

// Start begins a span as a child of whatever the context is already part of.
func (t *Tracer) Start(ctx context.Context, name string, kind SpanKind, attrs ...Attribute) (context.Context, *Span) {
	if ctx == nil {
		ctx = context.Background()
	}
	if !t.Enabled() {
		return ctx, &Span{name: name, kind: kind, finished: true}
	}

	parent, hasParent := parentFromContext(ctx)
	// An unsampled parent means this request was already decided against; the
	// span exists for structure but is never exported.
	sampled := !hasParent || parent.sampled
	span := &Span{
		name:    name,
		kind:    kind,
		start:   time.Now(),
		export:  sampled,
		tracer:  t,
		context: spanContext{sampled: sampled},
	}
	if hasParent {
		span.parent = parent.spanID
		span.context.traceID = parent.traceID
	} else if _, err := rand.Read(span.context.traceID[:]); err != nil {
		t.logger.Warn("generating a trace id", "error", err)
		span.export = false
	}
	if _, err := rand.Read(span.context.spanID[:]); err != nil {
		t.logger.Warn("generating a span id", "error", err)
		span.export = false
	}
	span.SetAttributes(attrs...)

	return contextWithSpan(ctx, span), span
}

// SetAttributes adds attributes to a span. It is safe on a no-op span.
func (s *Span) SetAttributes(attrs ...Attribute) {
	if s == nil || s.finished {
		return
	}
	for _, attr := range attrs {
		if value, ok := encodeAttribute(attr); ok {
			s.attrs = append(s.attrs, value)
		}
	}
}

// SetStatus records the span's outcome.
func (s *Span) SetStatus(code StatusCode, message string) {
	if s == nil || s.finished {
		return
	}
	s.status = statusJSON{Code: int(code), Message: message}
}

// RecordError marks the span failed and records the error's message as an
// attribute. The message is what a trace viewer shows; the stack, if any, is the
// logger's business.
func (s *Span) RecordError(err error) {
	if err == nil {
		return
	}
	s.SetStatus(StatusError, err.Error())
	s.SetAttributes(String("error.message", err.Error()))
}

// End finishes the span and queues it for export.
func (s *Span) End() {
	if s == nil || s.finished {
		return
	}
	s.finished = true
	if !s.export || s.tracer == nil || s.tracer.exporter == nil {
		return
	}

	finished := time.Now()
	span := spanJSON{
		TraceID:           hex.EncodeToString(s.context.traceID[:]),
		SpanID:            hex.EncodeToString(s.context.spanID[:]),
		Name:              s.name,
		Kind:              int(s.kind),
		StartTimeUnixNano: formatNanos(s.start),
		EndTimeUnixNano:   formatNanos(finished),
		Attributes:        s.attrs,
		Status:            s.status,
	}
	if s.parent != [8]byte{} {
		span.ParentSpanID = hex.EncodeToString(s.parent[:])
	}
	s.tracer.exporter.enqueue(span)
}

// TraceID returns the span's trace id as hex. It is empty for a span from a
// disabled tracer, which has no trace to report; an unsampled span still knows
// its trace, because the request log should show what it belongs to.
func (s *Span) TraceID() string {
	if s == nil || s.tracer == nil {
		return ""
	}
	return hex.EncodeToString(s.context.traceID[:])
}

// SpanID returns the span's id as hex, or "" for a span with no tracer behind it.
func (s *Span) SpanID() string {
	if s == nil || s.tracer == nil {
		return ""
	}
	return hex.EncodeToString(s.context.spanID[:])
}

// Traceparent renders the W3C header for propagating this span to another
// process, which is what makes a trace cross a service boundary.
func (s *Span) Traceparent() string {
	if s == nil {
		return ""
	}
	return s.context.traceparent()
}

/* ---- the server middleware ---- */

// Middleware traces every request. It reads an incoming traceparent so a trace
// started by a proxy or another service continues here, and records the method,
// path, peer and status.
func (t *Tracer) Middleware(next http.Handler) http.Handler {
	if !t.Enabled() {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		if parent, ok := parseTraceparent(r.Header.Get("traceparent")); ok {
			ctx = contextWithRemoteParent(ctx, parent)
		}

		ctx, span := t.Start(ctx, r.Method+" "+r.URL.Path, SpanKindServer,
			String("http.request.method", r.Method),
			String("url.path", r.URL.Path),
			String("client.address", peerAddress(r)))
		defer span.End()

		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(recorder, r.WithContext(ctx))

		span.SetAttributes(Int("http.response.status_code", int64(recorder.status)))
		if recorder.status >= 500 {
			span.SetStatus(StatusError, http.StatusText(recorder.status))
		}
	})
}

// statusRecorder captures the status code for the span. The request log has its
// own recorder; this one exists so tracing does not have to depend on the API
// package, and so the two can be ordered independently.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

// Unwrap lets http.ResponseController reach the real writer, so a streamed
// response still flushes through the tracing wrapper.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func peerAddress(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// encodeAttribute maps a typed attribute onto the OTLP value oneof, refusing a
// type this encoder does not know rather than stringifying it silently.
func encodeAttribute(attr Attribute) (attributeJSON, bool) {
	switch value := attr.Value.(type) {
	case string:
		return attributeJSON{Key: attr.Key, Value: stringValue(value)}, true
	case int:
		return attributeJSON{Key: attr.Key, Value: intValue(int64(value))}, true
	case int64:
		return attributeJSON{Key: attr.Key, Value: intValue(value)}, true
	case float64:
		return attributeJSON{Key: attr.Key, Value: doubleValue(value)}, true
	case bool:
		return attributeJSON{Key: attr.Key, Value: boolValue(value)}, true
	default:
		return attributeJSON{}, false
	}
}

// formatNanos renders a timestamp for OTLP/JSON, which carries 64-bit integers
// as strings because JSON numbers are doubles.
func formatNanos(t time.Time) string {
	return strconv.FormatInt(t.UnixNano(), 10)
}
