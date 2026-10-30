package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	spanQueueSize  = 512
	exportInterval = 5 * time.Second
	exportTimeout  = 3 * time.Second
	maxBatch       = 256
)

type otlpAttr struct {
	Key   string            `json:"key"`
	Value map[string]string `json:"value"`
}

type otlpSpan struct {
	TraceID           string     `json:"traceId"`
	SpanID            string     `json:"spanId"`
	Name              string     `json:"name"`
	Kind              int        `json:"kind"`
	StartTimeUnixNano string     `json:"startTimeUnixNano"`
	EndTimeUnixNano   string     `json:"endTimeUnixNano"`
	Attributes        []otlpAttr `json:"attributes"`
	Status            otlpStatus `json:"status"`
}

type otlpStatus struct {
	Code int `json:"code"`
}

func strAttr(k, v string) otlpAttr { return otlpAttr{k, map[string]string{"stringValue": v}} }
func intAttr(k string, v int64) otlpAttr {
	return otlpAttr{k, map[string]string{"intValue": strconv.FormatInt(v, 10)}}
}

// tracer batches spans and exports them as OTLP/HTTP JSON. A nil *tracer is off.
type tracer struct {
	url     string
	service string
	version string
	spans   chan otlpSpan
	client  *http.Client
}

// newTracerFromEnv returns nil (tracing off) when no endpoint is configured.
func newTracerFromEnv() *tracer {
	ep := strings.TrimRight(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"), "/")
	if ep == "" {
		return nil
	}
	svc := os.Getenv("OTEL_SERVICE_NAME")
	if svc == "" {
		svc = appName()
	}
	return newTracer(ep, svc, version)
}

func newTracer(endpoint, service, ver string) *tracer {
	return &tracer{
		url:     strings.TrimRight(endpoint, "/") + "/v1/traces",
		service: service,
		version: ver,
		spans:   make(chan otlpSpan, spanQueueSize),
		client:  &http.Client{Timeout: exportTimeout},
	}
}

// run flushes the queue every interval until stop is closed, then flushes once more.
func (t *tracer) run(interval time.Duration, stop <-chan struct{}) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			t.flush()
		case <-stop:
			t.flush()
			return
		}
	}
}

func (t *tracer) flush() {
	for {
		var batch []otlpSpan
	drain:
		for len(batch) < maxBatch {
			select {
			case s := <-t.spans:
				batch = append(batch, s)
			default:
				break drain
			}
		}
		if len(batch) == 0 {
			return
		}
		t.export(batch)
	}
}

func (t *tracer) export(batch []otlpSpan) {
	payload := map[string]any{"resourceSpans": []any{map[string]any{
		"resource": map[string]any{"attributes": []otlpAttr{
			strAttr("service.name", t.service),
			strAttr("service.version", t.version),
		}},
		"scopeSpans": []any{map[string]any{
			"scope": map[string]string{"name": "app/telemetry"},
			"spans": batch,
		}},
	}}}
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	req, err := http.NewRequest(http.MethodPost, t.url, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.client.Do(req)
	if err != nil {
		return
	}
	resp.Body.Close()
}

// record enqueues a span without blocking; it is dropped if the queue is full.
func (t *tracer) record(s otlpSpan) {
	select {
	case t.spans <- s:
	default:
	}
}

func randHex(nBytes int) string {
	b := make([]byte, nBytes)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

// middleware records one SERVER span per request, skipping /healthz.
// With a nil tracer it returns next unchanged.
func (t *tracer) middleware(next http.Handler) http.Handler {
	if t == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		end := time.Now()
		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}
		// The mux sets r.Pattern on the request it receives.
		name, route := r.Pattern, r.Pattern
		if route == "" {
			name = r.Method + " (unmatched)"
		}
		code := 0
		if status >= 500 {
			code = 2
		}
		t.record(otlpSpan{
			TraceID:           randHex(16),
			SpanID:            randHex(8),
			Name:              name,
			Kind:              2,
			StartTimeUnixNano: strconv.FormatInt(start.UnixNano(), 10),
			EndTimeUnixNano:   strconv.FormatInt(end.UnixNano(), 10),
			Attributes: []otlpAttr{
				strAttr("http.request.method", r.Method),
				strAttr("http.route", route),
				strAttr("url.path", r.URL.Path),
				intAttr("http.response.status_code", int64(status)),
			},
			Status: otlpStatus{Code: code},
		})
	})
}
