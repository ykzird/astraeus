package tracing

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ykzird/astraeus/internal/observability"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// collector is a stand-in OTLP/HTTP receiver: it decodes what the exporter
// posted so the assertions are about the wire format, not about our own types.
type collector struct {
	mu       sync.Mutex
	payloads []requestJSON
	status   int
}

func (c *collector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/traces" {
		http.NotFound(w, r)
		return
	}
	var payload requestJSON
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	c.mu.Lock()
	c.payloads = append(c.payloads, payload)
	c.mu.Unlock()
	if c.status != 0 {
		w.WriteHeader(c.status)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (c *collector) captured() []requestJSON {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]requestJSON(nil), c.payloads...)
}

// tracedTracer builds an enabled tracer pointed at a stub collector and returns
// it with the collector. Shutdown runs at cleanup so a test never leaks the
// batcher goroutine.
func tracedTracer(t *testing.T, adjust func(*Config)) (*Tracer, *collector) {
	t.Helper()

	sink := &collector{}
	server := httptest.NewServer(sink)
	t.Cleanup(server.Close)

	cfg := Config{
		ServiceName:   "astraeus-test",
		Version:       "9.9.9",
		Endpoint:      server.URL,
		BatchSize:     1,
		FlushInterval: time.Hour,
		Logger:        discardLogger(),
	}
	if adjust != nil {
		adjust(&cfg)
	}
	tracer, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tracer.Shutdown(ctx)
	})
	return tracer, sink
}

// firstSpan digs the single span out of a captured payload.
func firstSpan(t *testing.T, payloads []requestJSON) spanJSON {
	t.Helper()

	if len(payloads) == 0 {
		t.Fatal("no payload was posted to the collector")
	}
	resource := payloads[0].ResourceSpans
	if len(resource) == 0 || len(resource[0].ScopeSpans) == 0 || len(resource[0].ScopeSpans[0].Spans) == 0 {
		t.Fatalf("payload has no spans: %+v", payloads[0])
	}
	return resource[0].ScopeSpans[0].Spans[0]
}

func TestParseTraceparent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		header string
		ok     bool
		flag   bool
	}{
		{name: "valid sampled", header: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", ok: true, flag: true},
		{name: "valid unsampled", header: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-00", ok: true, flag: false},
		{name: "empty", header: "", ok: false},
		{name: "short trace id", header: "00-abc-00f067aa0ba902b7-01", ok: false},
		{name: "non-hex", header: "00-zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz-00f067aa0ba902b7-01", ok: false},
		{name: "future version", header: "01-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", ok: false},
		{name: "missing flag", header: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7", ok: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			context, ok := parseTraceparent(tt.header)
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v", ok, tt.ok)
			}
			if ok && context.sampled != tt.flag {
				t.Errorf("sampled = %v, want %v", context.sampled, tt.flag)
			}
		})
	}
}

// TestTraceparentRoundTrips covers the property that makes propagation work: a
// header this tracer writes is one it can read back.
func TestTraceparentRoundTrips(t *testing.T) {
	t.Parallel()

	original, ok := parseTraceparent("00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	if !ok {
		t.Fatal("the fixture header did not parse")
	}
	parsed, ok := parseTraceparent(original.traceparent())
	if !ok {
		t.Fatal("the rendered header did not parse")
	}
	if parsed.traceID != original.traceID || parsed.spanID != original.spanID || !parsed.sampled {
		t.Errorf("round trip changed the context: %+v vs %+v", parsed, original)
	}
}

func TestNewRejectsABadEndpoint(t *testing.T) {
	t.Parallel()

	for _, endpoint := range []string{"ftp://collector:4318", "127.0.0.1:4318", "://nope"} {
		if _, err := New(Config{Endpoint: endpoint, Logger: discardLogger()}); err == nil {
			t.Errorf("endpoint %q was accepted", endpoint)
		}
	}
}

