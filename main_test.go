package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func get(t *testing.T, path string) (*httptest.ResponseRecorder, map[string]string) {
	t.Helper()
	rec := httptest.NewRecorder()
	newMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	var body map[string]string
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: invalid JSON: %v", path, err)
		}
	}
	return rec, body
}

func TestRoot(t *testing.T) {
	t.Setenv("APP_NAME", "demo")
	rec, body := get(t, "/")
	if rec.Code != http.StatusOK || body["app"] != "demo" || body["message"] != "hello" {
		t.Fatalf("got %d %v", rec.Code, body)
	}
}

func TestHealthz(t *testing.T) {
	rec, body := get(t, "/healthz")
	if rec.Code != http.StatusOK || body["status"] != "ok" {
		t.Fatalf("got %d %v", rec.Code, body)
	}
}

func TestVersion(t *testing.T) {
	t.Setenv("APP_NAME", "demo")
	old := version
	version = "abc123"
	defer func() { version = old }()
	rec, body := get(t, "/version")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type %q", ct)
	}
	if len(body) != 2 || body["app"] != "demo" || body["version"] != "abc123" {
		t.Fatalf("got %v", body)
	}
}

func TestEcho(t *testing.T) {
	rec, body := get(t, "/echo?msg=hello")
	if rec.Code != http.StatusOK || body["echo"] != "hello" {
		t.Fatalf("got %d %v", rec.Code, body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type %q", ct)
	}
}

func TestEchoSpecialChars(t *testing.T) {
	const msg = `a b&c=d/é "<x>" +%`
	rec, body := get(t, "/echo?msg="+url.QueryEscape(msg))
	if rec.Code != http.StatusOK || body["echo"] != msg {
		t.Fatalf("got %d %v", rec.Code, body)
	}
}

func TestEchoMissingMsg(t *testing.T) {
	rec, _ := get(t, "/echo")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type %q", ct)
	}
}

func TestUnknownPath(t *testing.T) {
	if rec, _ := get(t, "/nope"); rec.Code != http.StatusNotFound {
		t.Fatalf("got %d", rec.Code)
	}
}
