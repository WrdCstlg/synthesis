package schema

import (
	"encoding/json"
	"fmt"

	"gopkg.in/yaml.v3"
)

// strictEnumJSON decodes a JSON string and checks closed-set membership.
func strictEnumJSON(b []byte, kind string, allowEmpty bool, ok func(string) bool) (string, error) {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return "", fmt.Errorf("schema: %s must be a string: %w", kind, err)
	}
	if s == "" && allowEmpty {
		return "", nil
	}
	if !ok(s) {
		return "", fmt.Errorf("schema: unknown %s %q", kind, s)
	}
	return s, nil
}

// strictEnumYAML decodes a YAML string scalar and checks closed-set membership.
func strictEnumYAML(n *yaml.Node, kind string, allowEmpty bool, ok func(string) bool) (string, error) {
	s, err := stringScalar(n, kind)
	if err != nil {
		return "", err
	}
	if s == "" && allowEmpty {
		return "", nil
	}
	if !ok(s) {
		return "", fmt.Errorf("%s: unknown value %q", kind, s)
	}
	return s, nil
}

// VerdictResult is the `verdict` field of synthesis.verdict/v1.
type VerdictResult string

const (
	VerdictPass            VerdictResult = "PASS"
	VerdictFail            VerdictResult = "FAIL"
	VerdictInconclusive    VerdictResult = "INCONCLUSIVE"
	VerdictBudgetExhausted VerdictResult = "BUDGET_EXHAUSTED"
	VerdictOracleDrift     VerdictResult = "ORACLE_DRIFT"
	VerdictConfigError     VerdictResult = "CONFIG_ERROR"
)

// AllVerdictResults lists every defined verdict in exit-code order.
var AllVerdictResults = [...]VerdictResult{
	VerdictPass, VerdictFail, VerdictInconclusive,
	VerdictBudgetExhausted, VerdictOracleDrift, VerdictConfigError,
}

func (v VerdictResult) Valid() bool {
	for _, x := range AllVerdictResults {
		if v == x {
			return true
		}
	}
	return false
}

// ExitCode maps each VerdictResult to its CLI exit code, or -1 if undefined.
func (v VerdictResult) ExitCode() ExitCode {
	switch v {
	case VerdictPass:
		return ExitPass
	case VerdictFail:
		return ExitFail
	case VerdictInconclusive:
		return ExitInconclusive
	case VerdictBudgetExhausted:
		return ExitBudgetExhausted
	case VerdictOracleDrift:
		return ExitOracleDrift
	case VerdictConfigError:
		return ExitConfigError
	}
	return -1
}

func (v *VerdictResult) UnmarshalJSON(b []byte) error {
	s, err := strictEnumJSON(b, "verdict", true, func(x string) bool { return VerdictResult(x).Valid() })
	if err != nil {
		return err
	}
	*v = VerdictResult(s)
	return nil
}

// OracleClass is the `class` field of synthesis.oracle_output/v1 and a verdict violation.
type OracleClass string

const (
	ClassCrash        OracleClass = "crash"
	ClassConsistency  OracleClass = "consistency"
	ClassLiveness     OracleClass = "liveness"
	ClassConvergence  OracleClass = "convergence"
	ClassResource     OracleClass = "resource"
	ClassSafety       OracleClass = "safety"
	ClassDifferential OracleClass = "differential"
	ClassMetamorphic  OracleClass = "metamorphic"
)

// AllOracleClasses lists every defined oracle class.
var AllOracleClasses = [...]OracleClass{
	ClassCrash, ClassConsistency, ClassLiveness, ClassConvergence,
	ClassResource, ClassSafety, ClassDifferential, ClassMetamorphic,
}

func (c OracleClass) Valid() bool {
	for _, x := range AllOracleClasses {
		if c == x {
			return true
		}
	}
	return false
}

func (c *OracleClass) UnmarshalJSON(b []byte) error {
	s, err := strictEnumJSON(b, "oracle class", true, func(x string) bool { return OracleClass(x).Valid() })
	if err != nil {
		return err
	}
	*c = OracleClass(s)
	return nil
}

func (c *OracleClass) UnmarshalYAML(n *yaml.Node) error {
	s, err := strictEnumYAML(n, "oracle class", true, func(x string) bool { return OracleClass(x).Valid() })
	if err != nil {
		return err
	}
	*c = OracleClass(s)
	return nil
}

