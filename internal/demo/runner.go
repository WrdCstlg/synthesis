package demo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"synthesis.dev/synthesis/internal/buildinfo"
	"synthesis.dev/synthesis/internal/lock"
	"synthesis.dev/synthesis/internal/recorder"
	"synthesis.dev/synthesis/pkg/schema"
)

// DemoRunOptions configures one execution of `synthesis run`.
type DemoRunOptions struct {
	ProjectDir     string
	ProfileName    string
	Seed           uint64
	SeedSet        bool
	WorldsOverride int
	BudgetOverride time.Duration
	Stdout         io.Writer
	Stderr         io.Writer
}

// DemoRunSummary captures the outcome and artifacts produced by `synthesis run`.
type DemoRunSummary struct {
	ExitCode       schema.ExitCode
	Verdict        *schema.Verdict
	VerdictPath    string
	WitnessSVGPath string
	LockReport     *lock.Report
}

// VerifyProjectLock checks .synthesis/lock for projectDir and prints the human diagnosis.
func VerifyProjectLock(projectDir string, stdout, stderr io.Writer) schema.ExitCode {
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	rep, err := lock.Gate(lock.GateOptions{ProjectDir: projectDir})
	if err != nil {
		fmt.Fprintf(stderr, "synthesis verify-lock: %v\n", err)
		return schema.ExitInconclusive
	}
	code := rep.VerifyExitCode()
	if code == schema.ExitPass {
		fmt.Fprint(stdout, rep.Diagnosis())
	} else {
		fmt.Fprint(stderr, rep.Diagnosis())
	}
	return code
}

// ExplainVerdictFile reads a verdict.json file and writes a concise human explanation.
func ExplainVerdictFile(verdictPath string, stdout io.Writer) error {
	if stdout == nil {
		stdout = os.Stdout
	}
	raw, err := os.ReadFile(verdictPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", verdictPath, err)
	}
	v, err := schema.UnmarshalVerdict(raw)
	if err != nil {
		return err
	}
	if err := v.Validate(); err != nil {
		return err
	}

	fmt.Fprintf(stdout, "run_id:      %s\n", v.RunID)
	fmt.Fprintf(stdout, "profile:     %s\n", v.Profile)
	fmt.Fprintf(stdout, "verdict:     %s (exit %d)\n", v.Verdict, v.ExitCode())
	fmt.Fprintf(stdout, "oracle_lock: %s (%s)\n", v.OracleLock.Status, v.OracleLock.ManifestSHA)
	fmt.Fprintf(stdout, "budget:      %d/%d world(s), %ds/%ds wall",
		v.Budget.WorldsRun, v.Budget.WorldsPlanned, v.Budget.UsedS, v.Budget.WallS)
	if v.Budget.Narrowed {
		fmt.Fprintf(stdout, " [NARROWED below locked %d world(s) / %ds]",
			v.Budget.WorldsRequired, v.Budget.WallSRequired)
	}
	fmt.Fprintln(stdout)
	if v.Error != "" {
		fmt.Fprintf(stdout, "reason:      %s\n", v.Error)
	}
	fmt.Fprintf(stdout, "artifacts:   %s\n", v.Artifacts)

	for i, vi := range v.Violations {
		fmt.Fprintf(stdout, "\nviolation[%d]: %s (%s, severity=%s, phase=%s, t+%dms)\n",
			i, vi.Oracle, vi.Class, vi.Severity, vi.Phase, vi.FirstSeenMS)
		if vi.Witness.Key != "" || len(vi.Witness.OpIDs) > 0 {
			fmt.Fprintf(stdout, "  witness:   key=%s op_ids=%v\n", vi.Witness.Key, vi.Witness.OpIDs)
		}
		fmt.Fprintf(stdout, "  explain:   %s\n", vi.Explanation)
		if len(vi.CausalTimeline) > 0 {
			fmt.Fprintln(stdout, "  timeline:")
			for _, ev := range vi.CausalTimeline {
				node := ""
				if ev.Node != "" {
					node = " [" + ev.Node + "]"
				}
				fmt.Fprintf(stdout, "    t+%4dms  %-6s%s  %s\n", ev.TMS, ev.Event, node, ev.Detail)
			}
		}
	}
	return nil
}

