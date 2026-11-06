package httpapi

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/legacy"
	"github.com/go-chi/chi/v5"

	"enquoir/api"
)

var (
	specOnce   sync.Once
	specDoc    *openapi3.T
	specRouter routers.Router
	specErr    error
)

// loadSpec parses and validates api/openapi.yaml once per test binary.
func loadSpec(t *testing.T) (*openapi3.T, routers.Router) {
	t.Helper()
	specOnce.Do(func() {
		openapi3filter.RegisterBodyDecoder("text/html", openapi3filter.PlainBodyDecoder)
		loader := openapi3.NewLoader()
		specDoc, specErr = loader.LoadFromData(api.Spec)
		if specErr != nil {
			return
		}
		if specErr = specDoc.Validate(context.Background()); specErr != nil {
			return
		}
		// Match requests by path alone; the test server's host is not
		// one of the spec's servers.
		specDoc.Servers = nil
		specRouter, specErr = legacy.NewRouter(specDoc)
	})
	if specErr != nil {
		t.Fatalf("api/openapi.yaml: %v", specErr)
	}
	return specDoc, specRouter
}

func TestSpec_IsValidOpenAPI31(t *testing.T) {
	doc, _ := loadSpec(t)
	if !doc.IsOpenAPI31OrLater() {
		t.Errorf("openapi = %q, want 3.1.x", doc.OpenAPI)
	}
}

// The spec is the contract: every route the router serves is
// documented, and every documented operation is served.
func TestSpec_DocumentsEveryRouteAndNothingElse(t *testing.T) {
	doc, _ := loadSpec(t)

	documented := map[string]bool{}
	for path, item := range doc.Paths.Map() {
		for method := range item.Operations() {
			documented[method+" "+path] = true
		}
	}

	served := map[string]bool{}
	err := chi.Walk(NewRouter(&Handler{}), func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		served[method+" "+strings.TrimSuffix(route, "/*")] = true
		return nil
	})
	if err != nil {
		t.Fatalf("walking routes: %v", err)
	}

	var missing, extra []string
	for r := range served {
		if !documented[r] {
			missing = append(missing, r)
		}
	}
	for r := range documented {
		if !served[r] {
			extra = append(extra, r)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	for _, r := range missing {
		t.Errorf("served but not in api/openapi.yaml: %s", r)
	}
	for _, r := range extra {
		t.Errorf("in api/openapi.yaml but not served: %s", r)
	}
}

// contract wraps h so every response it produces in a test is checked
// against api/openapi.yaml: status code, content type and body schema.
// A handler test that passes therefore also proves the spec is true.
func contract(t *testing.T, h http.Handler) http.Handler {
	t.Helper()
	_, router := loadSpec(t)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)

		checkResponse(t, router, r, body, rec)

		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(rec.Code)
		w.Write(rec.Body.Bytes())
	})
}

func checkResponse(t *testing.T, router routers.Router, r *http.Request, reqBody []byte, rec *httptest.ResponseRecorder) {
	t.Helper()
	probe := r.Clone(context.Background())
	probe.Body = io.NopCloser(bytes.NewReader(reqBody))
	route, params, err := router.FindRoute(probe)
	if err != nil {
		t.Errorf("contract: %s %s is not in api/openapi.yaml: %v", r.Method, r.URL.Path, err)
		return
	}
	input := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: &openapi3filter.RequestValidationInput{
			Request:    probe,
			PathParams: params,
			Route:      route,
			Options:    &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
		},
		Status:  rec.Code,
		Header:  rec.Header(),
		Options: &openapi3filter.Options{IncludeResponseStatus: true},
	}
	input.SetBodyBytes(rec.Body.Bytes())
	if err := openapi3filter.ValidateResponse(context.Background(), input); err != nil {
		t.Errorf("contract: %s %s -> %d does not match api/openapi.yaml: %v\nbody: %s", r.Method, r.URL.Path, rec.Code, err, rec.Body.String())
	}
}

func TestServesTheSpec(t *testing.T) {
	router := contract(t, NewRouter(&Handler{}))

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/openapi.yaml", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /openapi.yaml: status %d", rec.Code)
	}
	if !bytes.Equal(rec.Body.Bytes(), api.Spec) {
		t.Error("GET /openapi.yaml did not return api/openapi.yaml")
	}

	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/docs", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "/openapi.yaml") {
		t.Errorf("GET /docs: status %d, want an HTML page that loads /openapi.yaml", rec.Code)
	}
}
