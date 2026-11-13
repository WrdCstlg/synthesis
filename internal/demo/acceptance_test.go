package demo

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"synthesis.dev/synthesis/internal/lock"
	"synthesis.dev/synthesis/pkg/schema"
)

func requireAcceptanceEnv(t *testing.T) string {
	t.Helper()
	if os.Getenv("SYNTHESIS_ACCEPTANCE") != "1" {
		t.Skip("skipping docker compose acceptance test (set SYNTHESIS_ACCEPTANCE=1 to enable)")
	}
	repoRoot, err := locateRepoRoot()
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	ensureFixtureArtifactsBuilt(t, repoRoot)
	return repoRoot
}

func locateRepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		modPath := filepath.Join(dir, "go.mod")
		if raw, rerr := os.ReadFile(modPath); rerr == nil && strings.Contains(string(raw), "module synthesis.dev/synthesis") {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", os.ErrNotExist
		}
		dir = parent
	}
}

func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

func ensureFixtureArtifactsBuilt(t *testing.T, repoRoot string) {
	t.Helper()
	fixtureDir := filepath.Join(repoRoot, "testdata", "kvfixture")
	fixtureBin := filepath.Join(fixtureDir, "bin")
	if err := os.MkdirAll(fixtureBin, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", fixtureBin, err)
	}

	oracleBin := filepath.Join(fixtureBin, "linearizable-kv"+exeSuffix())
	if _, err := os.Stat(oracleBin); err != nil {
		cmd := exec.Command("go", "build", "-trimpath", "-ldflags=-s -w", "-o", oracleBin, "./cmd/synthesis-oracle-linearizable")
		cmd.Dir = repoRoot
		if out, berr := cmd.CombinedOutput(); berr != nil {
			t.Fatalf("build linearizable-kv: %v\n%s", berr, string(out))
		}
	}

	loadgenBin := filepath.Join(fixtureBin, "loadgen"+exeSuffix())
	if _, err := os.Stat(loadgenBin); err != nil {
		cmd := exec.Command("go", "build", "-trimpath", "-ldflags=-s -w", "-o", loadgenBin, "./cmd/loadgen")
		cmd.Dir = fixtureDir
		if out, berr := cmd.CombinedOutput(); berr != nil {
			t.Fatalf("build loadgen: %v\n%s", berr, string(out))
		}
	}
}

func cleanupRunArtifactDir(sum *DemoRunSummary) {
	if sum == nil || sum.VerdictPath == "" {
		return
	}
	_ = os.RemoveAll(filepath.Dir(sum.VerdictPath))
}