// ExecuteDemoRun enforces the test lock, executes the profile's worlds, evaluates oracles, and writes verdict artifacts.
func ExecuteDemoRun(ctx context.Context, opts DemoRunOptions) (*DemoRunSummary, error) {
	if opts.Stdout == nil {
		opts.Stdout = io.Discard
	}
	if opts.Stderr == nil {
		opts.Stderr = io.Discard
	}
	projectDir := opts.ProjectDir
	if projectDir == "" {
		projectDir = "."
	}
	absProject, err := filepath.Abs(projectDir)
	if err != nil {
		return &DemoRunSummary{ExitCode: schema.ExitConfigError}, err
	}
	projectDir = absProject

	cfgPath := filepath.Join(projectDir, "synthesis.yaml")
	rawCfg, err := os.ReadFile(cfgPath)
	if err != nil {
		return &DemoRunSummary{ExitCode: schema.ExitConfigError}, fmt.Errorf("read %s: %w", cfgPath, err)
	}

	// Step 1: Enforce tamper-evident lock BEFORE starting any container.
	rep, err := lock.Gate(lock.GateOptions{
		ProjectDir:  projectDir,
		ConfigPath:  cfgPath,
		ConfigBytes: rawCfg,
	})
	if err != nil {
		return &DemoRunSummary{ExitCode: schema.ExitInconclusive}, err
	}
	if rep.VerifyExitCode() != schema.ExitPass {
		fmt.Fprint(opts.Stderr, rep.Diagnosis())
		return &DemoRunSummary{
			ExitCode:   schema.ExitOracleDrift,
			LockReport: rep,
		}, nil
	}

	// Step 2: Decode and validate configuration and profile.
	cfg, err := schema.DecodeConfig(rawCfg)
	if err != nil {
		return &DemoRunSummary{ExitCode: schema.ExitConfigError, LockReport: rep}, err
	}
	profileName := opts.ProfileName
	if profileName == "" {
		profileName = "buggy"
	}
	prof, ok := cfg.Profiles[profileName]
	if !ok {
		return &DemoRunSummary{ExitCode: schema.ExitConfigError, LockReport: rep},
			fmt.Errorf("unknown profile %q in synthesis.yaml", profileName)
	}
	drvProf, ok := cfg.LoadDriver.Profiles[prof.DriverProfile]
	if !ok {
		return &DemoRunSummary{ExitCode: schema.ExitConfigError, LockReport: rep},
			fmt.Errorf("unknown driver_profile %q", prof.DriverProfile)
	}

	faultSpecs, err := validateProfileFaults(cfg, prof)
	if err != nil {
		return &DemoRunSummary{ExitCode: schema.ExitConfigError, LockReport: rep}, err
	}

	extOracles, err := discoverExternalOracles(projectDir, cfg.Oracles.Dir)
	if err != nil {
		return &DemoRunSummary{ExitCode: schema.ExitConfigError, LockReport: rep}, err
	}

	worldsRequired := *prof.Worlds
	wallSRequired := int64(prof.Budget.Seconds())
	worldsPlanned := worldsRequired
	wallDur := prof.Budget.Std()
	narrowed := false

	if opts.WorldsOverride > 0 {
		if opts.WorldsOverride < worldsRequired {
			narrowed = true
		}
		worldsPlanned = opts.WorldsOverride
	}
	if opts.BudgetOverride > 0 {
		if opts.BudgetOverride < prof.Budget.Std() {
			narrowed = true
		}
		wallDur = opts.BudgetOverride
	}
	wallSPlanned := int64(wallDur.Seconds())

	// Step 3: Allocate run directory under artifacts.dir.
	runsDir := cfg.Artifacts.Dir
	if runsDir == "" {
		runsDir = schema.DefaultArtifactsDir
	}
	if !filepath.IsAbs(runsDir) {
		runsDir = filepath.Join(projectDir, filepath.FromSlash(runsDir))
	}
	runStart := time.Now().UTC()
	runID, runDir, err := recorder.AllocRunID(runsDir, runStart)
	if err != nil {
		return &DemoRunSummary{ExitCode: schema.ExitInconclusive, LockReport: rep}, err
	}

	baseSeed := uint64(20261109)
	if opts.SeedSet {
		baseSeed = opts.Seed
	}

	runCtx, cancelRun := context.WithTimeout(ctx, wallDur)
	defer cancelRun()

	fmt.Fprintf(opts.Stdout, "synthesis run: id=%s profile=%s variant=%s worlds=%d lock=%s\n",
		runID, profileName, effectiveVariant(prof), worldsPlanned, rep.ManifestSHA[:19])

	worldsCompleted := 0
	for wIdx := 0; wIdx < worldsPlanned; wIdx++ {
		worldSeed := baseSeed + uint64(wIdx)
		worldDir := filepath.Join(runDir, fmt.Sprintf("world-%02d", wIdx+1))
		if err := os.MkdirAll(worldDir, 0o755); err != nil {
			return finalizeVerdict(runID, runDir, projectDir, profileName, baseSeed, rep,
				worldsPlanned, worldsCompleted, worldsRequired, wallSPlanned, wallSRequired,
				narrowed, runStart, schema.VerdictInconclusive, nil, err.Error(), opts.Stdout)
		}

		res := executeSingleWorld(runCtx, projectDir, cfg, prof, drvProf, faultSpecs, extOracles, runID, wIdx+1, worldSeed, worldDir, opts.Stdout)
		worldsCompleted++

		switch res.outcome {
		case schema.VerdictFail:
			return finalizeVerdict(runID, runDir, projectDir, profileName, baseSeed, rep,
				worldsPlanned, worldsCompleted, worldsRequired, wallSPlanned, wallSRequired,
				narrowed, runStart, schema.VerdictFail, res.violations, "", opts.Stdout)
		case schema.VerdictInconclusive:
			return finalizeVerdict(runID, runDir, projectDir, profileName, baseSeed, rep,
				worldsPlanned, worldsCompleted, worldsRequired, wallSPlanned, wallSRequired,
				narrowed, runStart, schema.VerdictInconclusive, nil, res.reason, opts.Stdout)
		case schema.VerdictBudgetExhausted:
			return finalizeVerdict(runID, runDir, projectDir, profileName, baseSeed, rep,
				worldsPlanned, worldsCompleted, worldsRequired, wallSPlanned, wallSRequired,
				narrowed, runStart, schema.VerdictBudgetExhausted, nil, res.reason, opts.Stdout)
		}
	}

	// A narrowed run can prove FAIL, never PASS.
	if narrowed {
		reason := fmt.Sprintf(
			"run narrowed below locked profile (%d/%d world(s), %ds/%ds budget); a narrowed run can prove FAIL, never PASS",
			worldsPlanned, worldsRequired, wallSPlanned, wallSRequired)
		return finalizeVerdict(runID, runDir, projectDir, profileName, baseSeed, rep,
			worldsPlanned, worldsCompleted, worldsRequired, wallSPlanned, wallSRequired,
			true, runStart, schema.VerdictInconclusive, nil, reason, opts.Stdout)
	}

	return finalizeVerdict(runID, runDir, projectDir, profileName, baseSeed, rep,
		worldsPlanned, worldsCompleted, worldsRequired, wallSPlanned, wallSRequired,
		false, runStart, schema.VerdictPass, nil, "", opts.Stdout)
}

