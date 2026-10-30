package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type fakeCollector struct {
	*httptest.Server
	mu     sync.Mutex
	paths  []string
	ctypes []string
	bodies []string
}

func newFakeCollector() *fakeCollector {
	c := &fakeCollector{}
	c.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.paths = append(c.paths, r.URL.Path)
		c.ctypes = append(c.ctypes, r.Header.Get("Content-Type"))
		c.bodies = append(c.bodies, string(b))
		c.mu.Unlock()
	}))
	return c
}

func serve(h http.Handler, method, path string) {
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(method, path, nil))
}

func tracedMux(t *tracer) http.Handler {
	mux := newMux()
	mux.HandleFunc("GET /boom", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "boom"})
	})
	return t.middleware(mux)
}

func TestSpanExported(t *testing.T) {
	c := newFakeCollector()
	defer c.Close()
	tr := newTracer(c.URL, "svc", "v1")
	h := tracedMux(tr)
	serve(h, "GET", "/version")
	serve(h, "GET", "/nope")
	tr.flush()

	if len(c.bodies) != 1 || c.paths[0] != "/v1/traces" || c.ctypes[0] != "application/json" {
		t.Fatalf("unexpected export: %v %v", c.paths, c.ctypes)
	}
	var p struct {
		ResourceSpans []struct {
			Resource struct {
				Attributes []otlpAttr `json:"attributes"`
			} `json:"resource"`
			ScopeSpans []struct {
				Spans []otlpSpan `json:"spans"`
			} `json:"scopeSpans"`
		} `json:"resourceSpans"`
	}
	if err := json.Unmarshal([]byte(c.bodies[0]), &p); err != nil {
		t.Fatal(err)
	}
	res := p.ResourceSpans[0].Resource.Attributes
	if res[0].Value["stringValue"] != "svc" || res[1].Value["stringValue"] != "v1" {
		t.Errorf("bad resource: %+v", res)
	}
	spans := p.ResourceSpans[0].ScopeSpans[0].Spans
	if len(spans) != 2 {
		t.Fatalf("want 2 spans, got %d", len(spans))
	}
	s := spans[0]
	if s.Name != "GET /version" || s.Kind != 2 || len(s.TraceID) != 32 || len(s.SpanID) != 16 || s.Status.Code != 0 {
		t.Errorf("bad span: %+v", s)
	}
	a := map[string]map[string]string{}
	for _, at := range s.Attributes {
		a[at.Key] = at.Value
	}
	if a["http.request.method"]["stringValue"] != "GET" || a["http.route"]["stringValue"] != "GET /version" ||
		a["url.path"]["stringValue"] != "/version" || a["http.response.status_code"]["intValue"] != "200" {
		t.Errorf("bad attrs: %+v", a)
	}
	if spans[1].Name != "GET (unmatched)" {
		t.Errorf("404 name = %q", spans[1].Name)
	}
	if !strings.Contains(c.bodies[0], `"startTimeUnixNano":"`) {
		t.Errorf("int64 times must be strings")
	}
}

func TestServerErrorMarked(t *testing.T) {
	c := newFakeCollector()
	defer c.Close()
	tr := newTracer(c.URL, "svc", "v1")
	serve(tracedMux(tr), "GET", "/boom")
	tr.flush()
	if len(c.bodies) != 1 || !strings.Contains(c.bodies[0], `"status":{"code":2}`) {
		t.Fatalf("expected error status, got %v", c.bodies)
	}
}

func TestHealthzNotTraced(t *testing.T) {
	c := newFakeCollector()
	defer c.Close()
	tr := newTracer(c.URL, "svc", "v1")
	serve(tracedMux(tr), "GET", "/healthz")
	tr.flush()
	if len(c.bodies) != 0 {
		t.Fatalf("healthz was exported: %v", c.bodies)
	}
}

func TestTracingOffWithoutEndpoint(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	tr := newTracerFromEnv()
	if tr != nil {
		t.Fatal("tracer should be nil without endpoint")
	}
	rec := httptest.NewRecorder()
	tr.middleware(newMux()).ServeHTTP(rec, httptest.NewRequest("GET", "/version", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestFullQueueDropsWithoutBlocking(t *testing.T) {
	tr := newTracer("http://127.0.0.1:1", "svc", "v1")
	for i := 0; i < spanQueueSize+10; i++ {
		tr.record(otlpSpan{})
	}
	if len(tr.spans) != spanQueueSize {
		t.Fatalf("queue len = %d", len(tr.spans))
	}
	tr.flush() // unreachable collector must not panic or hang
}
