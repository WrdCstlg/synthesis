package schema

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
)

// Verdict is the minimal prothesis.verdict/v1 document emitted by synthesis.
// Only fields produced by the public demonstration are included.
type Verdict struct {
	Schema     string        `json:"schema"`
	RunID      string        `json:"run_id"`
	Profile    string        `json:"profile"`
	Commit     string        `json:"commit"`
	Verdict    VerdictResult `json:"verdict"`
	Error      string        `json:"error,omitempty"`
	Budget     Budget        `json:"budget"`
	Violations []Violation   `json:"violations"`
	OracleLock OracleLock    `json:"oracle_lock"`
	Artifacts  string        `json:"artifacts"`
	Seed       *uint64       `json:"seed,omitempty"`
}

// Budget records the run's wall-clock and world budget consumption.
type Budget struct {
	WallS          int64 `json:"wall_s"`
	UsedS          int64 `json:"used_s"`
	WorldsPlanned  int   `json:"worlds_planned"`
	WorldsRun      int   `json:"worlds_run"`
	Narrowed       bool  `json:"narrowed,omitempty"`
	WorldsRequired int   `json:"worlds_required,omitempty"`
	WallSRequired  int64 `json:"wall_s_required,omitempty"`
}

// Violation is one oracle violation recorded in a Verdict.
type Violation struct {
	ID             string          `json:"id"`
	Oracle         string          `json:"oracle"`
	Class          OracleClass     `json:"class"`
	Severity       Severity        `json:"severity"`
	Phase          Phase           `json:"phase"`
	FirstSeenMS    int64           `json:"first_seen_ms"`
	Explanation    string          `json:"explanation"`
	Witness        Witness         `json:"witness"`
	CausalTimeline []TimelineEvent `json:"causal_timeline"`
}

// TimelineEvent is one row of causal_timeline, relative to DRIVE start (t_ms = 0).
type TimelineEvent struct {
	TMS    int64  `json:"t_ms"`
	Event  string `json:"event"`
	Node   string `json:"node,omitempty"`
	Detail string `json:"detail"`
}

// OracleLock records the lock verification status and manifest digest.
type OracleLock struct {
	Status           LockStatus `json:"status"`
	ManifestSHA      string     `json:"manifest_sha"`
	ExecutablesMoved []string   `json:"executables_moved,omitempty"`
}

// Witness holds the operation IDs and key demonstrating a consistency violation.
type Witness struct {
	OpIDs []int64
	Key   string
	Extra map[string]json.RawMessage
}

const (
	witnessOpIDs = "op_ids"
	witnessKey   = "key"
)

// IsEmpty reports whether the witness carries no fields.
func (w Witness) IsEmpty() bool {
	return len(w.OpIDs) == 0 && w.Key == "" && len(w.Extra) == 0
}

// MarshalJSON emits only present witness members so an empty witness encodes as {}.
func (w Witness) MarshalJSON() ([]byte, error) {
	m := make(map[string]json.RawMessage, len(w.Extra)+2)
	for k, v := range w.Extra {
		if k == witnessOpIDs || k == witnessKey {
			continue
		}
		m[k] = v
	}
	if w.OpIDs != nil {
		b, err := marshalNoEscape(w.OpIDs)
		if err != nil {
			return nil, err
		}
		m[witnessOpIDs] = b
	}
	if w.Key != "" {
		b, err := marshalNoEscape(w.Key)
		if err != nil {
			return nil, err
		}
		m[witnessKey] = b
	}
	return marshalNoEscape(m)
}

// UnmarshalJSON strictly decodes op_ids and key while preserving extra oracle fields.
func (w *Witness) UnmarshalJSON(b []byte) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return fmt.Errorf("witness: must be an object: %w", err)
	}
	*w = Witness{}
	for k, v := range m {
		switch k {
		case witnessOpIDs:
			if err := json.Unmarshal(v, &w.OpIDs); err != nil {
				return fmt.Errorf("witness.op_ids: must be an array of integers: %w", err)
			}
		case witnessKey:
			if err := json.Unmarshal(v, &w.Key); err != nil {
				return fmt.Errorf("witness.key: must be a string: %w", err)
			}
		default:
			if w.Extra == nil {
				w.Extra = make(map[string]json.RawMessage, len(m))
			}
			w.Extra[k] = v
		}
	}
	return nil
}