func finalizeVerdict(
	runID, runDir, projectDir, profileName string,
	seed uint64,
	rep *lock.Report,
	worldsPlanned, worldsRun, worldsRequired int,
	wallSPlanned, wallSRequired int64,
	narrowed bool,
	runStart time.Time,
	outcome schema.VerdictResult,
	violations []schema.Violation,
	errReason string,
	stdout io.Writer,
) (*DemoRunSummary, error) {
	usedS := int64(time.Since(runStart).Seconds())
	if usedS < 0 {
		usedS = 0
	}
	commit := buildinfo.Commit
	if commit == "" {
		commit = "unstamped"
	}
	relArtifacts := filepath.ToSlash(runDir)
	if rel, err := filepath.Rel(projectDir, runDir); err == nil && !strings.HasPrefix(rel, "..") {
		relArtifacts = filepath.ToSlash(rel)
	}

	lockObj := rep.OracleLock()
	if narrowed && lockObj.Status == schema.LockOK {
		lockObj.Status = schema.LockBypassed
	}

	v := schema.NewVerdict(runID, profileName, outcome)
	v.Commit = commit
	v.Error = errReason
	v.Seed = schema.Uint64(seed)
	v.OracleLock = lockObj
	v.Artifacts = relArtifacts
	v.Budget = schema.Budget{
		WallS:         wallSPlanned,
		UsedS:         usedS,
		WorldsPlanned: worldsPlanned,
		WorldsRun:     worldsRun,
	}
	if narrowed {
		v.Budget.Narrowed = true
		v.Budget.WorldsRequired = worldsRequired
		v.Budget.WallSRequired = wallSRequired
	}
	if len(violations) > 0 {
		v.Violations = violations
	}
	v.Normalize()

	verdictPath, err := recorder.SaveVerdictJSON(runDir, &v)
	if err != nil {
		return &DemoRunSummary{ExitCode: schema.ExitInconclusive, LockReport: rep}, err
	}

	var svgPath string
	if outcome == schema.VerdictFail && len(v.Violations) > 0 {
		if svg, svgErr := RenderWitnessSVG(&v); svgErr == nil {
			p := filepath.Join(runDir, recorder.WitnessSVGFileName)
			if wErr := recorder.WriteFileAtomic(p, []byte(svg)); wErr == nil {
				svgPath = p
			}
		}
	}

	fmt.Fprintf(stdout, "verdict: %s (exit %d) | worlds=%d/%d | lock=%s | %s\n",
		v.Verdict, v.ExitCode(), worldsRun, worldsPlanned, v.OracleLock.Status, displayRelPath(projectDir, verdictPath))
	if v.Error != "" {
		fmt.Fprintf(stdout, "  reason: %s\n", v.Error)
	}
	if len(v.Violations) > 0 {
		fmt.Fprintf(stdout, "  violation: [%s] %s\n", v.Violations[0].Oracle, v.Violations[0].Explanation)
		if svgPath != "" {
			fmt.Fprintf(stdout, "  witness:   %s\n", displayRelPath(projectDir, svgPath))
		}
	}

	return &DemoRunSummary{
		ExitCode:       v.ExitCode(),
		Verdict:        &v,
		VerdictPath:    verdictPath,
		WitnessSVGPath: svgPath,
		LockReport:     rep,
	}, nil
}

