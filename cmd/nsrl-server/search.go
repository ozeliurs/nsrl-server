package main

import (
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

const maxSearchResults = 100

type searchResponse struct {
	Dataset string           `json:"dataset"`
	Hash    string           `json:"hash"`
	Count   int              `json:"count"`
	Results []map[string]any `json:"results"`
}

// search discovers the archive's schema instead of coupling the API to a
// particular NSRL release. It searches every hash column with the right name.
func (a *app) search(w http.ResponseWriter, r *http.Request) {
	dataset := r.URL.Query().Get("dataset")
	if dataset == "" {
		dataset = "modern"
	}
	if dataset != "modern" && dataset != "legacy" {
		writeSearchError(w, http.StatusBadRequest, "dataset must be modern or legacy")
		return
	}
	hash := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("hash")))
	if !validHash(hash) {
		writeSearchError(w, http.StatusBadRequest, "hash must be a hexadecimal MD5, SHA-1, or SHA-256 value")
		return
	}
	m := a.databases[dataset]
	if m == nil || m.DatabaseFilename == "" {
		writeSearchError(w, http.StatusServiceUnavailable, "search database is not available")
		return
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(a.cfg.DataDir, m.DatabaseFilename)+"?mode=ro&immutable=1")
	if err != nil {
		writeSearchError(w, http.StatusServiceUnavailable, "search database is unavailable")
		return
	}
	defer db.Close()
	results, err := searchDatabase(db, hash)
	if err != nil {
		writeSearchError(w, http.StatusInternalServerError, "database search failed")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(searchResponse{Dataset: dataset, Hash: hash, Count: len(results), Results: results})
}

func validHash(value string) bool {
	if len(value) != 32 && len(value) != 40 && len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func searchDatabase(db *sql.DB, hash string) ([]map[string]any, error) {
	tables, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	var names []string
	for tables.Next() {
		var name string
		if err := tables.Scan(&name); err != nil {
			tables.Close()
			return nil, err
		}
		names = append(names, name)
	}
	if err := tables.Close(); err != nil {
		return nil, err
	}
	var results []map[string]any
	for _, table := range names {
		columns, err := tableColumns(db, table)
		if err != nil {
			return nil, err
		}
		for _, column := range columns {
			normalized := strings.NewReplacer("_", "", "-", "").Replace(strings.ToLower(column))
			if normalized != "md5" && normalized != "sha1" && normalized != "sha256" {
				continue
			}
			rows, err := db.Query(fmt.Sprintf(`SELECT * FROM %s WHERE lower(%s) = ? LIMIT ?`, quoteIdentifier(table), quoteIdentifier(column)), hash, maxSearchResults-len(results))
			if err != nil {
				return nil, err
			}
			found, err := scanRows(rows, table)
			rows.Close()
			if err != nil {
				return nil, err
			}
			results = append(results, found...)
			if len(results) >= maxSearchResults {
				return results, nil
			}
		}
	}
	if results == nil {
		results = []map[string]any{}
	}
	return results, nil
}

func tableColumns(db *sql.DB, table string) ([]string, error) {
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var column string
		if err := rows.Scan(&column); err != nil {
			return nil, err
		}
		columns = append(columns, column)
	}
	return columns, rows.Err()
}

func scanRows(rows *sql.Rows, table string) ([]map[string]any, error) {
	columns, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var result []map[string]any
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			return nil, err
		}
		item := map[string]any{"_table": table}
		for i, value := range values {
			if b, ok := value.([]byte); ok {
				value = string(b)
			}
			item[columns[i]] = value
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func quoteIdentifier(value string) string { return `"` + strings.ReplaceAll(value, `"`, `""`) + `"` }

func writeSearchError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}
