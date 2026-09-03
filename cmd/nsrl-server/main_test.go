package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
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

func TestSearchByHash(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", dir+"/modern.db")
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE files (sha_256 TEXT, file_name TEXT, size INTEGER); INSERT INTO files VALUES ('aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', 'example.exe', 42)`)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	a := app{cfg: config{DataDir: dir}, databases: map[string]*metadata{"modern": {DatabaseFilename: "modern.db"}}}
	w := httptest.NewRecorder()
	a.routes().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/search?hash=AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("search status = %d: %s", w.Code, w.Body.String())
	}
	var response searchResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Count != 1 || response.Results[0]["file_name"] != "example.exe" || response.Results[0]["_table"] != "files" {
		t.Fatalf("unexpected response: %+v", response)
	}
}

func TestSearchValidation(t *testing.T) {
	a := app{databases: make(map[string]*metadata)}
	for _, path := range []string{"/v1/search?hash=nope", "/v1/search?hash=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa&dataset=other"} {
		w := httptest.NewRecorder()
		a.routes().ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusBadRequest {
			t.Errorf("GET %s = %d, want 400", path, w.Code)
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
	if err := os.WriteFile(a.cfg.DataDir+"/database.db", []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	a.databases["modern"] = &metadata{Filename: "database.zip", DatabaseFilename: "database.db"}
	a.databases["legacy"] = &metadata{Filename: "database.zip", DatabaseFilename: "database.db"}
	w = httptest.NewRecorder()
	a.ready(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("readiness with database = %d, want 200", w.Code)
	}
}