// OracleStatus is the `status` field of synthesis.oracle_output/v1.
type OracleStatus string

const (
	StatusOK           OracleStatus = "ok"
	StatusViolated     OracleStatus = "violated"
	StatusInconclusive OracleStatus = "inconclusive"
)

// AllOracleStatuses lists every defined oracle status.
var AllOracleStatuses = [...]OracleStatus{StatusOK, StatusViolated, StatusInconclusive}

func (s OracleStatus) Valid() bool {
	return s == StatusOK || s == StatusViolated || s == StatusInconclusive
}

func (s OracleStatus) rank() int {
	switch s {
	case StatusViolated:
		return 2
	case StatusInconclusive:
		return 1
	case StatusOK:
		return 0
	}
	return 1
}

// Worse returns the more severe of two statuses for fail-closed reconciliation.
func (s OracleStatus) Worse(o OracleStatus) OracleStatus {
	if o.rank() > s.rank() {
		return o
	}
	return s
}

func (s *OracleStatus) UnmarshalJSON(b []byte) error {
	v, err := strictEnumJSON(b, "oracle status", true, func(x string) bool { return OracleStatus(x).Valid() })
	if err != nil {
		return err
	}
	*s = OracleStatus(v)
	return nil
}

// Severity is the verdict-level severity grade of a violation.
type Severity string

const (
	SeverityLow      Severity = "low"
	SeverityMedium   Severity = "medium"
	SeverityHigh     Severity = "high"
	SeverityCritical Severity = "critical"
)

// AllSeverities lists every defined severity, weakest first.
var AllSeverities = [...]Severity{SeverityLow, SeverityMedium, SeverityHigh, SeverityCritical}

func (s Severity) Valid() bool {
	for _, x := range AllSeverities {
		if s == x {
			return true
		}
	}
	return false
}

// DefaultSeverity assigns severity from OracleClass (fail-closed to high).
func DefaultSeverity(c OracleClass) Severity {
	if c == ClassResource {
		return SeverityMedium
	}
	return SeverityHigh
}

func (s *Severity) UnmarshalJSON(b []byte) error {
	v, err := strictEnumJSON(b, "severity", true, func(x string) bool { return Severity(x).Valid() })
	if err != nil {
		return err
	}
	*s = Severity(v)
	return nil
}

// HistoryType is the `type` field of a history log record.
type HistoryType string

const (
	HistoryInvoke HistoryType = "invoke"
	HistoryOK     HistoryType = "ok"
	HistoryFail   HistoryType = "fail"
	HistoryInfo   HistoryType = "info"
)

// AllHistoryTypes lists every defined history record type.
var AllHistoryTypes = [...]HistoryType{HistoryInvoke, HistoryOK, HistoryFail, HistoryInfo}

func (t HistoryType) Valid() bool {
	switch t {
	case HistoryInvoke, HistoryOK, HistoryFail, HistoryInfo:
		return true
	}
	return false
}

func (t *HistoryType) UnmarshalJSON(b []byte) error {
	v, err := strictEnumJSON(b, "history type", false, func(x string) bool { return HistoryType(x).Valid() })
	if err != nil {
		return err
	}
	*t = HistoryType(v)
	return nil
}

// LockStatus is `oracle_lock.status` in synthesis.verdict/v1.
type LockStatus string

const (
	LockOK       LockStatus = "ok"
	LockMismatch LockStatus = "mismatch"
	LockAbsent   LockStatus = "absent"
	LockBypassed LockStatus = "bypassed"
)

// AllLockStatuses lists every defined oracle lock status.
var AllLockStatuses = [...]LockStatus{LockOK, LockMismatch, LockAbsent, LockBypassed}

func (s LockStatus) Valid() bool {
	return s == LockOK || s == LockMismatch || s == LockAbsent || s == LockBypassed
}

func (s *LockStatus) UnmarshalJSON(b []byte) error {
	v, err := strictEnumJSON(b, "oracle lock status", true, func(x string) bool { return LockStatus(x).Valid() })
	if err != nil {
		return err
	}
	*s = LockStatus(v)
	return nil
}

// HarnessBackend is `harness.backend` in synthesis.yaml.
type HarnessBackend string

