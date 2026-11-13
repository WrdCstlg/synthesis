package demo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"synthesis.dev/synthesis/pkg/schema"
)

type nodeEndpoint struct {
	ID             string
	ComposeService string
	ContainerName  string
	PeerAlias      string
	HostPort       int
}

func (n nodeEndpoint) BaseURL() string {
	return fmt.Sprintf("http://127.0.0.1:%d", n.HostPort)
}

type composeCluster struct {
	ProjectDir  string
	ComposeFile string
	Prefix      string
	Variant     string
	Seed        uint64
	Nodes       []nodeEndpoint
	PeerNetwork string
	ClientNet   string
}

type healthPayload struct {
	OK      bool   `json:"ok"`
	Node    string `json:"node"`
	Variant string `json:"variant"`
}

type statusPayload struct {
	Node        string `json:"node"`
	Variant     string `json:"variant"`
	Role        string `json:"role"`
	Term        uint64 `json:"term"`
	LeaderID    string `json:"leader_id"`
	CommitIndex uint64 `json:"commit_index"`
	LastApplied uint64 `json:"last_applied"`
}

func allocateFreePorts(count int) ([]int, error) {
	listeners := make([]net.Listener, 0, count)
	ports := make([]int, 0, count)
	for i := 0; i < count; i++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			for _, l := range listeners {
				_ = l.Close()
			}
			return nil, fmt.Errorf("allocate free localhost port: %w", err)
		}
		listeners = append(listeners, ln)
		ports = append(ports, ln.Addr().(*net.TCPAddr).Port)
	}
	for _, l := range listeners {
		_ = l.Close()
	}
	return ports, nil
}

func peerAliasFor(nodeID string) string {
	short := strings.TrimPrefix(nodeID, "kv-")
	return short + "-peer"
}

func ensureVariantImage(ctx context.Context, projectDir, composeFile, variant string) error {
	imageName := "synthesis/kvfixture:" + variant
	inspect := exec.CommandContext(ctx, "docker", "image", "inspect", imageName)
	inspect.Dir = projectDir
	if err := inspect.Run(); err == nil {
		return nil
	}
	cmd := exec.CommandContext(ctx, "docker", "compose", "-f", composeFile, "build", "kv-n1")
	cmd.Dir = projectDir
	cmd.Env = append(os.Environ(), "KV_VARIANT="+variant)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker compose build (%s): %w\n%s", variant, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func bootComposeCluster(
	ctx context.Context,
	projectDir string,
	cfg *schema.Config,
	prefix string,
	variant string,
	seed uint64,
) (*composeCluster, error) {
	composeFile := cfg.Harness.File
	if composeFile == "" {
		composeFile = schema.DefaultHarnessFile
	}
	if !filepath.IsAbs(composeFile) {
		composeFile = filepath.Join(projectDir, composeFile)
	}
	if err := ensureVariantImage(ctx, projectDir, composeFile, variant); err != nil {
		return nil, err
	}

	ports, err := allocateFreePorts(len(cfg.Harness.Nodes))
	if err != nil {
		return nil, err
	}

	nodes := make([]nodeEndpoint, 0, len(cfg.Harness.Nodes))
	for i, n := range cfg.Harness.Nodes {
		svc := n.EffectiveComposeService()
		nodes = append(nodes, nodeEndpoint{
			ID:             n.ID,
			ComposeService: svc,
			ContainerName:  prefix + "-" + svc,
			PeerAlias:      peerAliasFor(n.ID),
			HostPort:       ports[i],
		})
	}

	cl := &composeCluster{
		ProjectDir:  projectDir,
		ComposeFile: composeFile,
		Prefix:      prefix,
		Variant:     variant,
		Seed:        seed,
		Nodes:       nodes,
		PeerNetwork: prefix + "-kvpeer",
		ClientNet:   prefix + "-kvclient",
	}

	// Ensure clean slate before starting.
	_ = cl.teardown(ctx)

	upCmd := exec.CommandContext(ctx, "docker", "compose", "-f", composeFile, "-p", prefix, "up", "-d", "--no-build")
	upCmd.Dir = projectDir
	upCmd.Env = cl.composeEnv()
	if out, err := upCmd.CombinedOutput(); err != nil {
		_ = cl.teardown(context.Background())
		return nil, fmt.Errorf("docker compose up: %w\n%s", err, strings.TrimSpace(string(out)))
	}

	healthTimeout := 30 * time.Second
	if len(cfg.Harness.HealthChecks) > 0 && cfg.Harness.HealthChecks[0].Timeout.Std() > 0 {
		healthTimeout = cfg.Harness.HealthChecks[0].Timeout.Std()
	}
	if err := cl.waitHealthy(ctx, healthTimeout); err != nil {
		_ = cl.teardown(context.Background())
		return nil, err
	}

	steadyTimeout := cfg.Harness.SteadyState.Timeout.Std()
	if steadyTimeout <= 0 {
		steadyTimeout = 45 * time.Second
	}
	if err := cl.waitSteadyState(ctx, steadyTimeout); err != nil {
		_ = cl.teardown(context.Background())
		return nil, err
	}
	return cl, nil
}

