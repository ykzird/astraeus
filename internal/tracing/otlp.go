// The OTLP/HTTP JSON encoder and batcher. See the package comment in tracing.go
// for why this is hand-rolled rather than the OpenTelemetry SDK.
package tracing

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jok/astraeus-media/internal/observability"
)

// exporter batches finished spans and posts them to an OTLP/HTTP endpoint.
type exporter struct {
	endpoint   string
	resource   resourceJSON
	scope      scopeJSON
	httpClient *http.Client
	logger     *slog.Logger
	metrics    *observability.Metrics

	batchSize int
	interval  time.Duration

	queue chan spanJSON
	// stop asks the batcher to drain and exit; done closes when it has. The
	// queue is deliberately never closed, so a span that ends during shutdown
	// cannot panic by sending on a closed channel.
	stop chan struct{}
	done chan struct{}
	once sync.Once

	// dropped counts spans discarded because the queue was full, which is the
	// only backpressure signal a tracer can offer without slowing the request
	// that produced the span.
	mu      sync.Mutex
	dropped int
}

func newExporter(cfg Config) *exporter {
	endpoint := strings.TrimRight(cfg.Endpoint, "/")
	// A base URL is the configuration, but the signal path is part of the
	// protocol, so accepting either spelling is friendlier than making the
	// operator discover /v1/traces from a 404.
	if !strings.HasSuffix(endpoint, "/v1/traces") {
		endpoint += "/v1/traces"
	}

	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}

	attributes := []attributeJSON{{Key: "service.name", Value: valueJSON{StringValue: ptr(cfg.ServiceName)}}}
	if cfg.Version != "" {
		attributes = append(attributes, attributeJSON{Key: "service.version", Value: valueJSON{StringValue: ptr(cfg.Version)}})
	}

	exp := &exporter{
		endpoint:   endpoint,
		resource:   resourceJSON{Attributes: attributes},
		scope:      scopeJSON{Name: "astraeus-media", Version: cfg.Version},
		httpClient: client,
		logger:     cfg.Logger,
		metrics:    cfg.Metrics,
		batchSize:  cfg.BatchSize,
		interval:   cfg.FlushInterval,
		queue:      make(chan spanJSON, cfg.QueueSize),
		stop:       make(chan struct{}),
		done:       make(chan struct{}),
	}
	go exp.run()
	return exp
}

// run drains the queue, posting a batch when it is full or the interval passes.
func (e *exporter) run() {
	ticker := time.NewTicker(e.interval)
	defer ticker.Stop()

	batch := make([]spanJSON, 0, e.batchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		e.post(batch)
		batch = batch[:0]
	}

	for {
		select {
		case span := <-e.queue:
			batch = append(batch, span)
			if len(batch) >= e.batchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-e.stop:
			// Drain what is already buffered, then post it. Anything that
			// arrives after this is dropped, which is the honest end of a
			// process: better to lose the last span than to hang the exit.
		drain:
			for {
				select {
				case span := <-e.queue:
					batch = append(batch, span)
					if len(batch) >= e.batchSize {
						flush()
					}
				default:
					break drain
				}
			}
			flush()
			close(e.done)
			return
		}
	}
}

// enqueue accepts a span without blocking the caller. A full queue means the
// exporter is slower than the traffic, and dropping is better than adding
// latency to a request that has already finished.
func (e *exporter) enqueue(span spanJSON) {
	select {
	case e.queue <- span:
	case <-e.stop:
	default:
		e.mu.Lock()
		e.dropped++
		e.mu.Unlock()
		e.metrics.IncCounter(observability.MetricSpansDropped,
			"Spans dropped because the export queue was full.", nil)
	}
}

// shutdown stops the batcher after flushing what it holds.
func (e *exporter) shutdown(ctx context.Context) {
	e.once.Do(func() { close(e.stop) })
	select {
	case <-e.done:
	case <-ctx.Done():
	}
}