func displayRelPath(projectDir, targetPath string) string {
	if cwd, err := os.Getwd(); err == nil {
		if rel, rerr := filepath.Rel(cwd, targetPath); rerr == nil && !strings.HasPrefix(rel, "..") {
			return filepath.ToSlash(rel)
		}
	}
	if rel, err := filepath.Rel(projectDir, targetPath); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.ToSlash(rel)
	}
	return filepath.ToSlash(filepath.Base(targetPath))
}

type worldExecutionResult struct {
	outcome    schema.VerdictResult
	violations []schema.Violation
	reason     string
}

func effectiveVariant(prof schema.Profile) string {
	if strings.TrimSpace(prof.Variant) != "" {
		return strings.TrimSpace(prof.Variant)
	}
	return "buggy"
}

func validateProfileFaults(cfg *schema.Config, prof schema.Profile) ([]schema.ScheduledFault, error) {
	if len(prof.Faults) > cfg.Perturber.Budget.MaxFaultsPerWorld {
		return nil, fmt.Errorf("profile schedules %d faults, exceeding perturber.budget.max_faults_per_world (%d)",
			len(prof.Faults), cfg.Perturber.Budget.MaxFaultsPerWorld)
	}
	allowed := map[schema.FaultKind]bool{}
	for _, k := range cfg.Perturber.Allow {
		allowed[k] = true
	}
	denied := map[schema.FaultKind]bool{}
	for _, k := range cfg.Perturber.Deny {
		denied[k] = true
	}

	specs := make([]schema.ScheduledFault, 0, len(prof.Faults))
	for _, raw := range prof.Faults {
		fs, err := schema.ParseFault(raw)
		if err != nil {
			return nil, err
		}
		if denied[fs.Kind] || !allowed[fs.Kind] {
			return nil, fmt.Errorf("fault kind %q is not permitted by perturber allow/deny policy", fs.Kind)
		}
		specs = append(specs, fs)
	}
	return specs, nil
}

func discoverExternalOracles(projectDir, oraclesDir string) ([]schema.OracleDef, error) {
	if oraclesDir == "" {
		oraclesDir = schema.DefaultOraclesDir
	}
	abs := oraclesDir
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(projectDir, filepath.FromSlash(oraclesDir))
	}
	info, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("oracles.dir %s is not a directory", abs)
	}

	var defs []schema.OracleDef
	err = filepath.WalkDir(abs, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(d.Name()))
		if ext != ".yaml" && ext != ".yml" {
			return nil
		}
		raw, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		def, derr := schema.DecodeOracleDef(raw)
		if derr != nil {
			return fmt.Errorf("%s: %w", p, derr)
		}
		defs = append(defs, *def)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(defs, func(i, j int) bool { return defs[i].Name < defs[j].Name })
	return defs, nil
}