func (c *composeCluster) composeEnv() []string {
	env := append([]string(nil), os.Environ()...)
	env = append(env,
		"KV_PREFIX="+c.Prefix,
		"KV_VARIANT="+c.Variant,
		"KV_SEED="+strconv.FormatUint(c.Seed, 10),
	)
	for i, n := range c.Nodes {
		env = append(env, fmt.Sprintf("KV_PORT_N%d=%d", i+1, n.HostPort))
	}
	return env
}

func (c *composeCluster) targetsCSV() string {
	parts := make([]string, 0, len(c.Nodes))
	for _, n := range c.Nodes {
		parts = append(parts, fmt.Sprintf("127.0.0.1:%d", n.HostPort))
	}
	return strings.Join(parts, ",")
}

func (c *composeCluster) teardown(ctx context.Context) error {
	tctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(tctx, "docker", "compose", "-f", c.ComposeFile, "-p", c.Prefix, "down", "-v", "--remove-orphans")
	cmd.Dir = c.ProjectDir
	cmd.Env = c.composeEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker compose down (%s): %w\n%s", c.Prefix, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (c *composeCluster) waitHealthy(ctx context.Context, timeout time.Duration) error {
	client := &http.Client{Timeout: 1500 * time.Millisecond}
	deadline := time.Now().Add(timeout)

	for _, n := range c.Nodes {
		url := n.BaseURL() + "/healthz"
		for {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			hp, err := getHealth(ctx, client, url)
			if err == nil && hp.OK {
				if hp.Variant != c.Variant {
					return fmt.Errorf("node %s reported compiled variant %q, expected %q",
						n.ID, hp.Variant, c.Variant)
				}
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("health timeout waiting for %s (%s): last err=%v", n.ID, url, err)
			}
			time.Sleep(150 * time.Millisecond)
		}
	}
	return nil
}

func getHealth(ctx context.Context, client *http.Client, url string) (*healthPayload, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	var hp healthPayload
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&hp); err != nil {
		return nil, err
	}
	return &hp, nil
}

