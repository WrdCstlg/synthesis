package schema

import (
	"bytes"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Compiled-in defaults for synthesis.yaml.
const (
	DefaultHarnessFile   = "docker-compose.yaml"
	DefaultHealthTimeout = 30 * time.Second
	DefaultSteadyTimeout = 60 * time.Second
	DefaultOraclesDir    = ".synthesis/oracles"
	DefaultArtifactsDir  = ".synthesis/runs"

	DefaultMaxConcurrentFaults = 1
	DefaultMaxFaultsPerWorld   = 4

	DefaultClientPort = 8080
	WorldsUnbounded   = -1
)

// Workload command placeholders.
const (
	PlaceholderHistoryPath = "{history_path}"
	PlaceholderSeed        = "{seed}"
	PlaceholderProfile     = "{profile}"
	PlaceholderPlanPath    = "{plan_path}"
)

var knownPlaceholders = map[string]bool{
	PlaceholderHistoryPath: true,
	PlaceholderSeed:        true,
	PlaceholderProfile:     true,
	PlaceholderPlanPath:    true,
}

var placeholderToken = regexp.MustCompile(`\{[a-z][a-z0-9_]*\}`)

// UnknownPlaceholders returns placeholder-shaped tokens in tmpl outside the known set.
func UnknownPlaceholders(tmpl string) []string {
	var out []string
	seen := map[string]bool{}
	for _, tok := range placeholderToken.FindAllString(tmpl, -1) {
		if knownPlaceholders[tok] || seen[tok] {
			continue
		}
		seen[tok] = true
		out = append(out, tok)
	}
	sort.Strings(out)
	return out
}

// Config is a decoded synthesis.yaml document for the public demonstration.
type Config struct {
	Version    string             `yaml:"version"`
	Name       string             `yaml:"name"`
	Harness    HarnessConfig      `yaml:"harness"`
	LoadDriver DriverConfig       `yaml:"driver"`
	Perturber  PerturberConfig    `yaml:"perturber"`
	Oracles    OraclesConfig      `yaml:"oracles"`
	Profiles   map[string]Profile `yaml:"profiles"`
	Artifacts  ArtifactsConfig    `yaml:"artifacts"`
}

// HarnessConfig configures the fixture's docker compose topology.
type HarnessConfig struct {
	BackendKind  HarnessBackend   `yaml:"backend"`
	File         string           `yaml:"file"`
	PrefixEnv    string           `yaml:"prefix_env,omitempty"`
	VariantEnv   string           `yaml:"variant_env,omitempty"`
	Nodes        []NodeConfig     `yaml:"nodes"`
	HealthChecks []HealthProbe    `yaml:"health"`
	SteadyState  SteadyStateProbe `yaml:"steady_state"`
}

// NodeConfig describes one logical node in harness.nodes.
type NodeConfig struct {
	ID             string `yaml:"id"`
	Service        string `yaml:"service"`
	ComposeService string `yaml:"compose_service,omitempty"`
	PortEnv        string `yaml:"port_env,omitempty"`
	Port           int    `yaml:"port,omitempty"`
	RoleHint       string `yaml:"role_hint"`
}

// ClientPort returns the in-container client HTTP port.
func (n NodeConfig) ClientPort() int {
	if n.Port > 0 {
		return n.Port
	}
	return DefaultClientPort
}

// EffectiveComposeService returns the compose service name backing this node.
func (n NodeConfig) EffectiveComposeService() string {
	if n.ComposeService != "" {
		return n.ComposeService
	}
	return n.Service
}

// HealthProbe configures a host-side HTTP health check.
type HealthProbe struct {
	Node      string   `yaml:"node"`
	ProbeSpec string   `yaml:"probe"`
	Timeout   Duration `yaml:"timeout"`
}

// SteadyStateProbe configures the cluster convergence check run before DRIVE.
type SteadyStateProbe struct {
	ProbeSpec string   `yaml:"probe"`
	Timeout   Duration `yaml:"timeout"`
}

// DriverConfig configures the client workload generator.
type DriverConfig struct {
	Cmd      string                   `yaml:"cmd"`
	Profiles map[string]DriverProfile `yaml:"profiles"`
}

// DriverProfile configures one named workload profile.
type DriverProfile struct {
	Clients int                `yaml:"clients"`
	Ops     int                `yaml:"ops"`
	Mix     map[string]float64 `yaml:"mix"`
}

// PerturberConfig configures fault bounds and allowed fault kinds.
type PerturberConfig struct {
	Budget      PerturberBudget `yaml:"budget"`
	Allow       []FaultKind     `yaml:"allow"`
	Deny        []FaultKind     `yaml:"deny"`
	Constraints []string        `yaml:"constraints"`
}

// PerturberBudget bounds injected faults per world.
type PerturberBudget struct {
	MaxConcurrentFaults int `yaml:"max_concurrent_faults"`
	MaxFaultsPerWorld   int `yaml:"max_faults_per_world"`
}

// OraclesConfig configures built-in and external oracle checks.
type OraclesConfig struct {
	Dir     string          `yaml:"dir"`
	Builtin []BuiltinOracle `yaml:"builtin"`
}

// Profile configures one named run profile in synthesis.yaml.
type Profile struct {
	Budget        Duration `yaml:"budget"`
	Worlds        *int     `yaml:"worlds"`
	Variant       string   `yaml:"variant,omitempty"`
	DriverProfile string   `yaml:"driver_profile"`
	Faults        []string `yaml:"faults,omitempty"`
}

// ArtifactsConfig configures where run artifacts are written.
type ArtifactsConfig struct {
	Dir string `yaml:"dir"`
}

// OracleDef is a decoded external oracle YAML file under .synthesis/oracles/.
type OracleDef struct {
	Version     string      `yaml:"version"`
	Name        string      `yaml:"name"`
	Class       OracleClass `yaml:"class"`
	ValidPhases []Phase     `yaml:"valid_phases"`
	Cmd         string      `yaml:"cmd"`
	Timeout     Duration    `yaml:"timeout"`
}

// DefaultConfig returns a Config initialized with standard defaults.
func DefaultConfig() Config {
	return Config{
		Harness: HarnessConfig{
			BackendKind: BackendCompose,
			File:        DefaultHarnessFile,
			SteadyState: SteadyStateProbe{Timeout: Duration(DefaultSteadyTimeout)},
		},
		Perturber: PerturberConfig{
			Budget: PerturberBudget{
				MaxConcurrentFaults: DefaultMaxConcurrentFaults,
				MaxFaultsPerWorld:   DefaultMaxFaultsPerWorld,
			},
		},
		Oracles:   OraclesConfig{Dir: DefaultOraclesDir},
		Artifacts: ArtifactsConfig{Dir: DefaultArtifactsDir},
	}
}

// DecodeConfig strictly decodes and validates a synthesis.yaml byte slice.
func DecodeConfig(data []byte) (*Config, error) {
	cfg := DefaultConfig()
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if cfg.Version != ConfigVersion {
		return nil, fmt.Errorf("config: version is %q, want %q", cfg.Version, ConfigVersion)
	}
	if strings.TrimSpace(cfg.Name) == "" {
		return nil, fmt.Errorf("config: name is required")
	}
	if cfg.Harness.BackendKind != BackendCompose {
		return nil, fmt.Errorf("config: demo supports backend %q only, got %q", BackendCompose, cfg.Harness.BackendKind)
	}
	if len(cfg.Harness.Nodes) == 0 {
		return nil, fmt.Errorf("config: harness.nodes must not be empty")
	}
	if strings.TrimSpace(cfg.LoadDriver.Cmd) == "" {
		return nil, fmt.Errorf("config: driver.cmd is required")
	}
	if unk := UnknownPlaceholders(cfg.LoadDriver.Cmd); len(unk) > 0 {
		return nil, fmt.Errorf("config: driver.cmd has unknown placeholder(s): %v", unk)
	}
	if len(cfg.Profiles) == 0 {
		return nil, fmt.Errorf("config: profiles must not be empty")
	}
	for name, p := range cfg.Profiles {
		if p.Budget <= 0 {
			return nil, fmt.Errorf("config: profiles.%s.budget must be positive", name)
		}
		if p.Worlds == nil || *p.Worlds <= 0 {
			return nil, fmt.Errorf("config: profiles.%s.worlds must be >= 1 in demo", name)
		}
		if _, ok := cfg.LoadDriver.Profiles[p.DriverProfile]; !ok {
			return nil, fmt.Errorf("config: profiles.%s references unknown driver_profile %q", name, p.DriverProfile)
		}
		for _, f := range p.Faults {
			if _, err := ParseFault(f); err != nil {
				return nil, fmt.Errorf("config: profiles.%s.faults: %w", name, err)
			}
		}
	}
	return &cfg, nil
}

// DecodeOracleDef strictly decodes and validates an external oracle YAML definition.
func DecodeOracleDef(data []byte) (*OracleDef, error) {
	var def OracleDef
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&def); err != nil {
		return nil, fmt.Errorf("oracle_def: %w", err)
	}
	if def.Version != OracleDefVersion {
		return nil, fmt.Errorf("oracle_def: version is %q, want %q", def.Version, OracleDefVersion)
	}
	if strings.TrimSpace(def.Name) == "" {
		return nil, fmt.Errorf("oracle_def: name is required")
	}
	if !def.Class.Valid() {
		return nil, fmt.Errorf("oracle_def %s: invalid class %q", def.Name, def.Class)
	}
	if len(def.ValidPhases) == 0 {
		return nil, fmt.Errorf("oracle_def %s: valid_phases must not be empty", def.Name)
	}
	if strings.TrimSpace(def.Cmd) == "" {
		return nil, fmt.Errorf("oracle_def %s: cmd is required", def.Name)
	}
	if def.Timeout <= 0 {
		def.Timeout = Duration(120 * time.Second)
	}
	return &def, nil
}