const (
	BackendCompose HarnessBackend = "compose"
	BackendProcess HarnessBackend = "process"
	BackendK8s     HarnessBackend = "k8s"
	BackendSim     HarnessBackend = "sim"
)

// AllBackends lists every defined backend spelling.
var AllBackends = [...]HarnessBackend{BackendCompose, BackendProcess, BackendK8s, BackendSim}

func (b HarnessBackend) Valid() bool {
	for _, x := range AllBackends {
		if b == x {
			return true
		}
	}
	return false
}

func (b HarnessBackend) ImplementedV1() bool {
	return b == BackendCompose || b == BackendProcess
}

func (b *HarnessBackend) UnmarshalYAML(n *yaml.Node) error {
	s, err := strictEnumYAML(n, "harness.backend", false, func(x string) bool { return HarnessBackend(x).Valid() })
	if err != nil {
		return fmt.Errorf("%w (want one of %v)", err, AllBackends)
	}
	*b = HarnessBackend(s)
	return nil
}

// BuiltinOracle is one entry of `oracles.builtin`.
type BuiltinOracle string

const (
	BuiltinNoCrash                  BuiltinOracle = "no_crash"
	BuiltinNoPanicLog               BuiltinOracle = "no_panic_log"
	BuiltinNoUnboundedQueue         BuiltinOracle = "no_unbounded_queue"
	BuiltinResourceReturnToBaseline BuiltinOracle = "resource_return_to_baseline"
	BuiltinAvailabilityAfterHeal    BuiltinOracle = "availability_after_heal"
	BuiltinNoStuckOp                BuiltinOracle = "no_stuck_op"
)

// AllBuiltinOracles lists every defined built-in oracle.
var AllBuiltinOracles = [...]BuiltinOracle{
	BuiltinNoCrash, BuiltinNoPanicLog, BuiltinNoUnboundedQueue,
	BuiltinResourceReturnToBaseline, BuiltinAvailabilityAfterHeal, BuiltinNoStuckOp,
}

func (o BuiltinOracle) Valid() bool {
	for _, x := range AllBuiltinOracles {
		if o == x {
			return true
		}
	}
	return false
}

// Class returns the oracle class under which the built-in reports findings.
func (o BuiltinOracle) Class() OracleClass {
	switch o {
	case BuiltinNoCrash, BuiltinNoPanicLog:
		return ClassCrash
	case BuiltinNoUnboundedQueue, BuiltinResourceReturnToBaseline:
		return ClassResource
	case BuiltinAvailabilityAfterHeal, BuiltinNoStuckOp:
		return ClassLiveness
	}
	return ""
}

// ValidPhases returns the lifecycle phases in which evaluating the built-in is meaningful.
func (o BuiltinOracle) ValidPhases() []Phase {
	switch o {
	case BuiltinNoUnboundedQueue, BuiltinResourceReturnToBaseline, BuiltinAvailabilityAfterHeal:
		return []Phase{PhaseQuiesce, PhaseAssert}
	case BuiltinNoStuckOp:
		return []Phase{PhaseHeal, PhaseQuiesce, PhaseAssert}
	case BuiltinNoCrash, BuiltinNoPanicLog:
		return append([]Phase(nil), AllPhases[:]...)
	}
	return nil
}

func (o *BuiltinOracle) UnmarshalYAML(n *yaml.Node) error {
	s, err := strictEnumYAML(n, "oracles.builtin", false, func(x string) bool { return BuiltinOracle(x).Valid() })
	if err != nil {
		return fmt.Errorf("%w (want one of %v)", err, AllBuiltinOracles)
	}
	*o = BuiltinOracle(s)
	return nil
}

// Role hints for harness.nodes[].role_hint.
const (
	RoleLeader   = "leader"
	RoleFollower = "follower"
	RoleReplica  = "replica"
	RoleStorage  = "storage"
	RoleProxy    = "proxy"
	RoleClient   = "client"
)

// History marker `event` values.
const (
	EventPhase = "phase"
	EventFault = "fault"
	EventNote  = "note"
)

// causal_timeline[].event values.
const (
	TimelineFault  = "fault"
	TimelineLog    = "log"
	TimelineOp     = "op"
	TimelinePhase  = "phase"
	TimelineOracle = "oracle"
)
