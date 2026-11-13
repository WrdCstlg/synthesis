package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type fakeKVServer struct {
	mu     sync.Mutex
	node   string
	writes map[string]int64
}

func startFakeKVServer(t *testing.T, node string) *httptest.Server {
	t.Helper()
	f := &fakeKVServer{node: node, writes: map[string]int64{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/status":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"node": f.node, "role": "leader", "term": 1, "commit_index": 1,
			})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/kv/"):
			key := strings.TrimPrefix(r.URL.Path, "/kv/")
			f.mu.Lock()
			v, ok := f.writes[key]
			f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true, "found": ok, "value": v, "served_by": f.node,
				"term": 1, "commit_index": 1, "read_mode": r.URL.Query().Get("consistency"),
			})
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/kv/"):
			key := strings.TrimPrefix(r.URL.Path, "/kv/")
			var body struct {
				Value int64 `json:"value"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.mu.Lock()
			f.writes[key] = body.Value
			f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true, "applied": true, "served_by": f.node, "term": 1, "index": 1,
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true, "applied": true, "served_by": f.node,
			})
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestProfileRunWritesPairedHistoryRecords(t *testing.T) {
	srv := startFakeKVServer(t, "kv-n1")
	hist := filepath.Join(t.TempDir(), "history.jsonl")

	code, err := run([]string{
		"--history", hist,
		"--seed", "42",
		"--profile", "linear",
		"--targets", srv.URL,
		"--clients", "2",
		"--ops", "12",
		"--think-max-ms", "0",
		"--quiet",
	})
	if err != nil || code != exitOK {
		t.Fatalf("run = %d, %v; want exit 0", code, err)
	}

	raw, err := os.ReadFile(hist)
	if err != nil {
		t.Fatalf("read history: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 24 {
		t.Fatalf("history line count = %d, want 24 (12 invoke + 12 completion)", len(lines))
	}
}

func TestUnknownProfileExitsConfigErrorBeforeCreatingHistory(t *testing.T) {
	srv := startFakeKVServer(t, "kv-n1")
	hist := filepath.Join(t.TempDir(), "history.jsonl")

	code, err := run([]string{
		"--history", hist,
		"--seed", "1",
		"--profile", "nonexistent_profile",
		"--targets", srv.URL,
		"--quiet",
	})
	if code != exitConfig || err == nil {
		t.Fatalf("run = %d, %v; want exit %d (CONFIG_ERROR)", code, err, exitConfig)
	}
	if _, statErr := os.Stat(hist); statErr == nil {
		t.Fatalf("refused run should not have created history file %s", hist)
	}
}
