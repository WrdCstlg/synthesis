package recorder

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"synthesis.dev/synthesis/pkg/schema"
)

func TestAllocRunIDAndWriteVerdict(t *testing.T) {
	root := t.TempDir()
	runs := RunsDir(root)
	wall := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

	id, dir, err := AllocRunID(runs, wall)
	if err != nil {
		t.Fatalf("AllocRunID: %v", err)
	}
	if !ValidRunID(id) {
		t.Fatalf("invalid run id %q", id)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Fatalf("run dir %q not created: %v", dir, err)
	}

	v := schema.NewVerdict(id, "patched", schema.VerdictPass)
	v.OracleLock = schema.OracleLock{Status: schema.LockOK, ManifestSHA: "sha256:0000"}
	v.Artifacts = filepath.ToSlash(dir)

	vp, err := SaveVerdictJSON(dir, &v)
	if err != nil {
		t.Fatalf("SaveVerdictJSON: %v", err)
	}
	raw, err := os.ReadFile(vp)
	if err != nil {
		t.Fatalf("read verdict: %v", err)
	}
	got, err := schema.UnmarshalVerdict(raw)
	if err != nil {
		t.Fatalf("UnmarshalVerdict: %v", err)
	}
	if got.RunID != id || got.Verdict != schema.VerdictPass {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}

func TestSafeNameRejectsUnsafeInputs(t *testing.T) {
	for _, bad := range []string{"", "../etc", "con", "nul.txt", "trailing.", "a/b", "a\\b"} {
		if err := SafeName(bad); err == nil {
			t.Errorf("SafeName(%q) = nil, want error", bad)
		}
	}
	for _, good := range []string{"kv-n1", "linearizable.kv.yaml", "world-01"} {
		if err := SafeName(good); err != nil {
			t.Errorf("SafeName(%q) unexpected error: %v", good, err)
		}
	}
}
