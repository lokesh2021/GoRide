package observability

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/newrelic/go-agent/v3/newrelic"

	"github.com/lokeshbm/goride/internal/config"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestNewEmptyLicenseReturnsNil covers the documented "monitoring disabled"
// path: no license configured, no infra/network involved, nil app returned.
func TestNewEmptyLicenseReturnsNil(t *testing.T) {
	cfg := config.Config{NewRelicLicense: "", NewRelicAppName: "goride-test"}
	app := New(cfg, discardLogger())
	if app != nil {
		t.Fatalf("New with empty license = %v, want nil", app)
	}
}

// TestNewMalformedLicenseReturnsNil covers the "agent init failed" path: the
// go-agent library validates license shape (must be exactly 40 chars)
// synchronously in NewApplication, before any goroutine or network I/O is
// started, so this is safe to exercise without live infra.
func TestNewMalformedLicenseReturnsNil(t *testing.T) {
	cfg := config.Config{NewRelicLicense: "too-short-to-be-valid", NewRelicAppName: "goride-test"}
	app := New(cfg, discardLogger())
	if app != nil {
		t.Fatalf("New with malformed license = %v, want nil", app)
	}
}

// TestMiddlewareNilAppPassesThrough covers Middleware(nil): every request
// must reach the wrapped handler unchanged, with no New Relic involvement.
func TestMiddlewareNilAppPassesThrough(t *testing.T) {
	var gotMethod, gotPath, gotBody string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("passthrough-ok"))
	})

	handler := Middleware(nil)(next)

	req := httptest.NewRequest(http.MethodPost, "/v1/rides", strings.NewReader("payload"))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if gotMethod != http.MethodPost {
		t.Errorf("wrapped handler saw method %q, want POST", gotMethod)
	}
	if gotPath != "/v1/rides" {
		t.Errorf("wrapped handler saw path %q, want /v1/rides", gotPath)
	}
	if gotBody != "payload" {
		t.Errorf("wrapped handler saw body %q, want %q", gotBody, "payload")
	}
	if rec.Code != http.StatusTeapot {
		t.Errorf("response status = %d, want %d", rec.Code, http.StatusTeapot)
	}
	if rec.Body.String() != "passthrough-ok" {
		t.Errorf("response body = %q, want %q", rec.Body.String(), "passthrough-ok")
	}
}

// TestMiddlewareNilAppPassesThroughSSERoute covers the SSE-prefix branch of
// the guard condition too (both operands of the || short-circuit through the
// same passthrough line when app is nil, but this documents intent for the
// SSE path specifically).
func TestMiddlewareNilAppPassesThroughSSERoute(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	handler := Middleware(nil)(next)
	req := httptest.NewRequest(http.MethodGet, "/v1/events?ride_id=abc", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !called {
		t.Fatal("wrapped handler was not called for SSE route with nil app")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("response status = %d, want 200", rec.Code)
	}
}

// disabledApp builds a real, non-nil *newrelic.Application with
// ConfigEnabled(false): NewApplication returns immediately with no error, and
// per go-agent's own newApp() the connect/process/runtime-sampler goroutines
// are only started "if app.config.Enabled" -- so a disabled app never dials
// the collector or does any network I/O, while StartTransaction/SetName/End
// etc. all still run their real (in-process) logic against it. This lets the
// non-nil-app branch of Middleware -- including the post-ServeHTTP route
// pattern rename -- be exercised with zero network dependency.
func disabledApp(t *testing.T) *newrelic.Application {
	t.Helper()
	app, err := newrelic.NewApplication(
		newrelic.ConfigAppName("goride-test"),
		newrelic.ConfigEnabled(false),
	)
	if err != nil {
		t.Fatalf("newrelic.NewApplication (disabled): %v", err)
	}
	if app == nil {
		t.Fatal("newrelic.NewApplication (disabled) returned nil app")
	}
	return app
}

// TestMiddlewareNonNilAppRoutedRequest drives a real (but disabled/inert) app
// through a chi router so RoutePattern() resolves post-routing, covering the
// StartTransaction -> SetWebRequestHTTP -> SetWebResponse -> ServeHTTP ->
// route-pattern rename -> End lifecycle end to end.
func TestMiddlewareNonNilAppRoutedRequest(t *testing.T) {
	app := disabledApp(t)

	var sawPattern string
	r := chi.NewRouter()
	r.Use(Middleware(app))
	r.Get("/v1/rides/{id}", func(w http.ResponseWriter, req *http.Request) {
		sawPattern = chi.RouteContext(req.Context()).RoutePattern()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/rides/abc-123", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("response status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != "ok" {
		t.Errorf("response body = %q, want %q", rec.Body.String(), "ok")
	}
	if sawPattern != "/v1/rides/{id}" {
		t.Errorf("handler saw route pattern %q, want /v1/rides/{id}", sawPattern)
	}
}

// TestMiddlewareNonNilAppErrorStatus covers the branch with a handler that
// writes a non-2xx status, still through the full non-nil-app lifecycle.
func TestMiddlewareNonNilAppErrorStatus(t *testing.T) {
	app := disabledApp(t)

	r := chi.NewRouter()
	r.Use(Middleware(app))
	r.Get("/v1/broken", func(w http.ResponseWriter, req *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/broken", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("response status = %d, want 500", rec.Code)
	}
}

// TestMiddlewareNonNilAppSSESkipsTransaction covers the SSE-prefix skip when
// app is non-nil: the guard must bypass StartTransaction entirely for
// /v1/events routes even though monitoring is otherwise "on".
func TestMiddlewareNonNilAppSSESkipsTransaction(t *testing.T) {
	app := disabledApp(t)

	called := false
	r := chi.NewRouter()
	r.Use(Middleware(app))
	r.Get("/v1/events", func(w http.ResponseWriter, req *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/events?ride_id=abc", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if !called {
		t.Fatal("handler was not called for SSE route")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("response status = %d, want 200", rec.Code)
	}
}

// TestMiddlewareNonNilAppUnmatchedRoute covers the request path where chi
// never resolves a RoutePattern (no route matches -> 404), so Middleware's
// "if pattern != """ guard takes its false branch and SetName is skipped.
func TestMiddlewareNonNilAppUnmatchedRoute(t *testing.T) {
	app := disabledApp(t)

	r := chi.NewRouter()
	r.Use(Middleware(app))
	r.Get("/v1/known", func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/does-not-exist", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("response status = %d, want 404", rec.Code)
	}
}
