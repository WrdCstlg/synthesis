package schema

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	switch name {
	case "history.golden.jsonl":
		return []byte(
			`{"t_ns":1725300000000000,"process":3,"type":"invoke","f":"write","key":"k/42","value":7,"op_id":8891}` + "\n" +
				`{"t_ns":1725300000410000,"process":3,"type":"ok","f":"write","key":"k/42","value":7,"op_id":8891}` + "\n" +
				`{"t_ns":1725300001010000,"process":5,"type":"invoke","f":"read","key":"k/42","op_id":8903}` + "\n" +
				`{"t_ns":1725300001330000,"process":5,"type":"fail","f":"read","key":"k/42","error":"timeout","op_id":8903}` + "\n" +
				`{"t_ns":1725300002000000,"type":"info","event":"phase","phase":"HEAL"}` + "\n",
		)
	case "oracle_input.golden.json":
		return []byte(`{
  "schema": "prothesis.oracle_input/v1",
  "history_path": "/path/to/history.jsonl",
  "final_state_path": "/path/to/final_state.json",
  "telemetry_path": "/path/to/telemetry.json",
  "world_path": "/path/to/world.thesis",
  "phases": [
    {"phase": "DRIVE", "start_ms": 0, "end_ms": 42000},
    {"phase": "HEAL", "start_ms": 42000, "end_ms": 45000},
    {"phase": "QUIESCE", "start_ms": 45000, "end_ms": 50000},
    {"phase": "ASSERT", "start_ms": 50000, "end_ms": 55000}
  ]
}
`)
	case "oracle_output.golden.json":
		return []byte(`{
  "schema": "prothesis.oracle_output/v1",
  "oracle": "linearizable.kv",
  "class": "consistency",
  "valid_phases": ["ASSERT"],
  "status": "violated",
  "witness": { "op_ids": [8891, 8903], "key": "k/42" },
  "explanation": "process 5 read stale value under lease violation"
}
`)
	default:
		b, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatalf("read testdata/%s: %v", name, err)
		}
		return b
	}
}

func TestGoldenVerdictRoundTrip(t *testing.T) {
	raw := readTestdata(t, "verdict.golden.json")

	v, err := UnmarshalVerdict(raw)
	if err != nil {
		t.Fatalf("UnmarshalVerdict: %v", err)
	}
	if err := v.Validate(); err != nil {
		t.Fatalf("golden verdict does not validate: %v", err)
	}

	out, err := MarshalVerdict(v)
	if err != nil {
		t.Fatalf("MarshalVerdict: %v", err)
	}
	gotCanon, err := Canonicalize(out)
	if err != nil {
		t.Fatalf("canonicalize re-emission: %v", err)
	}
	wantCanon, err := Canonicalize(raw)
	if err != nil {
		t.Fatalf("canonicalize golden: %v", err)
	}
	if !bytes.Equal(gotCanon, wantCanon) {
		t.Errorf("verdict re-emission differs from golden.\n got: %s\nwant: %s", gotCanon, wantCanon)
	}

	if v.RunID != "r_2026_09_28_a41f" || v.Profile != "buggy" || v.Verdict != VerdictFail {
		t.Errorf("verdict header = %+v", *v)
	}
	if v.ExitCode() != ExitFail {
		t.Errorf("ExitCode = %d, want 1", v.ExitCode())
	}
	if len(v.Violations) != 1 {
		t.Fatalf("violations = %d, want 1", len(v.Violations))
	}
	vi := v.Violations[0]
	if vi.Severity != SeverityHigh {
		t.Errorf("severity = %q, want high", vi.Severity)
	}
	if len(vi.Witness.OpIDs) != 3 || vi.Witness.Key != "k0" {
		t.Errorf("witness = %+v", vi.Witness)
	}
	if v.OracleLock.Status != LockOK || !strings.HasPrefix(v.OracleLock.ManifestSHA, HashPrefix) {
		t.Errorf("oracle_lock = %+v", v.OracleLock)
	}
}

func TestVerdictPresenceRule(t *testing.T) {
	v := NewVerdict("r_2026_09_28_a41f", "patched", VerdictPass)
	out, err := MarshalVerdict(&v)
	if err != nil {
		t.Fatalf("MarshalVerdict: %v", err)
	}
	if !strings.Contains(string(out), `"violations": []`) {
		t.Errorf("empty violations must serialize as []: %s", out)
	}
	if v.OracleLock.Status != LockAbsent {
		t.Errorf("unset lock status = %q, want %q", v.OracleLock.Status, LockAbsent)
	}
}

func TestExitCodeBijection(t *testing.T) {
	for _, code := range AllExitCodes {
		res := code.Verdict()
		if !res.Valid() {
			t.Errorf("code %d -> invalid verdict %q", code, res)
		}
		if res.ExitCode() != code {
			t.Errorf("round-trip failed for %d -> %q -> %d", code, res, res.ExitCode())
		}
	}
}