// TestAT1_DefectiveFixtureFailsWithWitness verifies AT-1:
// Given the defective fixture, when `synthesis run` executes, then it exits
// with the FAIL code (1), and the verdict JSON and witness SVG show a stale
// read inside the partition window.
func TestAT1_DefectiveFixtureFailsWithWitness(t *testing.T) {
	repoRoot := requireAcceptanceEnv(t)
	fixtureDir := filepath.Join(repoRoot, "testdata", "kvfixture")

	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	sum, err := ExecuteDemoRun(ctx, DemoRunOptions{
		ProjectDir:  fixtureDir,
		ProfileName: "buggy",
		Seed:        20261109,
		SeedSet:     true,
		Stdout:      &stdout,
		Stderr:      &stderr,
	})
	defer cleanupRunArtifactDir(sum)
	if err != nil {
		t.Fatalf("ExecuteDemoRun(buggy) returned error: %v\nstderr:\n%s", err, stderr.String())
	}
	if sum.ExitCode != schema.ExitFail {
		t.Fatalf("AT-1 exit code = %d, want %d (FAIL)\nstdout:\n%s\nstderr:\n%s",
			sum.ExitCode, schema.ExitFail, stdout.String(), stderr.String())
	}
	if sum.Verdict == nil || sum.Verdict.Verdict != schema.VerdictFail {
		t.Fatalf("AT-1 verdict = %+v, want FAIL", sum.Verdict)
	}

	rawVerdict, err := os.ReadFile(sum.VerdictPath)
	if err != nil {
		t.Fatalf("read verdict.json: %v", err)
	}
	parsed, err := schema.UnmarshalVerdict(rawVerdict)
	if err != nil {
		t.Fatalf("UnmarshalVerdict: %v", err)
	}
	if err := parsed.Validate(); err != nil {
		t.Fatalf("Verdict.Validate: %v", err)
	}
	if len(parsed.Violations) == 0 {
		t.Fatalf("AT-1 expected at least 1 violation in verdict.json")
	}

	v0 := parsed.Violations[0]
	if v0.Oracle != "linearizable.kv" {
		t.Fatalf("AT-1 violation oracle = %q, want linearizable.kv", v0.Oracle)
	}
	if v0.Witness.Key == "" || len(v0.Witness.OpIDs) < 2 {
		t.Fatalf("AT-1 witness incomplete: key=%q op_ids=%v", v0.Witness.Key, v0.Witness.OpIDs)
	}

	var injectMS, withdrawMS int64 = -1, -1
	for _, ev := range v0.CausalTimeline {
		if ev.Event == schema.TimelineFault {
			if strings.HasPrefix(ev.Detail, "inject net.partition") {
				injectMS = ev.TMS
			}
			if strings.HasPrefix(ev.Detail, "withdraw net.partition") {
				withdrawMS = ev.TMS
			}
		}
	}
	if injectMS < 0 || withdrawMS <= injectMS {
		t.Fatalf("AT-1 causal_timeline missing valid partition window: inject=%d withdraw=%d", injectMS, withdrawMS)
	}

	sawReadInWindow := false
	for _, ev := range v0.CausalTimeline {
		if ev.Event == schema.TimelineOp && strings.Contains(ev.Detail, " read ") {
			if ev.TMS >= injectMS && ev.TMS <= withdrawMS {
				sawReadInWindow = true
				break
			}
		}
	}
	if !sawReadInWindow {
		t.Fatalf("AT-1 expected witnessed stale read inside partition window [%d..%d ms]; timeline=%+v",
			injectMS, withdrawMS, v0.CausalTimeline)
	}

	if sum.WitnessSVGPath == "" {
		t.Fatalf("AT-1 did not produce witness.svg")
	}
	svgBytes, err := os.ReadFile(sum.WitnessSVGPath)
	if err != nil {
		t.Fatalf("read witness.svg: %v", err)
	}
	svgText := string(svgBytes)
	if !strings.Contains(svgText, "<svg") || !strings.Contains(svgText, "network partition") || !strings.Contains(svgText, "read") {
		t.Fatalf("AT-1 witness.svg missing expected partition/stale-read elements")
	}
}