// NewVerdict returns a normalized Verdict for a run.
func NewVerdict(runID, profile string, result VerdictResult) Verdict {
	v := Verdict{
		Schema:  VerdictSchema,
		RunID:   runID,
		Profile: profile,
		Verdict: result,
	}
	v.Normalize()
	return v
}

// Normalize ensures non-null slices and fail-closed lock status defaults.
func (v *Verdict) Normalize() {
	v.Schema = VerdictSchema
	if v.Violations == nil {
		v.Violations = []Violation{}
	}
	for i := range v.Violations {
		v.Violations[i].normalize()
	}
	sort.SliceStable(v.Violations, func(i, j int) bool {
		return v.Violations[i].ID < v.Violations[j].ID
	})
	if v.OracleLock.Status == "" {
		v.OracleLock.Status = LockAbsent
	}
}

func (vi *Violation) normalize() {
	if vi.Severity == "" {
		vi.Severity = DefaultSeverity(vi.Class)
	}
	if vi.CausalTimeline == nil {
		vi.CausalTimeline = []TimelineEvent{}
	}
}

// Validate checks internal invariants of a Verdict document.
func (v *Verdict) Validate() error {
	var errs ValidationErrors
	if v.Schema != VerdictSchema {
		errs.Add("schema", "is %q, want %q", v.Schema, VerdictSchema)
	}
	if v.RunID == "" {
		errs.Add("run_id", "missing")
	} else if !ValidRunID(v.RunID) {
		errs.Add("run_id", "%q is not a run id (want %s)", v.RunID, RunIDShape)
	}
	if !v.Verdict.Valid() {
		errs.Add("verdict", "unknown verdict %q (want one of %v)", v.Verdict, AllVerdictResults)
	}
	if v.Violations == nil {
		errs.Add("violations", "is null; an empty verdict carries []")
	}
	seen := map[string]int{}
	for i, vi := range v.Violations {
		p := indexPath("violations", i)
		if vi.ID == "" {
			errs.Add(p+".id", "missing")
		} else if j, dup := seen[vi.ID]; dup {
			errs.Add(p+".id", "duplicate id %q (already used at violations[%d])", vi.ID, j)
		} else {
			seen[vi.ID] = i
		}
		if vi.Oracle == "" {
			errs.Add(p+".oracle", "missing")
		}
		if vi.Class != "" && !vi.Class.Valid() {
			errs.Add(p+".class", "unknown class %q (want one of %v)", vi.Class, AllOracleClasses)
		}
		if !vi.Severity.Valid() {
			errs.Add(p+".severity", "unknown severity %q (want one of %v)", vi.Severity, AllSeverities)
		}
		if vi.Phase != "" && !vi.Phase.Valid() {
			errs.Add(p+".phase", "unknown phase %q (want one of %v, or \"\" for unscoped)", vi.Phase, AllPhases)
		}
		if vi.CausalTimeline == nil {
			errs.Add(p+".causal_timeline", "is null; write []")
		}
	}
	if !v.OracleLock.Status.Valid() {
		errs.Add("oracle_lock.status", "unknown status %q (want one of %v)", v.OracleLock.Status, AllLockStatuses)
	}
	if v.Budget.UsedS < 0 || v.Budget.WallS < 0 {
		errs.Add("budget", "wall_s and used_s must not be negative")
	}
	return errs.OrNil()
}

// ExitCode returns the CLI exit code corresponding to the verdict.
func (v *Verdict) ExitCode() ExitCode { return v.Verdict.ExitCode() }

// MarshalVerdict normalizes and formats the verdict as indented JSON with a trailing LF.
func MarshalVerdict(v *Verdict) ([]byte, error) {
	v.Normalize()
	b, err := marshalNoEscape(v)
	if err != nil {
		return nil, fmt.Errorf("verdict: encode: %w", err)
	}
	return indentJSON(b)
}

// WriteVerdict writes a normalized, indented Verdict JSON document to w.
func WriteVerdict(w io.Writer, v *Verdict) error {
	b, err := MarshalVerdict(v)
	if err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

// UnmarshalVerdict decodes a Verdict JSON document.
func UnmarshalVerdict(data []byte) (*Verdict, error) {
	var v Verdict
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, fmt.Errorf("verdict: decode: %w", err)
	}
	if v.Schema != VerdictSchema {
		return nil, fmt.Errorf("verdict: schema is %q, want %q", v.Schema, VerdictSchema)
	}
	return &v, nil
}
