package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// testTelemetry installs in-memory trace and metric pipelines as the global providers for one test.
func testTelemetry(t *testing.T) (*tracetest.InMemoryExporter, *sdkmetric.ManualReader) {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	reader := sdkmetric.NewManualReader()
	prevT, prevM, prevP := otel.GetTracerProvider(), otel.GetMeterProvider(), otel.GetTextMapPropagator()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp)))
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() { otel.SetTracerProvider(prevT); otel.SetMeterProvider(prevM); otel.SetTextMapPropagator(prevP) })
	return exp, reader
}

func spanAttr(s tracetest.SpanStub, key string) attribute.Value {
	for _, a := range s.Attributes {
		if string(a.Key) == key {
			return a.Value
		}
	}
	return attribute.Value{}
}

// histogramCounts returns <attr value> -> request count for one histogram metric.
func histogramCounts(t *testing.T, r *sdkmetric.ManualReader, name, attr string) map[string]uint64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	got := map[string]uint64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				for _, dp := range m.Data.(metricdata.Histogram[float64]).DataPoints {
					v, _ := dp.Attributes.Value(attribute.Key(attr))
					got[v.Emit()] += dp.Count
				}
			}
		}
	}
	return got
}

func TestServerSpansAndSemconvDurationMetric(t *testing.T) {
	exp, reader := testTelemetry(t)
	mux := newMux()
	mux.HandleFunc("GET /fail", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "x"})
	})
	h := serverHandler(mux)
	for _, p := range []string{"/version", "/fail", "/nope", "/healthz"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, p, nil))
	}
	byName := map[string]tracetest.SpanStub{}
	for _, s := range exp.GetSpans() {
		byName[s.Name] = s
		if s.SpanKind != trace.SpanKindServer {
			t.Fatalf("want SERVER spans: %v", s.Name)
		}
	}
	if len(byName) != 3 { // /healthz is not traced
		t.Fatalf("got spans %v", byName)
	}
	ok, fail := byName["GET /version"], byName["GET /fail"]
	if spanAttr(ok, "http.route").AsString() != "/version" || spanAttr(fail, "http.response.status_code").AsInt64() != 500 {
		t.Fatalf("route/status not recorded: %v %v", ok.Attributes, fail.Attributes)
	}
	if fail.Status.Code != codes.Error || ok.Status.Code == codes.Error {
		t.Fatalf("5xx must be an error, 2xx not")
	}
	// New Relic's OpenTelemetry APM view and golden metrics read this metric (http.route = transaction name).
	routes := histogramCounts(t, reader, "http.server.request.duration", "http.route")
	if routes["/version"] != 1 || routes["/fail"] != 1 {
		t.Fatalf("http.server.request.duration by route: %v", routes)
	}
}

func TestOutboundCallsAreClientSpansAndMetrics(t *testing.T) {
	exp, reader := testTelemetry(t)
	var hdr string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hdr = r.Header.Get("traceparent") }))
	defer srv.Close()
	for _, propagate := range []bool{true, false} {
		hdr = ""
		req, _ := http.NewRequestWithContext(context.Background(), "GET", srv.URL, nil)
		resp, err := instrumentedClient(0, propagate).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close() // ends the client span
		if (hdr != "") != propagate {
			t.Fatalf("propagate=%v but traceparent=%q", propagate, hdr)
		}
	}
	if n := len(exp.GetSpans()); n != 2 || exp.GetSpans()[0].SpanKind != trace.SpanKindClient {
		t.Fatalf("want 2 CLIENT spans, got %d", n)
	}
	if len(histogramCounts(t, reader, "http.client.request.duration", "server.address")) == 0 {
		t.Fatal("outbound calls must record http.client.request.duration (external services)")
	}
}

func TestOffWithoutEndpoint(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	setupTelemetry(context.Background())(context.Background())
	if _, ok := otel.GetTracerProvider().(*sdktrace.TracerProvider); ok {
		t.Fatal("no SDK provider must be installed without an endpoint")
	}
}

func TestBrowserSnippetInjectedIntoHead(t *testing.T) {
	f := filepath.Join(t.TempDir(), "snippet.html")
	if err := os.WriteFile(f, []byte(`<script type="text/javascript">window.NREUM={}</script>`), 0o600); err != nil {
		t.Fatal(err)
	}
	prev := browserAgent
	browserAgent = loadBrowserSnippet(f)
	t.Cleanup(func() { browserAgent = prev })
	rec := httptest.NewRecorder()
	servePage(rec, []byte("<!doctype html>\n<html>\n<head>\n<meta charset=\"utf-8\">\n<title>x</title>\n</head>\n</html>"))
	page := rec.Body.String()
	if !strings.Contains(page, "<meta charset=\"utf-8\">\n<script type=\"text/javascript\">window.NREUM") || rec.Header().Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatalf("snippet must open <head>:\n%s", page)
	}
	if loadBrowserSnippet("") != nil || string(withBrowserAgent([]byte("<head>\n"), nil)) != "<head>\n" {
		t.Fatal("no file must mean no snippet")
	}
}