// TestAT2_FixedFixturePassesFullLockedBudget verifies AT-2:
// Given the fixed fixture, when `synthesis run` executes, then it exits with
// the PASS code (0), and the verdict records that the full locked budget was consumed.
// Narrowing below the locked budget refuses PASS with INCONCLUSIVE (exit 2).
func TestAT2_FixedFixturePassesFullLockedBudget(t *testing.T) {
	repoRoot := requireAcceptanceEnv(t)
	fixtureDir := filepath.Join(repoRoot, "testdata", "kvfixture")

	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	sum, err := ExecuteDemoRun(ctx, DemoRunOptions{
		ProjectDir:  fixtureDir,
		ProfileName: "patched",
		Seed:        20261109,
		SeedSet:     true,
		Stdout:      &stdout,
		Stderr:      &stderr,
	})
	defer cleanupRunArtifactDir(sum)
	if err != nil {
		t.Fatalf("ExecuteDemoRun(patched) error: %v\nstderr:\n%s", err, stderr.String())
	}
	if sum.ExitCode != schema.ExitPass {
		t.Fatalf("AT-2 exit code = %d, want %d (PASS)\nstdout:\n%s\nstderr:\n%s",
			sum.ExitCode, schema.ExitPass, stdout.String(), stderr.String())
	}
	if sum.Verdict == nil || sum.Verdict.Verdict != schema.VerdictPass {
		t.Fatalf("AT-2 verdict = %+v, want PASS", sum.Verdict)
	}
	if sum.Verdict.OracleLock.Status != schema.LockOK {
		t.Fatalf("AT-2 oracle_lock.status = %q, want ok", sum.Verdict.OracleLock.Status)
	}
	if sum.Verdict.Budget.Narrowed {
		t.Fatalf("AT-2 budget.narrowed = true, want false")
	}
	if sum.Verdict.Budget.WorldsPlanned != 2 || sum.Verdict.Budget.WorldsRun != 2 {
		t.Fatalf("AT-2 worlds = %d/%d, want 2/2 full locked budget",
			sum.Verdict.Budget.WorldsRun, sum.Verdict.Budget.WorldsPlanned)
	}

	t.Run("narrowed_budget_refuses_pass", func(t *testing.T) {
		var nOut, nErr bytes.Buffer
		nSum, err := ExecuteDemoRun(ctx, DemoRunOptions{
			ProjectDir:     fixtureDir,
			ProfileName:    "patched",
			Seed:           20261109,
			SeedSet:        true,
			WorldsOverride: 1,
			Stdout:         &nOut,
			Stderr:         &nErr,
		})
		defer cleanupRunArtifactDir(nSum)
		if err != nil {
			t.Fatalf("ExecuteDemoRun(patched --worlds 1) error: %v", err)
		}
		if nSum.ExitCode == schema.ExitPass || nSum.ExitCode != schema.ExitInconclusive {
			t.Fatalf("narrowed run exit = %d, want %d (INCONCLUSIVE, never PASS)", nSum.ExitCode, schema.ExitInconclusive)
		}
		if nSum.Verdict == nil || !nSum.Verdict.Budget.Narrowed || nSum.Verdict.OracleLock.Status != schema.LockBypassed {
			t.Fatalf("narrowed verdict = %+v, want Narrowed=true and OracleLock.Status=bypassed", nSum.Verdict)
		}
	})
}