func executeSingleWorld(
	ctx context.Context,
	projectDir string,
	cfg *schema.Config,
	prof schema.Profile,
	drvProf schema.DriverProfile,
	faultSpecs []schema.ScheduledFault,
	extOracles []schema.OracleDef,
	runID string,
	worldNum int,
	seed uint64,
	worldDir string,
	stdout io.Writer,
) worldExecutionResult {
	bootStart := time.Now()
	shortRun := strings.TrimPrefix(runID, "r_")
	shortRun = strings.ReplaceAll(shortRun, "_", "")
	prefix := fmt.Sprintf("syn_%s_w%d", shortRun, worldNum)
	variant := effectiveVariant(prof)

	cl, err := bootComposeCluster(ctx, projectDir, cfg, prefix, variant, seed)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return worldExecutionResult{outcome: schema.VerdictBudgetExhausted, reason: "wall budget exhausted during cluster boot"}
		}
		return worldExecutionResult{outcome: schema.VerdictInconclusive, reason: fmt.Sprintf("cluster boot failed: %v", err)}
	}
	defer func() { _ = cl.teardown(context.Background()) }()
	steadyEnd := time.Now()

	historyPath := filepath.Join(worldDir, "history.jsonl")
	finalStatePath := filepath.Join(worldDir, "final_state.json")
	telemetryPath := filepath.Join(worldDir, "telemetry.json")
	worldMarkerPath := filepath.Join(worldDir, "world.json")

	_ = os.WriteFile(telemetryPath, []byte("{}\n"), 0o644)
	_ = os.WriteFile(worldMarkerPath, []byte(fmt.Sprintf("{\"seed\":%d,\"variant\":%q}\n", seed, variant)), 0o644)

	// Start client load generator (DRIVE).
	drvCmd, drvStdin, drvStderr, err := startLoadDriver(ctx, projectDir, cfg.LoadDriver.Cmd, prof.DriverProfile, drvProf, historyPath, seed, cl.targetsCSV())
	if err != nil {
		return worldExecutionResult{outcome: schema.VerdictInconclusive, reason: fmt.Sprintf("driver launch failed: %v", err)}
	}
	driveStart := time.Now()

	drvDone := make(chan error, 1)
	go func() {
		drvDone <- drvCmd.Wait()
	}()

	type faultOutcome struct {
		rec *partitionRecord
		err error
	}
	faultCh := make(chan faultOutcome, len(faultSpecs))
	for _, spec := range faultSpecs {
		sp := spec
		go func() {
			rec, ferr := executePeerPartition(ctx, cl, sp, driveStart)
			faultCh <- faultOutcome{rec: rec, err: ferr}
		}()
	}

	// Determine target DRIVE duration so load spans the entire fault window plus post-heal reads/writes.
	driveTarget := 3500 * time.Millisecond
	for _, sp := range faultSpecs {
		need := time.Duration(sp.EndMS+1500) * time.Millisecond
		if need > driveTarget {
			driveTarget = need
		}
	}

	var driverErr error
	driverExitedEarly := false
	select {
	case <-ctx.Done():
		_, _ = io.WriteString(drvStdin, "{\"cmd\":\"stop\"}\n")
		_ = drvStdin.Close()
		return worldExecutionResult{outcome: schema.VerdictBudgetExhausted, reason: "wall budget exhausted during DRIVE"}
	case derr := <-drvDone:
		driverErr = derr
		driverExitedEarly = true
		_ = drvStdin.Close()
	case <-time.After(driveTarget):
		_, _ = io.WriteString(drvStdin, "{\"cmd\":\"stop\"}\n")
		_ = drvStdin.Close()
		select {
		case derr := <-drvDone:
			driverErr = derr
		case <-time.After(10 * time.Second):
			_ = drvCmd.Process.Kill()
			return worldExecutionResult{outcome: schema.VerdictInconclusive, reason: "driver did not stop within 10s of QUIESCE"}
		}
	}
	quiesceEnd := time.Now()

	var partRecords []*partitionRecord
	for i := 0; i < len(faultSpecs); i++ {
		fo := <-faultCh
		if fo.err != nil {
			return worldExecutionResult{outcome: schema.VerdictInconclusive, reason: fmt.Sprintf("fault injection failed: %v", fo.err)}
		}
		partRecords = append(partRecords, fo.rec)
	}

	if driverErr != nil {
		errText := strings.TrimSpace(drvStderr.String())
		if errText != "" {
			return worldExecutionResult{outcome: schema.VerdictInconclusive, reason: fmt.Sprintf("driver exited with error: %v (%s)", driverErr, errText)}
		}
		return worldExecutionResult{outcome: schema.VerdictInconclusive, reason: fmt.Sprintf("driver exited with error: %v", driverErr)}
	}
	_ = driverExitedEarly

	// Read history records to check AT-4 (load overlap with fault window) and built-in no_stuck_op.
	entries, err := readHistoryEntries(historyPath)
	if err != nil {
		return worldExecutionResult{outcome: schema.VerdictInconclusive, reason: fmt.Sprintf("unreadable history %s: %v", historyPath, err)}
	}
	if len(entries) == 0 {
		return worldExecutionResult{outcome: schema.VerdictInconclusive, reason: "driver produced an empty history; refusing vacuous PASS"}
	}

	// AT-4 guard: if a fault was scheduled, verify client operations were active during the fault window.
	for _, pr := range partRecords {
		overlapping := countOpsDuringFault(entries, pr.InjectNS, pr.WithdrawNS)
		if overlapping == 0 {
			lastOpMS := maxHistoryEventMS(entries, driveStart.UnixNano())
			return worldExecutionResult{
				outcome: schema.VerdictInconclusive,
				reason: fmt.Sprintf(
					"fault %s landed at t+%dms..%dms after client load had already ended at t+%dms (0 operations overlapped fault window)",
					pr.Spec, pr.InjectMS, pr.WithdrawMS, lastOpMS),
			}
		}
	}

	assertStart := time.Now()
	_ = cl.dumpFinalState(ctx, finalStatePath)

	// Evaluate built-in oracles configured in synthesis.yaml.
	var violations []schema.Violation
	oraclesEvaluated := 0
	logsCaptured := false

	for _, bo := range cfg.Oracles.Builtin {
		oraclesEvaluated++
		switch bo {
		case schema.BuiltinNoCrash:
			if v := cl.checkNoCrash(ctx); v != nil {
				violations = append(violations, *v)
			}
		case schema.BuiltinNoPanicLog:
			v, lerr := cl.captureLogsAndCheckNoPanic(ctx, filepath.Join(worldDir, "logs"))
			logsCaptured = true
			if lerr != nil {
				return worldExecutionResult{outcome: schema.VerdictInconclusive, reason: fmt.Sprintf("log capture failed: %v", lerr)}
			}
			if v != nil {
				violations = append(violations, *v)
			}
		case schema.BuiltinAvailabilityAfterHeal:
			if v := cl.checkAvailabilityAfterHeal(ctx); v != nil {
				violations = append(violations, *v)
			}
		case schema.BuiltinNoStuckOp:
			if v := checkNoStuckOp(entries, driveStart.UnixNano()); v != nil {
				violations = append(violations, *v)
			}
		}
	}
	if !logsCaptured {
		_, _ = cl.captureLogsAndCheckNoPanic(ctx, filepath.Join(worldDir, "logs"))
	}

	// Build PhaseTimings (milliseconds relative to DRIVE start = 0) for external oracles.
	assertNow := time.Now()
	relMS := func(t time.Time) int64 { return t.Sub(driveStart).Milliseconds() }
	phases := schema.PhaseTimings{
		{Phase: schema.PhaseBoot, StartMS: relMS(bootStart), EndMS: relMS(steadyEnd)},
		{Phase: schema.PhaseSeed, StartMS: relMS(steadyEnd), EndMS: 0},
		{Phase: schema.PhaseDrive, StartMS: 0, EndMS: relMS(quiesceEnd)},
		{Phase: schema.PhaseQuiesce, StartMS: relMS(quiesceEnd), EndMS: relMS(assertStart)},
		{Phase: schema.PhaseAssert, StartMS: relMS(assertStart), EndMS: relMS(assertNow)},
	}
	oracleIn := schema.NewOracleInput(historyPath, finalStatePath, telemetryPath, worldMarkerPath, phases)

	for _, def := range extOracles {
		oraclesEvaluated++
		status, outDoc, reason := runExternalOracle(ctx, projectDir, def, &oracleIn)
		switch status {
		case schema.StatusInconclusive:
			return worldExecutionResult{
				outcome: schema.VerdictInconclusive,
				reason:  fmt.Sprintf("oracle %s inconclusive: %s", def.Name, reason),
			}
		case schema.StatusViolated:
			firstMS, timeline := buildCausalTimeline(entries, partRecords, outDoc.Witness, driveStart.UnixNano())
			viol := outDoc.ToViolation(fmt.Sprintf("v_%s_w%02d", strings.ReplaceAll(def.Name, ".", "_"), worldNum), schema.PhaseDrive, firstMS)
			viol.CausalTimeline = timeline
			violations = append(violations, viol)
		case schema.StatusOK:
		}
	}

	if oraclesEvaluated == 0 {
		return worldExecutionResult{outcome: schema.VerdictInconclusive, reason: "no oracles configured; refusing vacuous PASS"}
	}

	fmt.Fprintf(stdout, "  world %d/%s: records=%d violations=%d\n", worldNum, variant, len(entries), len(violations))
	if len(violations) > 0 {
		return worldExecutionResult{outcome: schema.VerdictFail, violations: violations}
	}
	return worldExecutionResult{outcome: schema.VerdictPass}
}