// TestDisabledTracerIsANoOp keeps the default honest: without an endpoint the
// server runs, spans exist, and nothing is posted anywhere.
func TestDisabledTracerIsANoOp(t *testing.T) {
	t.Parallel()

	tracer, err := New(Config{Logger: discardLogger()})
	if err != nil {
		t.Fatalf("New with no endpoint: %v", err)
	}
	if tracer.Enabled() {
		t.Fatal("a tracer with no endpoint should be disabled")
	}

	_, span := tracer.Start(context.Background(), "work", SpanKindInternal)
	span.SetAttributes(String("k", "v"))
	span.RecordError(io.EOF)
	span.End()
	if span.TraceID() != "" || span.SpanID() != "" {
		t.Error("a no-op span should not claim an id")
	}
	if err := tracer.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown on a disabled tracer: %v", err)
	}

	called := false
	handler := tracer.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/entities", nil))
	if !called {
		t.Error("a disabled tracer must not swallow the request")
	}
}

// TestExporterPostsOTLPJSON is the wire-format check: the captured payload is
// decoded with the OTLP field names, so a mistake in the encoding shows up here
// rather than in a collector's logs.
func TestExporterPostsOTLPJSON(t *testing.T) {
	t.Parallel()

	tracer, sink := tracedTracer(t, nil)

	_, span := tracer.Start(context.Background(), "probe", SpanKindServer,
		String("http.request.method", "GET"),
		Int("attempt", 42),
		Bool("cached", false),
	)
	span.SetStatus(StatusError, "boom")
	span.End()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := tracer.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	payloads := sink.captured()
	got := firstSpan(t, payloads)

	if got.Name != "probe" || got.Kind != int(SpanKindServer) {
		t.Errorf("span = %+v, want name probe and server kind", got)
	}
	if len(got.TraceID) != 32 || len(got.SpanID) != 16 {
		t.Errorf("ids are not hex of the right width: trace=%q span=%q", got.TraceID, got.SpanID)
	}
	if got.ParentSpanID != "" {
		t.Errorf("a root span should have no parent, got %q", got.ParentSpanID)
	}
	for name, value := range map[string]string{"startTimeUnixNano": got.StartTimeUnixNano, "endTimeUnixNano": got.EndTimeUnixNano} {
		if _, err := strconv.ParseInt(value, 10, 64); err != nil {
			t.Errorf("%s = %q, want a decimal string (OTLP/JSON encodes 64-bit ints as strings)", name, value)
		}
	}
	if got.Status.Code != int(StatusError) || got.Status.Message != "boom" {
		t.Errorf("status = %+v, want error and the message", got.Status)
	}

	attributes := map[string]valueJSON{}
	for _, attr := range got.Attributes {
		attributes[attr.Key] = attr.Value
	}
	if attributes["http.request.method"].StringValue == nil || *attributes["http.request.method"].StringValue != "GET" {
		t.Errorf("string attribute = %+v", attributes["http.request.method"])
	}
	if attributes["attempt"].IntValue == nil || *attributes["attempt"].IntValue != "42" {
		t.Errorf("int attribute = %+v, want the string \"42\"", attributes["attempt"])
	}
	if attributes["cached"].BoolValue == nil || *attributes["cached"].BoolValue {
		t.Errorf("bool attribute = %+v", attributes["cached"])
	}

	// The resource identifies the service, and the scope names the emitter.
	resource := payloads[0].ResourceSpans[0].Resource
	service := ""
	for _, attr := range resource.Attributes {
		if attr.Key == "service.name" && attr.Value.StringValue != nil {
			service = *attr.Value.StringValue
		}
	}
	if service != "astraeus-test" {
		t.Errorf("service.name = %q, want astraeus-test", service)
	}
	if scope := payloads[0].ResourceSpans[0].ScopeSpans[0].Scope; scope.Name != "astraeus-media" {
		t.Errorf("scope = %+v", scope)
	}
}

