package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestLatestUsesFixedSources(t *testing.T) {
	a := app{}
	for dataset, want := range map[string]string{"modern": defaultModernSourceURL, "legacy": defaultLegacySourceURL} {
		got, _, _, err := a.latest(context.Background(), dataset)
		if err != nil || got != want {
			t.Errorf("latest(%q) = %q, %v; want %q", dataset, got, err, want)
		}
	}
	a.cfg.SourceURL = "https://mirror.test/modern.zip"
	got, _, _, err := a.latest(context.Background(), "modern")
	if err != nil || got != a.cfg.SourceURL {
		t.Errorf("latest() override = %q, %v", got, err)
	}
}

func TestDefaultSourcesUseCurrentRelease(t *testing.T) {
	if defaultModernSourceURL != "https://s3.amazonaws.com/rds.nsrl.nist.gov/RDS/rds_2026.03.1/RDS_2026.03.1_modern.zip" {
		t.Errorf("default modern source = %q", defaultModernSourceURL)
	}
	if defaultLegacySourceURL != "https://s3.amazonaws.com/rds.nsrl.nist.gov/RDS/rds_2026.03.1/RDS_2026.03.1_legacy.zip" {
		t.Errorf("default legacy source = %q", defaultLegacySourceURL)
	}
}

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

func TestFetchAndServe(t *testing.T) {
	archive := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("zip data")) }))
	defer archive.Close()
	a := app{cfg: config{DataDir: t.TempDir()}, client: archive.Client(), databases: make(map[string]*metadata)}
	if err := a.fetch(context.Background(), "modern", archive.URL+"/RDS-modern.zip", "tag", time.Now()); err != nil {
		t.Fatal(err)
	}
	// Installing identical content again must not delete the content-addressed
	// archive when cleaning up the previous version.
	if err := a.fetch(context.Background(), "modern", archive.URL+"/RDS-modern.zip", "new-tag", time.Now()); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "/v1/nsrl", nil)
	w := httptest.NewRecorder()
	a.download("modern")(w, r)
	if w.Code != http.StatusOK || w.Body.String() != "zip data" {
		t.Fatalf("download status/body = %d/%q", w.Code, w.Body.String())
	}
	if _, err := os.Stat(a.cfg.DataDir + "/modern-metadata.json"); err != nil {
		t.Fatal(err)
	}
	if got := w.Header().Get("Content-Disposition"); got != `attachment; filename="RDS-modern.zip"` {
		t.Fatalf("Content-Disposition = %q", got)
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