func startLoadDriver(
	ctx context.Context,
	projectDir string,
	cmdTemplate string,
	profileName string,
	drvProf schema.DriverProfile,
	historyPath string,
	seed uint64,
	targetsCSV string,
) (*exec.Cmd, io.WriteCloser, *bytes.Buffer, error) {
	rawTokens := strings.Fields(cmdTemplate)
	if len(rawTokens) == 0 {
		return nil, nil, nil, errors.New("empty driver.cmd")
	}
	repl := strings.NewReplacer(
		schema.PlaceholderHistoryPath, historyPath,
		schema.PlaceholderSeed, strconv.FormatUint(seed, 10),
		schema.PlaceholderProfile, profileName,
		schema.PlaceholderPlanPath, "",
	)
	argv := make([]string, 0, len(rawTokens))
	for _, tok := range rawTokens {
		exp := repl.Replace(tok)
		if exp == "" {
			continue
		}
		argv = append(argv, exp)
	}
	if len(argv) == 0 {
		return nil, nil, nil, errors.New("empty driver.cmd after expansion")
	}
	prog := resolveExecInProject(projectDir, argv[0])
	args := append(argv[1:], "--stdin-control=true", "--quiet=true", "--targets="+targetsCSV)

	cmd := exec.CommandContext(ctx, prog, args...)
	cmd.Dir = projectDir

	readPermille := int(math.Round(drvProf.Mix["read"] * 1000.0))
	writePermille := int(math.Round(drvProf.Mix["write"] * 1000.0))
	txnPermille := int(math.Round(drvProf.Mix["txn"] * 1000.0))
	adminPermille := int(math.Round(drvProf.Mix["admin"] * 1000.0))

	cmd.Env = append(os.Environ(),
		"SYNTHESIS_TARGETS="+targetsCSV,
		"SYNTHESIS_STDIN_CONTROL=1",
		fmt.Sprintf("SYNTHESIS_CLIENTS=%d", drvProf.Clients),
		fmt.Sprintf("SYNTHESIS_OPS=%d", drvProf.Ops),
		fmt.Sprintf("SYNTHESIS_MIX_READ=%d", readPermille),
		fmt.Sprintf("SYNTHESIS_MIX_WRITE=%d", writePermille),
		fmt.Sprintf("SYNTHESIS_MIX_TXN=%d", txnPermille),
		fmt.Sprintf("SYNTHESIS_MIX_ADMIN=%d", adminPermille),
	)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return nil, nil, nil, fmt.Errorf("start %s: %w", prog, err)
	}
	return cmd, stdin, &stderrBuf, nil
}