func (c *composeCluster) waitSteadyState(ctx context.Context, timeout time.Duration) error {
	sctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Run the fixture's internal cluster-wide convergence probe from kv-n1.
	inClusterTargets := make([]string, 0, len(c.Nodes))
	for _, n := range c.Nodes {
		inClusterTargets = append(inClusterTargets, fmt.Sprintf("http://%s:8080", n.ComposeService))
	}
	probeDur := timeout - 2*time.Second
	if probeDur < 5*time.Second {
		probeDur = 5 * time.Second
	}
	cmd := exec.CommandContext(
		sctx,
		"docker", "exec", c.Nodes[0].ContainerName,
		"/kv", "steady",
		"--targets", strings.Join(inClusterTargets, ","),
		"--timeout", fmt.Sprintf("%ds", int(probeDur.Seconds())),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("cluster steady-state check failed: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

type containerInspectState struct {
	RestartCount int `json:"RestartCount"`
	State        struct {
		Running   bool `json:"Running"`
		OOMKilled bool `json:"OOMKilled"`
		ExitCode  int  `json:"ExitCode"`
	} `json:"State"`
}

func (c *composeCluster) checkNoCrash(ctx context.Context) *schema.Violation {
	for _, n := range c.Nodes {
		cmd := exec.CommandContext(ctx, "docker", "inspect", n.ContainerName)
		out, err := cmd.Output()
		if err != nil {
			return &schema.Violation{
				ID:          "builtin_no_crash_" + n.ID,
				Oracle:      string(schema.BuiltinNoCrash),
				Class:       schema.ClassCrash,
				Severity:    schema.SeverityHigh,
				Phase:       schema.PhaseAssert,
				Explanation: fmt.Sprintf("container %s could not be inspected: %v", n.ContainerName, err),
			}
		}
		var states []containerInspectState
		if err := json.Unmarshal(out, &states); err != nil || len(states) == 0 {
			return &schema.Violation{
				ID:          "builtin_no_crash_" + n.ID,
				Oracle:      string(schema.BuiltinNoCrash),
				Class:       schema.ClassCrash,
				Severity:    schema.SeverityHigh,
				Phase:       schema.PhaseAssert,
				Explanation: fmt.Sprintf("unexpected inspect payload for %s", n.ContainerName),
			}
		}
		st := states[0]
		if !st.State.Running || st.State.OOMKilled || st.State.ExitCode != 0 || st.RestartCount > 0 {
			return &schema.Violation{
				ID:       "builtin_no_crash_" + n.ID,
				Oracle:   string(schema.BuiltinNoCrash),
				Class:    schema.ClassCrash,
				Severity: schema.SeverityHigh,
				Phase:    schema.PhaseAssert,
				Explanation: fmt.Sprintf("node %s container state running=%v exit=%d oom=%v restarts=%d",
					n.ID, st.State.Running, st.State.ExitCode, st.State.OOMKilled, st.RestartCount),
			}
		}
	}
	return nil
}

func (c *composeCluster) captureLogsAndCheckNoPanic(ctx context.Context, logsDir string) (*schema.Violation, error) {
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		return nil, err
	}
	for _, n := range c.Nodes {
		cmd := exec.CommandContext(ctx, "docker", "logs", n.ContainerName)
		var buf bytes.Buffer
		cmd.Stdout = &buf
		cmd.Stderr = &buf
		_ = cmd.Run()
		data := buf.Bytes()
		if err := os.WriteFile(filepath.Join(logsDir, n.ID+".log"), data, 0o644); err != nil {
			return nil, err
		}
		text := string(data)
		for _, needle := range []string{"panic:", "fatal error:", "DATA RACE"} {
			if strings.Contains(text, needle) {
				return &schema.Violation{
					ID:          "builtin_no_panic_" + n.ID,
					Oracle:      string(schema.BuiltinNoPanicLog),
					Class:       schema.ClassCrash,
					Severity:    schema.SeverityHigh,
					Phase:       schema.PhaseAssert,
					Explanation: fmt.Sprintf("node %s log contained %q", n.ID, needle),
				}, nil
			}
		}
	}
	return nil, nil
}

func (c *composeCluster) checkAvailabilityAfterHeal(ctx context.Context) *schema.Violation {
	if err := c.waitSteadyState(ctx, 20*time.Second); err != nil {
		return &schema.Violation{
			ID:          "builtin_availability_after_heal",
			Oracle:      string(schema.BuiltinAvailabilityAfterHeal),
			Class:       schema.ClassLiveness,
			Severity:    schema.SeverityHigh,
			Phase:       schema.PhaseAssert,
			Explanation: fmt.Sprintf("cluster failed to converge after fault withdrawal: %v", err),
		}
	}
	client := &http.Client{Timeout: 2 * time.Second}
	for _, n := range c.Nodes {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, n.BaseURL()+"/readyz", nil)
		resp, err := client.Do(req)
		if err != nil {
			return &schema.Violation{
				ID:          "builtin_availability_" + n.ID,
				Oracle:      string(schema.BuiltinAvailabilityAfterHeal),
				Class:       schema.ClassLiveness,
				Severity:    schema.SeverityHigh,
				Phase:       schema.PhaseAssert,
				Explanation: fmt.Sprintf("node %s /readyz unreachable after heal: %v", n.ID, err),
			}
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return &schema.Violation{
				ID:          "builtin_availability_" + n.ID,
				Oracle:      string(schema.BuiltinAvailabilityAfterHeal),
				Class:       schema.ClassLiveness,
				Severity:    schema.SeverityHigh,
				Phase:       schema.PhaseAssert,
				Explanation: fmt.Sprintf("node %s /readyz returned HTTP %d after heal", n.ID, resp.StatusCode),
			}
		}
	}
	return nil
}

func (c *composeCluster) dumpFinalState(ctx context.Context, destPath string) error {
	client := &http.Client{Timeout: 2 * time.Second}
	dumps := make(map[string]json.RawMessage, len(c.Nodes))
	for _, n := range c.Nodes {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, n.BaseURL()+"/dump", nil)
		if err != nil {
			continue
		}
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusOK && json.Valid(body) {
			dumps[n.ID] = json.RawMessage(body)
		}
	}
	raw, err := json.MarshalIndent(dumps, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return os.WriteFile(destPath, raw, 0o644)
}