// post sends one batch, best effort. A failure is logged and the batch is
// dropped rather than retried: a tracing backend that is down must not become a
// queue that grows until the server dies.
func (e *exporter) post(spans []spanJSON) {
	payload := requestJSON{ResourceSpans: []resourceSpansJSON{{
		Resource:   e.resource,
		ScopeSpans: []scopeSpansJSON{{Scope: e.scope, Spans: spans}},
	}}}

	body, err := json.Marshal(payload)
	if err != nil {
		e.logger.Warn("encoding a trace batch", "error", err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint, bytes.NewReader(body))
	if err != nil {
		e.logger.Warn("building a trace request", "error", err)
		return
	}
	request.Header.Set("Content-Type", "application/json")

	response, err := e.httpClient.Do(request)
	if err != nil {
		e.logger.Warn("exporting traces", "endpoint", e.endpoint, "error", err, "spans", len(spans))
		return
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 {
		e.logger.Warn("exporting traces", "endpoint", e.endpoint, "status", response.StatusCode, "spans", len(spans))
	}
}

/* ---- the OTLP JSON shapes ---- */

// The field names and the string-encoded 64-bit integers are the OTLP/JSON
// encoding, not a local convention: JSON numbers are doubles, so timestamps and
// integer attributes are strings in the standard.

type requestJSON struct {
	ResourceSpans []resourceSpansJSON `json:"resourceSpans"`
}

type resourceSpansJSON struct {
	Resource   resourceJSON     `json:"resource"`
	ScopeSpans []scopeSpansJSON `json:"scopeSpans"`
}

type resourceJSON struct {
	Attributes []attributeJSON `json:"attributes,omitempty"`
}

type scopeSpansJSON struct {
	Scope scopeJSON  `json:"scope"`
	Spans []spanJSON `json:"spans"`
}

type scopeJSON struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

type spanJSON struct {
	TraceID           string          `json:"traceId"`
	SpanID            string          `json:"spanId"`
	ParentSpanID      string          `json:"parentSpanId,omitempty"`
	Name              string          `json:"name"`
	Kind              int             `json:"kind"`
	StartTimeUnixNano string          `json:"startTimeUnixNano"`
	EndTimeUnixNano   string          `json:"endTimeUnixNano"`
	Attributes        []attributeJSON `json:"attributes,omitempty"`
	Status            statusJSON      `json:"status"`
}

type statusJSON struct {
	Code    int    `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

type attributeJSON struct {
	Key   string    `json:"key"`
	Value valueJSON `json:"value"`
}

// valueJSON is the OTLP AnyValue oneof. Only the scalar members are emitted:
// nothing here records an array or a nested structure.
type valueJSON struct {
	StringValue *string  `json:"stringValue,omitempty"`
	IntValue    *string  `json:"intValue,omitempty"`
	DoubleValue *float64 `json:"doubleValue,omitempty"`
	BoolValue   *bool    `json:"boolValue,omitempty"`
}

func stringValue(value string) valueJSON { return valueJSON{StringValue: ptr(value)} }

func intValue(value int64) valueJSON { return valueJSON{IntValue: ptr(strconv.FormatInt(value, 10))} }

func doubleValue(value float64) valueJSON { return valueJSON{DoubleValue: ptr(value)} }

func boolValue(value bool) valueJSON { return valueJSON{BoolValue: ptr(value)} }

func ptr[T any](value T) *T { return &value }

// spanContext is what travels between spans and across a process boundary.
type spanContext struct {
	traceID [16]byte
	spanID  [8]byte
	sampled bool
}

// traceparent renders the W3C trace context header.
func (c spanContext) traceparent() string {
	flags := "00"
	if c.sampled {
		flags = "01"
	}
	return fmt.Sprintf("00-%s-%s-%s", hex.EncodeToString(c.traceID[:]), hex.EncodeToString(c.spanID[:]), flags)
}

// parseTraceparent reads a W3C trace context header, accepting only version 00
// (which is the version the format specifies today; a higher version has extra
// fields we would be guessing at).
func parseTraceparent(header string) (spanContext, bool) {
	parts := strings.Split(strings.TrimSpace(header), "-")
	if len(parts) != 4 || parts[0] != "00" {
		return spanContext{}, false
	}
	traceID, err := hex.DecodeString(parts[1])
	if err != nil || len(traceID) != 16 {
		return spanContext{}, false
	}
	spanID, err := hex.DecodeString(parts[2])
	if err != nil || len(spanID) != 8 {
		return spanContext{}, false
	}
	flags, err := hex.DecodeString(parts[3])
	if err != nil || len(flags) != 1 {
		return spanContext{}, false
	}

	var context spanContext
	copy(context.traceID[:], traceID)
	copy(context.spanID[:], spanID)
	context.sampled = flags[0]&0x01 == 0x01
	return context, true
}

// validateEndpoint rejects a configured endpoint the exporter could never post
// to, at startup rather than on the first span.
func validateEndpoint(endpoint string) error {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("tracing: invalid endpoint %q: %w", endpoint, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("tracing: endpoint %q must be http or https", endpoint)
	}
	if parsed.Host == "" {
		return fmt.Errorf("tracing: endpoint %q has no host", endpoint)
	}
	return nil
}