func resolveExecInProject(projectDir, prog string) string {
	p := prog
	if !filepath.IsAbs(p) && (strings.ContainsRune(p, '/') || strings.ContainsRune(p, '\\')) {
		p = filepath.Join(projectDir, filepath.FromSlash(p))
	}
	if runtime.GOOS == "windows" && filepath.Ext(p) == "" {
		if _, err := os.Stat(p + ".exe"); err == nil {
			return p + ".exe"
		}
	}
	return p
}

func runExternalOracle(
	ctx context.Context,
	projectDir string,
	def schema.OracleDef,
	in *schema.OracleInput,
) (schema.OracleStatus, *schema.OracleOutput, string) {
	inBytes, err := schema.MarshalOracleInput(in)
	if err != nil {
		return schema.StatusInconclusive, nil, fmt.Sprintf("marshal oracle input: %v", err)
	}

	argv := strings.Fields(def.Cmd)
	if len(argv) == 0 {
		return schema.StatusInconclusive, nil, "empty oracle cmd"
	}
	prog := resolveExecInProject(projectDir, argv[0])

	timeout := def.Timeout.Std()
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	octx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(octx, prog, argv[1:]...)
	cmd.Dir = projectDir
	cmd.Stdin = bytes.NewReader(inBytes)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()
	exitCode := 0
	if runErr != nil {
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			exitCode = ee.ExitCode()
		} else {
			return schema.StatusInconclusive, nil, fmt.Sprintf("exec %s failed: %v (%s)",
				prog, runErr, strings.TrimSpace(stderr.String()))
		}
	}

	if _, parseErr := schema.ParseOracleOutput(stdout.Bytes()); parseErr != nil {
		return schema.StatusInconclusive, nil, fmt.Sprintf(
			"oracle exited %d without valid %s output (%v); stderr=%q",
			exitCode, schema.OracleOutputSchema, parseErr, strings.TrimSpace(stderr.String()))
	}

	out := schema.DecodeOracleOutput(stdout.Bytes(), exitCode)
	if err := out.Validate(); err != nil {
		return schema.StatusInconclusive, nil, fmt.Sprintf("oracle output failed validation: %v", err)
	}
	if out.Oracle != def.Name {
		return schema.StatusInconclusive, nil, fmt.Sprintf(
			"oracle name mismatch: definition=%q output=%q", def.Name, out.Oracle)
	}
	if !out.ValidIn(schema.PhaseAssert) {
		return schema.StatusInconclusive, nil, fmt.Sprintf("oracle %s does not declare ASSERT phase validity", def.Name)
	}
	if out.Status == schema.StatusInconclusive {
		return schema.StatusInconclusive, out, out.Explanation
	}
	return out.Status, out, ""
}

func readHistoryEntries(path string) ([]schema.HistoryEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return schema.ReadAllHistory(f)
}

func countOpsDuringFault(entries []schema.HistoryEntry, injectNS, withdrawNS int64) int {
	type span struct {
		inv int64
		ret int64
	}
	ops := map[int64]*span{}
	for _, e := range entries {
		if e.OpID == nil {
			continue
		}
		id := *e.OpID
		s, ok := ops[id]
		if !ok {
			s = &span{}
			ops[id] = s
		}
		if e.Type == schema.HistoryInvoke {
			s.inv = e.TNS
		} else {
			s.ret = e.TNS
		}
	}
	count := 0
	for _, s := range ops {
		if s.inv == 0 {
			continue
		}
		end := s.ret
		if end == 0 {
			end = s.inv
		}
		if s.inv <= withdrawNS && end >= injectNS {
			count++
		}
	}
	return count
}