// TestAT3_TamperedLockRefusesBeforeBoot verifies AT-3:
// Given a locked test, when the workload, the fault budget or the oracle set is
// reduced, then `synthesis run` refuses before running and exits with the
// lock-violation code (4).
func TestAT3_TamperedLockRefusesBeforeBoot(t *testing.T) {
	repoRoot := requireAcceptanceEnv(t)
	fixtureDir := filepath.Join(repoRoot, "testdata", "kvfixture")

	cases := []struct {
		name   string
		mutate func(t *testing.T, dir string)
	}{
		{
			name: "workload_reduced",
			mutate: func(t *testing.T, dir string) {
				p := filepath.Join(dir, "synthesis.yaml")
				raw, err := os.ReadFile(p)
				if err != nil {
					t.Fatal(err)
				}
				edited := strings.Replace(string(raw), "ops: 60000", "ops: 100", 1)
				if err := os.WriteFile(p, []byte(edited), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "fault_budget_reduced",
			mutate: func(t *testing.T, dir string) {
				p := filepath.Join(dir, "synthesis.yaml")
				raw, err := os.ReadFile(p)
				if err != nil {
					t.Fatal(err)
				}
				edited := strings.Replace(string(raw), "worlds: 2", "worlds: 1", 1)
				if err := os.WriteFile(p, []byte(edited), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "oracle_set_reduced",
			mutate: func(t *testing.T, dir string) {
				if err := os.Remove(filepath.Join(dir, ".synthesis", "oracles", "linearizable.kv.yaml")); err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmp := copyFixtureMetadataForTamperTest(t, fixtureDir)
			tc.mutate(t, tmp)

			var stdout, stderr bytes.Buffer
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			sum, err := ExecuteDemoRun(ctx, DemoRunOptions{
				ProjectDir:  tmp,
				ProfileName: "patched",
				Stdout:      &stdout,
				Stderr:      &stderr,
			})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if sum.ExitCode != schema.ExitOracleDrift {
				t.Fatalf("exit code = %d, want %d (ORACLE_DRIFT)\nstderr:\n%s",
					sum.ExitCode, schema.ExitOracleDrift, stderr.String())
			}
			if sum.Verdict != nil {
				t.Fatalf("expected nil Verdict (refused before cluster boot), got %+v", sum.Verdict)
			}
			if !strings.Contains(stderr.String(), "ORACLE DRIFT") {
				t.Fatalf("stderr did not report ORACLE DRIFT:\n%s", stderr.String())
			}
		})
	}
}

// TestAT4_PostLoadFaultAndCrashedOracleNeverPass verifies AT-4:
// Given a run whose fault lands after the load has ended, or whose oracle
// crashes, then the verdict is NOT PASS.
func TestAT4_PostLoadFaultAndCrashedOracleNeverPass(t *testing.T) {
	repoRoot := requireAcceptanceEnv(t)
	fixtureDir := filepath.Join(repoRoot, "testdata", "kvfixture")

	t.Run("fault_after_load_ended", func(t *testing.T) {
		tmp := createCustomLockedProject(t, fixtureDir, func(cfg string) string {
			// 20 operations finish in < 200 ms, long before @2500..4000ms fault injection.
			cfg = strings.Replace(cfg, "ops: 60000", "ops: 20", 1)
			cfg = strings.ReplaceAll(cfg, "@3000..7500", "@2500..4000")
			cfg = strings.Replace(cfg, "worlds: 2", "worlds: 1", 1)
			return cfg
		}, "")

		var stdout, stderr bytes.Buffer
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
		defer cancel()

		sum, err := ExecuteDemoRun(ctx, DemoRunOptions{
			ProjectDir:  tmp,
			ProfileName: "patched",
			Seed:        20261109,
			SeedSet:     true,
			Stdout:      &stdout,
			Stderr:      &stderr,
		})
		if err != nil {
			t.Fatalf("ExecuteDemoRun error: %v", err)
		}
		if sum.ExitCode == schema.ExitPass {
			t.Fatalf("AT-4 VIOLATION: post-load fault returned PASS (exit 0)!")
		}
		if sum.ExitCode != schema.ExitInconclusive || sum.Verdict == nil || sum.Verdict.Verdict != schema.VerdictInconclusive {
			t.Fatalf("AT-4 expected INCONCLUSIVE (exit 2), got exit=%d verdict=%+v", sum.ExitCode, sum.Verdict)
		}
		if !strings.Contains(sum.Verdict.Error, "after client load had already ended") {
			t.Fatalf("AT-4 expected post-load fault explanation, got: %q", sum.Verdict.Error)
		}
	})

	t.Run("oracle_crashes", func(t *testing.T) {
		crashSrcDir := t.TempDir()
		crashMain := `package main
import (
	"fmt"
	"os"
)
func main() {
	fmt.Fprintln(os.Stderr, "simulated external oracle crash")
	os.Exit(2)
}
`
		if err := os.WriteFile(filepath.Join(crashSrcDir, "main.go"), []byte(crashMain), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(crashSrcDir, "go.mod"), []byte("module crashoracle\n\ngo 1.22\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		crashBin := filepath.Join(crashSrcDir, "crash-oracle"+exeSuffix())
		buildCmd := exec.Command("go", "build", "-o", crashBin, ".")
		buildCmd.Dir = crashSrcDir
		if out, err := buildCmd.CombinedOutput(); err != nil {
			t.Fatalf("build crash-oracle: %v\n%s", err, string(out))
		}

		tmp := createCustomLockedProject(t, fixtureDir, func(cfg string) string {
			cfg = strings.ReplaceAll(cfg, "@3000..7500", "@1500..2800")
			cfg = strings.Replace(cfg, "worlds: 2", "worlds: 1", 1)
			return cfg
		}, crashBin)

		var stdout, stderr bytes.Buffer
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
		defer cancel()

		sum, err := ExecuteDemoRun(ctx, DemoRunOptions{
			ProjectDir:  tmp,
			ProfileName: "patched",
			Seed:        20261109,
			SeedSet:     true,
			Stdout:      &stdout,
			Stderr:      &stderr,
		})
		if err != nil {
			t.Fatalf("ExecuteDemoRun error: %v", err)
		}
		if sum.ExitCode == schema.ExitPass {
			t.Fatalf("AT-4 VIOLATION: crashed oracle returned PASS (exit 0)!")
		}
		if sum.ExitCode != schema.ExitInconclusive || sum.Verdict == nil || sum.Verdict.Verdict != schema.VerdictInconclusive {
			t.Fatalf("AT-4 expected INCONCLUSIVE (exit 2), got exit=%d verdict=%+v", sum.ExitCode, sum.Verdict)
		}
		if !strings.Contains(sum.Verdict.Error, "oracle linearizable.kv inconclusive") {
			t.Fatalf("AT-4 expected oracle crash reason, got: %q", sum.Verdict.Error)
		}
	})
}

func copyFixtureMetadataForTamperTest(t *testing.T, fixtureDir string) string {
	t.Helper()
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, ".synthesis", "oracles"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{
		"synthesis.yaml",
		"docker-compose.yaml",
		filepath.Join(".synthesis", "lock"),
		filepath.Join(".synthesis", "oracles", "linearizable.kv.yaml"),
	} {
		raw, err := os.ReadFile(filepath.Join(fixtureDir, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if err := os.WriteFile(filepath.Join(tmp, rel), raw, 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	return tmp
}

func createCustomLockedProject(t *testing.T, fixtureDir string, editCfg func(string) string, overrideOracleBin string) string {
	t.Helper()
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, ".synthesis", "oracles"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(tmp, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}

	rawCfg, err := os.ReadFile(filepath.Join(fixtureDir, "synthesis.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	editedCfg := editCfg(string(rawCfg))
	if err := os.WriteFile(filepath.Join(tmp, "synthesis.yaml"), []byte(editedCfg), 0o644); err != nil {
		t.Fatal(err)
	}

	composeRaw, err := os.ReadFile(filepath.Join(fixtureDir, "docker-compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "docker-compose.yaml"), composeRaw, 0o644); err != nil {
		t.Fatal(err)
	}

	// Copy loadgen and oracle binaries into tmp/bin.
	loadgenName := "loadgen" + exeSuffix()
	copyFileMode(t, filepath.Join(fixtureDir, "bin", loadgenName), filepath.Join(tmp, "bin", loadgenName))

	oracleName := "linearizable-kv" + exeSuffix()
	if overrideOracleBin != "" {
		copyFileMode(t, overrideOracleBin, filepath.Join(tmp, "bin", oracleName))
	} else {
		copyFileMode(t, filepath.Join(fixtureDir, "bin", oracleName), filepath.Join(tmp, "bin", oracleName))
	}

	oracleYAML, err := os.ReadFile(filepath.Join(fixtureDir, ".synthesis", "oracles", "linearizable.kv.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, ".synthesis", "oracles", "linearizable.kv.yaml"), oracleYAML, 0o644); err != nil {
		t.Fatal(err)
	}

	rep, err := lock.Gate(lock.GateOptions{ProjectDir: tmp})
	if err != nil {
		t.Fatalf("Gate(tmp): %v", err)
	}
	_, err = lock.Write(lock.WriteOptions{
		ProjectDir:  tmp,
		Manifest:    rep.Manifest,
		Reason:      "locked test project for AT-4 verification",
		ToolVersion: "0.1.0-demo",
		Executables: rep.ExecutablesNow,
		Platform:    lock.CurrentPlatform(),
		Now:         time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("lock.Write(tmp): %v", err)
	}
	return tmp
}

func copyFileMode(t *testing.T, src, dst string) {
	t.Helper()
	raw, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	if err := os.WriteFile(dst, raw, 0o755); err != nil {
		t.Fatalf("write %s: %v", dst, err)
	}
}

// TestAT6_RuntimeNetworkAndEnvConfinement verifies runtime network and environment isolation:
// peer traffic runs over an internal Docker bridge, host bindings are restricted to 127.0.0.1,
// container environments contain only documented fixture keys (zero telemetry/secret/provider keys),
// and live /proc/net sockets connect exclusively to loopback or internal Docker subnets.
func TestAT6_RuntimeNetworkAndEnvConfinement(t *testing.T) {
	repoRoot := requireAcceptanceEnv(t)
	fixtureDir := filepath.Join(repoRoot, "testdata", "kvfixture")

	rawCfg, err := os.ReadFile(filepath.Join(fixtureDir, "synthesis.yaml"))
	if err != nil {
		t.Fatalf("read synthesis.yaml: %v", err)
	}
	cfg, err := schema.DecodeConfig(rawCfg)
	if err != nil {
		t.Fatalf("DecodeConfig: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	cl, err := bootComposeCluster(ctx, fixtureDir, cfg, "syn_at6_net", "kvfixed", 20261109)
	if err != nil {
		t.Fatalf("bootComposeCluster: %v", err)
	}
	defer func() { _ = cl.teardown(context.Background()) }()

	// 1. Inspect Docker networks: peer bridge must be isolated (Internal=true) and collect allowed CIDRs.
	peerInternal, peerSubnets, err := inspectDockerNetworkInfo(ctx, cl.PeerNetwork)
	if err != nil {
		t.Fatalf("inspect peer network %s: %v", cl.PeerNetwork, err)
	}
	if !peerInternal {
		t.Fatalf("peer network %s is not internal, want isolated peer bridge", cl.PeerNetwork)
	}
	_, clientSubnets, err := inspectDockerNetworkInfo(ctx, cl.ClientNet)
	if err != nil {
		t.Fatalf("inspect client network %s: %v", cl.ClientNet, err)
	}
	var allowedNets []*net.IPNet
	allowedNets = append(allowedNets, peerSubnets...)
	allowedNets = append(allowedNets, clientSubnets...)
	if len(allowedNets) == 0 {
		t.Fatalf("expected at least one IPAM subnet across %s / %s", cl.PeerNetwork, cl.ClientNet)
	}

	// 2. Drive a real PUT and GET on the live leader so client + Raft sockets are exercised.
	httpClient := &http.Client{Timeout: 3 * time.Second}
	var leaderURL string
	for _, n := range cl.Nodes {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, n.BaseURL()+"/status", nil)
		resp, serr := httpClient.Do(req)
		if serr != nil {
			continue
		}
		var st statusPayload
		derr := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&st)
		_ = resp.Body.Close()
		if derr == nil && strings.EqualFold(st.Role, "leader") {
			leaderURL = n.BaseURL()
			break
		}
	}
	if leaderURL == "" {
		leaderURL = cl.Nodes[0].BaseURL()
	}

	putReq, err := http.NewRequestWithContext(ctx, http.MethodPut, leaderURL+"/kv/netcheck", strings.NewReader(`{"value":42}`))
	if err != nil {
		t.Fatalf("build PUT request: %v", err)
	}
	putReq.Header.Set("Content-Type", "application/json")
	putResp, err := httpClient.Do(putReq)
	if err != nil {
		t.Fatalf("PUT %s/kv/netcheck: %v", leaderURL, err)
	}
	_ = putResp.Body.Close()
	if putResp.StatusCode != http.StatusOK {
		t.Fatalf("PUT /kv/netcheck status = %d, want 200", putResp.StatusCode)
	}

	getReq, _ := http.NewRequestWithContext(ctx, http.MethodGet, leaderURL+"/kv/netcheck", nil)
	getResp, err := httpClient.Do(getReq)
	if err != nil {
		t.Fatalf("GET %s/kv/netcheck: %v", leaderURL, err)
	}
	_ = getResp.Body.Close()
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /kv/netcheck status = %d, want 200", getResp.StatusCode)
	}

	// 3. Enforce loopback-only port bindings, env allowlist, and live /proc/net socket confinement.
	allowedEnv := map[string]bool{
		"PATH":               true,
		"HOSTNAME":           true,
		"KV_TICK_MS":         true,
		"KV_HEARTBEAT_MS":    true,
		"KV_ELECTION_MIN_MS": true,
		"KV_ELECTION_MAX_MS": true,
		"KV_RPC_TIMEOUT_MS":  true,
		"KV_APPLY_WAIT_MS":   true,
		"KV_CLIENT_ADDR":     true,
		"KV_PEER_ADDR":       true,
		"KV_DATA_DIR":        true,
		"KV_ROLE_HINT":       true,
		"KV_NODE_ID":         true,
		"KV_PEERS":           true,
		"KV_SEED":            true,
	}
	forbiddenEnvKey := regexp.MustCompile(`(?i)(API_KEY|TOKEN|SECRET|TELEMETRY|OTEL|MODEL|PROVIDER|LLM)`)

	for _, n := range cl.Nodes {
		if err := inspectContainerRuntimeConfinement(ctx, n.ContainerName, allowedEnv, forbiddenEnvKey, allowedNets); err != nil {
			t.Fatalf("container %s confinement failure: %v", n.ContainerName, err)
		}
	}
}

func inspectDockerNetworkInfo(ctx context.Context, netName string) (bool, []*net.IPNet, error) {
	cmd := exec.CommandContext(ctx, "docker", "network", "inspect", netName)
	out, err := cmd.Output()
	if err != nil {
		return false, nil, err
	}
	var payload []struct {
		Isolated bool `json:"Internal"`
		IPAM     struct {
			Config []struct {
				Subnet string `json:"Subnet"`
			} `json:"Config"`
		} `json:"IPAM"`
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		return false, nil, err
	}
	if len(payload) == 0 {
		return false, nil, fmt.Errorf("empty docker network inspect output for %s", netName)
	}
	var nets []*net.IPNet
	for _, cfg := range payload[0].IPAM.Config {
		if cfg.Subnet == "" {
			continue
		}
		_, ipNet, perr := net.ParseCIDR(cfg.Subnet)
		if perr != nil {
			return false, nil, fmt.Errorf("parse subnet %q: %w", cfg.Subnet, perr)
		}
		nets = append(nets, ipNet)
	}
	return payload[0].Isolated, nets, nil
}

func inspectContainerRuntimeConfinement(
	ctx context.Context,
	containerName string,
	allowedEnv map[string]bool,
	forbiddenEnvKey *regexp.Regexp,
	allowedNets []*net.IPNet,
) error {
	cmd := exec.CommandContext(ctx, "docker", "inspect", containerName)
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("docker inspect: %w", err)
	}
	var items []struct {
		Config struct {
			Env []string `json:"Env"`
		} `json:"Config"`
		NetworkSettings struct {
			Ports map[string][]struct {
				HostIP   string `json:"HostIp"`
				HostPort string `json:"HostPort"`
			} `json:"Ports"`
		} `json:"NetworkSettings"`
	}
	if err := json.Unmarshal(out, &items); err != nil {
		return fmt.Errorf("parse docker inspect: %w", err)
	}
	if len(items) == 0 {
		return fmt.Errorf("empty docker inspect output")
	}

	ports := items[0].NetworkSettings.Ports
	clientBindings := ports["8080/tcp"]
	if len(clientBindings) == 0 {
		return fmt.Errorf("expected 8080/tcp loopback binding, got none")
	}
	for _, b := range clientBindings {
		if b.HostIP != "127.0.0.1" {
			return fmt.Errorf("8080/tcp HostIp = %q, want 127.0.0.1", b.HostIP)
		}
	}
	if peerBindings := ports["9090/tcp"]; len(peerBindings) > 0 {
		return fmt.Errorf("peer port 9090/tcp must not bind to host, got %+v", peerBindings)
	}

	var unexpected []string
	for _, kvPair := range items[0].Config.Env {
		key, _, _ := strings.Cut(kvPair, "=")
		if forbiddenEnvKey.MatchString(key) {
			return fmt.Errorf("forbidden environment variable key present: %s", key)
		}
		if !allowedEnv[key] {
			unexpected = append(unexpected, key)
		}
	}
	if len(unexpected) > 0 {
		sort.Strings(unexpected)
		return fmt.Errorf("unexpected container env keys: %v", unexpected)
	}

	procCmd := exec.CommandContext(ctx, "docker", "exec", containerName, "sh", "-c",
		"cat /proc/net/tcp /proc/net/tcp6 /proc/net/udp /proc/net/udp6 2>/dev/null || true")
	procOut, err := procCmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("read /proc/net tables: %w (%s)", err, strings.TrimSpace(string(procOut)))
	}
	remoteIPs, err := parseProcNetRemoteIPs(string(procOut))
	if err != nil {
		return err
	}
	for _, ip := range remoteIPs {
		if ip.IsLoopback() || ip.IsUnspecified() {
			continue
		}
		confined := false
		for _, subnet := range allowedNets {
			if subnet.Contains(ip) {
				confined = true
				break
			}
		}
		if !confined {
			return fmt.Errorf("non-loopback remote socket IP %s outside Docker subnets %v", ip, allowedNets)
		}
	}
	return nil
}

func parseProcNetRemoteIPs(procText string) ([]net.IP, error) {
	var ips []net.IP
	for _, rawLine := range strings.Split(procText, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "sl") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		remAddr := fields[2]
		hexIP, _, ok := strings.Cut(remAddr, ":")
		if !ok {
			continue
		}
		switch len(hexIP) {
		case 8:
			ip, err := parseHexIPv4(hexIP)
			if err != nil {
				return nil, fmt.Errorf("parse ipv4 %q: %w", remAddr, err)
			}
			ips = append(ips, ip)
		case 32:
			ip, err := parseHexIPv6(hexIP)
			if err != nil {
				return nil, fmt.Errorf("parse ipv6 %q: %w", remAddr, err)
			}
			ips = append(ips, ip)
		default:
			if _, err := strconv.ParseUint(hexIP, 16, 64); err != nil {
				return nil, fmt.Errorf("unexpected rem_address format %q", remAddr)
			}
		}
	}
	return ips, nil
}

func parseHexIPv4(h string) (net.IP, error) {
	b, err := hex.DecodeString(h)
	if err != nil || len(b) != 4 {
		return nil, fmt.Errorf("invalid hex ipv4 %q", h)
	}
	// /proc/net/tcp encodes IPv4 words in little-endian host order on x86_64/arm64 Linux.
	return net.IPv4(b[3], b[2], b[1], b[0]), nil
}

func parseHexIPv6(h string) (net.IP, error) {
	b, err := hex.DecodeString(h)
	if err != nil || len(b) != 16 {
		return nil, fmt.Errorf("invalid hex ipv6 %q", h)
	}
	out := make(net.IP, 16)
	for word := 0; word < 16; word += 4 {
		out[word+0] = b[word+3]
		out[word+1] = b[word+2]
		out[word+2] = b[word+1]
		out[word+3] = b[word+0]
	}
	return out, nil
}
