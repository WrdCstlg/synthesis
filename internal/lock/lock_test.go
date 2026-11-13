package lock

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"synthesis.dev/synthesis/pkg/schema"
)

const sampleConfigYAML = `version: prothesis/v1
name: kvfixture
harness:
  backend: compose
  file: docker-compose.yaml
  variant_env: KV_VARIANT
  prefix_env: KV_PREFIX
  nodes:
    - { id: kv-n1, service: kv, compose_service: kv-n1, role_hint: replica, port_env: KV_PORT_N1 }
    - { id: kv-n2, service: kv, compose_service: kv-n2, role_hint: replica, port_env: KV_PORT_N2 }
    - { id: kv-n3, service: kv, compose_service: kv-n3, role_hint: replica, port_env: KV_PORT_N3 }
  health:
    - node: "kv:*"
      probe: "http://{host}:{port}/healthz"
      timeout: 30s
  steady_state:
    probe: "docker compose exec -T kv-n1 /kv steady"
    timeout: 60s
driver:
  cmd: "./bin/loadgen --history {history_path} --seed {seed} --profile {profile}"
  profiles:
    smoke: { clients: 4, ops: 500, mix: { read: 0.5, write: 0.5 } }
    linear: { clients: 16, ops: 60000, mix: { read: 0.5, write: 0.5 } }
perturber:
  budget: { max_concurrent_faults: 1, max_faults_per_world: 1 }
  allow: [net.partition]
  deny: []
  constraints:
    - "never partition more than minority of kv"
oracles:
  dir: .synthesis/oracles
  builtin: [no_crash, no_panic_log, availability_after_heal, no_stuck_op]
profiles:
  buggy: { budget: 10m, worlds: 1, variant: buggy, driver_profile: linear, faults: ["net.partition(role:leader)@3000..7500"] }
  patched: { budget: 10m, worlds: 2, variant: kvfixed, driver_profile: linear, faults: ["net.partition(role:leader)@3000..7500"] }
artifacts: { dir: .synthesis/runs }
`

const sampleOracleYAML = `version: prothesis.oracle_def/v1
name: linearizable.kv
class: consistency
valid_phases: [DRIVE, HEAL, QUIESCE, ASSERT]
cmd: ./bin/linearizable-kv
timeout: 120s
`

func setupSampleProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "synthesis.yaml"), sampleConfigYAML)
	mustWrite(t, filepath.Join(dir, "docker-compose.yaml"), "services:\n  kv-n1:\n    image: synthesis/kvfixture:buggy\n")
	mustWrite(t, filepath.Join(dir, ".synthesis", "oracles", "linearizable.kv.yaml"), sampleOracleYAML)
	return dir
}

func mustWrite(t *testing.T, p, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", p, err)
	}
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
}

