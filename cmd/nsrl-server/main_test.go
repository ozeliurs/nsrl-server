package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestDocumentationEndpoints(t *testing.T) {
	a := app{databases: make(map[string]*metadata)}
	for _, tc := range []struct{ path, contentType string }{{"/docs", "text/html"}, {"/openapi.json", "application/json"}} {
		w := httptest.NewRecorder()
		a.routes().ServeHTTP(w, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), tc.contentType) {
			t.Errorf("GET %s = %d, %q", tc.path, w.Code, w.Header().Get("Content-Type"))
		}
		if tc.path == "/openapi.json" {
			var document map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &document); err != nil {
				t.Fatalf("invalid OpenAPI JSON: %v", err)
			}
			if document["openapi"] != "3.1.0" {
				t.Errorf("OpenAPI version = %v", document["openapi"])
			}
		}
	}
}

func TestReadinessRequiresDatabase(t *testing.T) {
	a := app{cfg: config{DataDir: t.TempDir()}, databases: make(map[string]*metadata)}
	w := httptest.NewRecorder()
	a.ready(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("readiness without database = %d, want 503", w.Code)
	}

	if err := os.WriteFile(a.cfg.DataDir+"/database.zip", []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	a.databases["modern"] = &metadata{Filename: "database.zip"}
	a.databases["legacy"] = &metadata{Filename: "database.zip"}
	w = httptest.NewRecorder()
	a.ready(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("readiness with database = %d, want 200", w.Code)
	}
}