// TestMiddlewareContinuesAnIncomingTrace covers the propagation half: a trace
// started upstream keeps its id here, and the span points at its parent.
func TestMiddlewareContinuesAnIncomingTrace(t *testing.T) {
	t.Parallel()

	tracer, sink := tracedTracer(t, nil)

	const incoming = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	handler := tracer.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))

	request := httptest.NewRequest(http.MethodGet, "/api/entities/missing", nil)
	request.Header.Set("traceparent", incoming)
	handler.ServeHTTP(httptest.NewRecorder(), request)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := tracer.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	got := firstSpan(t, sink.captured())
	if got.TraceID != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("traceId = %q, want the incoming trace's id", got.TraceID)
	}
	if got.ParentSpanID != "00f067aa0ba902b7" {
		t.Errorf("parentSpanId = %q, want the incoming span", got.ParentSpanID)
	}
	if got.Name != "GET /api/entities/missing" {
		t.Errorf("name = %q", got.Name)
	}

	attributes := map[string]valueJSON{}
	for _, attr := range got.Attributes {
		attributes[attr.Key] = attr.Value
	}
	if attributes["http.response.status_code"].IntValue == nil || *attributes["http.response.status_code"].IntValue != "404" {
		t.Errorf("status attribute = %+v", attributes["http.response.status_code"])
	}
}

// TestMiddlewareMarksServerErrors: a 5xx is the server's failure and should be
// an error span, while a 4xx is the client's and the request still succeeded.
func TestMiddlewareMarksServerErrors(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		status int
		want   StatusCode
	}{
		{status: http.StatusNotFound, want: StatusUnset},
		{status: http.StatusInternalServerError, want: StatusError},
	} {
		tracer, sink := tracedTracer(t, nil)
		handler := tracer.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tt.status)
		}))
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/entities", nil))

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = tracer.Shutdown(ctx)
		cancel()

		if got := firstSpan(t, sink.captured()); got.Status.Code != int(tt.want) {
			t.Errorf("status %d produced span status %d, want %d", tt.status, got.Status.Code, tt.want)
		}
	}
}

// blockingTransport holds a post open so the queue can be filled deterministically.
type blockingTransport struct {
	started chan struct{}
	release chan struct{}
}

func (b *blockingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	select {
	case b.started <- struct{}{}:
	default:
	}
	<-b.release
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("")),
		Header:     http.Header{},
	}, nil
}

// TestAFullQueueDropsRatherThanBlocks is the backpressure contract: when the
// collector is slower than the traffic, a finished request must not be made to
// wait for its span to leave.
func TestAFullQueueDropsRatherThanBlocks(t *testing.T) {
	t.Parallel()

	transport := &blockingTransport{started: make(chan struct{}, 1), release: make(chan struct{})}
	metrics := observability.New()
	tracer, err := New(Config{
		ServiceName:   "astraeus-test",
		Endpoint:      "http://collector.invalid:4318",
		BatchSize:     1,
		FlushInterval: time.Hour,
		QueueSize:     1,
		HTTPClient:    &http.Client{Transport: transport},
		Logger:        discardLogger(),
		Metrics:       metrics,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		close(transport.release)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tracer.Shutdown(ctx)
	}()

	// The first span is taken by the batcher, which then blocks in the post.
	_, first := tracer.Start(context.Background(), "first", SpanKindInternal)
	first.End()
	select {
	case <-transport.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the exporter never attempted a post")
	}

	// The queue holds one; everything after that is dropped.
	for i := 0; i < 5; i++ {
		_, span := tracer.Start(context.Background(), "later", SpanKindInternal)
		span.End()
	}

	if rendered := metrics.Render(); !strings.Contains(rendered, observability.MetricSpansDropped+" 4") {
		t.Errorf("expected four dropped spans in:\n%s", rendered)
	}
}

// TestUnsampledParentIsNotExported: honouring the parent's decision is what keeps
// a sampler upstream from being overridden here.
func TestUnsampledParentIsNotExported(t *testing.T) {
	t.Parallel()

	tracer, sink := tracedTracer(t, nil)

	handler := tracer.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	request := httptest.NewRequest(http.MethodGet, "/api/entities", nil)
	request.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-00")
	handler.ServeHTTP(httptest.NewRecorder(), request)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = tracer.Shutdown(ctx)

	if len(sink.captured()) != 0 {
		t.Errorf("an unsampled parent still produced %d payload(s)", len(sink.captured()))
	}
}