func lockSampleProject(t *testing.T, dir string) *Report {
	t.Helper()
	rep, err := Gate(GateOptions{ProjectDir: dir})
	if err != nil {
		t.Fatalf("initial Gate: %v", err)
	}
	if rep.Status != schema.LockAbsent {
		t.Fatalf("initial status = %q, want absent", rep.Status)
	}
	_, err = Write(WriteOptions{
		ProjectDir:  dir,
		Manifest:    rep.Manifest,
		Reason:      "initial test lock",
		ToolVersion: "0.1.0-demo",
		Now:         time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Write lock: %v", err)
	}
	rep2, err := Gate(GateOptions{ProjectDir: dir})
	if err != nil {
		t.Fatalf("verified Gate: %v", err)
	}
	if rep2.Status != schema.LockOK || rep2.VerifyExitCode() != schema.ExitPass {
		t.Fatalf("post-write status = %q (exit %d), want ok (0)", rep2.Status, rep2.VerifyExitCode())
	}
	return rep2
}

func TestLockDetectsAllWeakeningVectors(t *testing.T) {
	t.Run("workload_ops_reduced", func(t *testing.T) {
		dir := setupSampleProject(t)
		lockSampleProject(t, dir)

		weakened := strings.Replace(sampleConfigYAML, "ops: 60000", "ops: 100", 1)
		mustWrite(t, filepath.Join(dir, "synthesis.yaml"), weakened)

		rep, err := Gate(GateOptions{ProjectDir: dir})
		if err != nil {
			t.Fatalf("Gate: %v", err)
		}
		if rep.Status != schema.LockMismatch || rep.EnforceExitCode() != schema.ExitOracleDrift {
			t.Fatalf("status=%q exit=%d, want mismatch / 4", rep.Status, rep.EnforceExitCode())
		}
		if !strings.Contains(rep.Diagnosis(), "driver.profiles.linear.ops") {
			t.Fatalf("diagnosis did not name driver.profiles.linear.ops:\n%s", rep.Diagnosis())
		}
	})

	t.Run("fault_budget_or_schedule_reduced", func(t *testing.T) {
		dir := setupSampleProject(t)
		lockSampleProject(t, dir)

		weakened := strings.Replace(sampleConfigYAML, "worlds: 2", "worlds: 1", 1)
		mustWrite(t, filepath.Join(dir, "synthesis.yaml"), weakened)

		rep, err := Gate(GateOptions{ProjectDir: dir})
		if err != nil {
			t.Fatalf("Gate: %v", err)
		}
		if rep.Status != schema.LockMismatch || rep.EnforceExitCode() != schema.ExitOracleDrift {
			t.Fatalf("status=%q exit=%d, want mismatch / 4", rep.Status, rep.EnforceExitCode())
		}
		if !strings.Contains(rep.Diagnosis(), "profiles.patched.worlds") {
			t.Fatalf("diagnosis did not name profiles.patched.worlds:\n%s", rep.Diagnosis())
		}
	})

	t.Run("builtin_oracle_removed", func(t *testing.T) {
		dir := setupSampleProject(t)
		lockSampleProject(t, dir)

		weakened := strings.Replace(sampleConfigYAML, ", no_stuck_op", "", 1)
		mustWrite(t, filepath.Join(dir, "synthesis.yaml"), weakened)

		rep, err := Gate(GateOptions{ProjectDir: dir})
		if err != nil {
			t.Fatalf("Gate: %v", err)
		}
		if rep.Status != schema.LockMismatch || rep.EnforceExitCode() != schema.ExitOracleDrift {
			t.Fatalf("status=%q exit=%d, want mismatch / 4", rep.Status, rep.EnforceExitCode())
		}
	})

	t.Run("external_oracle_removed", func(t *testing.T) {
		dir := setupSampleProject(t)
		lockSampleProject(t, dir)

		if err := os.Remove(filepath.Join(dir, ".synthesis", "oracles", "linearizable.kv.yaml")); err != nil {
			t.Fatal(err)
		}
		rep, err := Gate(GateOptions{ProjectDir: dir})
		if err != nil {
			t.Fatalf("Gate: %v", err)
		}
		if rep.Status != schema.LockMismatch || rep.EnforceExitCode() != schema.ExitOracleDrift {
			t.Fatalf("status=%q exit=%d, want mismatch / 4", rep.Status, rep.EnforceExitCode())
		}
	})

	t.Run("compose_topology_modified", func(t *testing.T) {
		dir := setupSampleProject(t)
		lockSampleProject(t, dir)

		mustWrite(t, filepath.Join(dir, "docker-compose.yaml"), "services:\n  kv-n1:\n    image: synthesis/kvfixture:kvfixed\n")
		rep, err := Gate(GateOptions{ProjectDir: dir})
		if err != nil {
			t.Fatalf("Gate: %v", err)
		}
		if rep.Status != schema.LockMismatch {
			t.Fatalf("status=%q, want mismatch", rep.Status)
		}
	})

	t.Run("unused_smoke_profile_edit_ignored", func(t *testing.T) {
		dir := setupSampleProject(t)
		lockSampleProject(t, dir)

		uneditedGate := strings.Replace(sampleConfigYAML, "ops: 500", "ops: 250", 1)
		mustWrite(t, filepath.Join(dir, "synthesis.yaml"), uneditedGate)

		rep, err := Gate(GateOptions{ProjectDir: dir})
		if err != nil {
			t.Fatalf("Gate: %v", err)
		}
		if rep.Status != schema.LockOK {
			t.Fatalf("editing unreferenced driver profile triggered drift: %s", rep.Diagnosis())
		}
	})

	t.Run("hand_edited_lock_digest_detected", func(t *testing.T) {
		dir := setupSampleProject(t)
		lockSampleProject(t, dir)

		lp := Path(dir)
		raw, err := os.ReadFile(lp)
		if err != nil {
			t.Fatal(err)
		}
		tampered := strings.Replace(string(raw), `"value": "60000"`, `"value": "100"`, 1)
		mustWrite(t, lp, tampered)

		rep, err := Gate(GateOptions{ProjectDir: dir})
		if err != nil {
			t.Fatalf("Gate: %v", err)
		}
		if rep.Status != schema.LockMismatch || !rep.SelfDigestMismatch {
			t.Fatalf("expected SelfDigestMismatch mismatch, got status=%q mismatch=%v",
				rep.Status, rep.SelfDigestMismatch)
		}
	})
}

func TestFixtureLockMatchesCommittedManifest(t *testing.T) {
	fixtureDir := filepath.Join("..", "..", "testdata", "kvfixture")
	if os.Getenv("SYNTHESIS_INIT_FIXTURE_LOCK") == "1" {
		rawCfg, err := os.ReadFile(filepath.Join(fixtureDir, "synthesis.yaml"))
		if err != nil {
			t.Fatalf("read fixture synthesis.yaml: %v", err)
		}
		oraclesDir, err := OracleDirFromConfig(rawCfg)
		if err != nil {
			t.Fatalf("OracleDirFromConfig: %v", err)
		}
		m, err := Build(fixtureDir, rawCfg, oraclesDir)
		if err != nil {
			t.Fatalf("Build manifest: %v", err)
		}
		_, err = Write(WriteOptions{
			ProjectDir:  fixtureDir,
			Manifest:    m,
			Reason:      "initial locked baseline for kvfixture public demonstration (AT-1..AT-3)",
			ToolVersion: "0.1.0-demo",
			Now:         time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC),
		})
		if err != nil {
			t.Fatalf("Write fixture lock: %v", err)
		}
	}

	rep, err := Gate(GateOptions{ProjectDir: fixtureDir})
	if err != nil {
		t.Fatalf("Gate(testdata/kvfixture): %v", err)
	}
	if rep.Status != schema.LockOK || rep.VerifyExitCode() != schema.ExitPass {
		t.Fatalf("testdata/kvfixture lock status=%q:\n%s", rep.Status, rep.Diagnosis())
	}
}