func maxHistoryEventMS(entries []schema.HistoryEntry, driveStartNS int64) int64 {
	var maxNS int64
	for _, e := range entries {
		if e.TNS > maxNS {
			maxNS = e.TNS
		}
	}
	if maxNS <= driveStartNS {
		return 0
	}
	return (maxNS - driveStartNS) / 1e6
}

func checkNoStuckOp(entries []schema.HistoryEntry, driveStartNS int64) *schema.Violation {
	invoked := map[int64]int64{}
	completed := map[int64]bool{}
	for _, e := range entries {
		if e.OpID == nil {
			continue
		}
		id := *e.OpID
		if e.Type == schema.HistoryInvoke {
			invoked[id] = e.TNS
		} else {
			completed[id] = true
		}
	}
	for id, invNS := range invoked {
		if !completed[id] {
			ms := (invNS - driveStartNS) / 1e6
			if ms < 0 {
				ms = 0
			}
			return &schema.Violation{
				ID:          fmt.Sprintf("builtin_no_stuck_op_%d", id),
				Oracle:      string(schema.BuiltinNoStuckOp),
				Class:       schema.ClassLiveness,
				Severity:    schema.SeverityHigh,
				Phase:       schema.PhaseQuiesce,
				FirstSeenMS: ms,
				Explanation: fmt.Sprintf("operation op_id=%d was invoked and never recorded a completion", id),
			}
		}
	}
	return nil
}

func buildCausalTimeline(
	entries []schema.HistoryEntry,
	faults []*partitionRecord,
	witness schema.Witness,
	driveStartNS int64,
) (int64, []schema.TimelineEvent) {
	wanted := make(map[int64]bool, len(witness.OpIDs))
	for _, id := range witness.OpIDs {
		wanted[id] = true
	}

	var timeline []schema.TimelineEvent
	for _, pr := range faults {
		timeline = append(timeline,
			schema.TimelineEvent{
				TMS:    pr.InjectMS,
				Event:  schema.TimelineFault,
				Node:   pr.TargetNode,
				Detail: fmt.Sprintf("inject %s -> %s", pr.Spec, pr.TargetNode),
			},
			schema.TimelineEvent{
				TMS:    pr.WithdrawMS,
				Event:  schema.TimelineFault,
				Node:   pr.TargetNode,
				Detail: fmt.Sprintf("withdraw %s", pr.Spec),
			},
		)
	}

	writeVals := map[int64]string{}
	for _, e := range entries {
		if e.OpID != nil && e.Type == schema.HistoryInvoke && e.F == "write" && len(e.Value) > 0 {
			writeVals[*e.OpID] = compactToken(e.Value)
		}
	}

	var firstSeenMS int64 = -1
	for _, e := range entries {
		if e.OpID == nil || e.Process == nil || e.Key == nil {
			continue
		}
		id := *e.OpID
		if len(wanted) > 0 && !wanted[id] {
			continue
		}
		tms := (e.TNS - driveStartNS) / 1e6
		if tms < 0 {
			tms = 0
		}
		if e.Type != schema.HistoryInvoke && (firstSeenMS < 0 || tms < firstSeenMS) {
			firstSeenMS = tms
		}

		valTok := ""
		if len(e.Value) > 0 {
			valTok = compactToken(e.Value)
		} else if e.F == "write" {
			valTok = writeVals[id]
		}

		var detail string
		if e.Type == schema.HistoryInvoke {
			if e.F == "write" && valTok != "" {
				detail = fmt.Sprintf("process %d write %s -> %s [invoke op_id=%d]", *e.Process, *e.Key, valTok, id)
			} else {
				detail = fmt.Sprintf("process %d %s %s [invoke op_id=%d]", *e.Process, e.F, *e.Key, id)
			}
		} else {
			errSuffix := ""
			if e.Error != "" {
				errSuffix = " error=" + strings.ReplaceAll(e.Error, " ", "_")
			}
			if valTok != "" {
				detail = fmt.Sprintf("process %d %s %s -> %s [%s op_id=%d%s]",
					*e.Process, e.F, *e.Key, valTok, e.Type, id, errSuffix)
			} else {
				detail = fmt.Sprintf("process %d %s %s [%s op_id=%d%s]",
					*e.Process, e.F, *e.Key, e.Type, id, errSuffix)
			}
		}
		timeline = append(timeline, schema.TimelineEvent{
			TMS:    tms,
			Event:  schema.TimelineOp,
			Detail: detail,
		})
	}

	sort.SliceStable(timeline, func(i, j int) bool {
		return timeline[i].TMS < timeline[j].TMS
	})
	if firstSeenMS < 0 {
		firstSeenMS = 0
	}
	return firstSeenMS, timeline
}

func compactToken(raw json.RawMessage) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return strings.ReplaceAll(strings.TrimSpace(string(raw)), " ", "")
	}
	return strings.ReplaceAll(buf.String(), " ", "")
}
