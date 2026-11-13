package schema

import (
	"fmt"
	"strconv"
	"strings"
)

// FaultKind names a fault primitive recognized by synthesis.yaml.
// The public demonstration implements only the network-partition primitive.
type FaultKind string

const (
	FaultNetPartition FaultKind = "net.partition"
)

// AllFaultKinds lists supported fault kinds in the public demonstration.
var AllFaultKinds = [...]FaultKind{FaultNetPartition}

// Valid reports whether k is a supported demo fault kind.
func (k FaultKind) Valid() bool {
	return k == FaultNetPartition
}

// ScheduledFault represents one fixed fault window in milliseconds from DRIVE start:
//
//	net.partition(role:leader)@3000..7500
//	net.partition(kv-n1)@3000..7500
type ScheduledFault struct {
	Kind    FaultKind
	Target  string
	StartMS int64
	EndMS   int64
	Raw     string
}

// String renders the canonical fault specification.
func (f ScheduledFault) String() string {
	return fmt.Sprintf("%s(%s)@%d..%d", f.Kind, f.Target, f.StartMS, f.EndMS)
}

// ParseFault parses a scheduled partition expression of the form
// `net.partition(<target>)@<start_ms>..<end_ms>`.
func ParseFault(raw string) (ScheduledFault, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ScheduledFault{}, fmt.Errorf("fault: empty specification")
	}
	at := strings.LastIndexByte(s, '@')
	if at <= 0 {
		return ScheduledFault{}, fmt.Errorf("fault %q: missing @start..end window", raw)
	}
	head, win := strings.TrimSpace(s[:at]), strings.TrimSpace(s[at+1:])
	open := strings.IndexByte(head, '(')
	if open <= 0 || !strings.HasSuffix(head, ")") {
		return ScheduledFault{}, fmt.Errorf("fault %q: expected kind(target)@start..end", raw)
	}
	kind := FaultKind(strings.TrimSpace(head[:open]))
	if !kind.Valid() {
		return ScheduledFault{}, fmt.Errorf("fault %q: unsupported kind %q (demo supports %q only)", raw, kind, FaultNetPartition)
	}
	target := strings.TrimSpace(head[open+1 : len(head)-1])
	if target == "" {
		return ScheduledFault{}, fmt.Errorf("fault %q: empty target", raw)
	}
	dots := strings.Index(win, "..")
	if dots <= 0 {
		return ScheduledFault{}, fmt.Errorf("fault %q: window %q must be start_ms..end_ms", raw, win)
	}
	startMS, err := strconv.ParseInt(strings.TrimSpace(win[:dots]), 10, 64)
	if err != nil || startMS < 0 {
		return ScheduledFault{}, fmt.Errorf("fault %q: invalid start_ms in %q", raw, win)
	}
	endMS, err := strconv.ParseInt(strings.TrimSpace(win[dots+2:]), 10, 64)
	if err != nil || endMS <= startMS {
		return ScheduledFault{}, fmt.Errorf("fault %q: end_ms must exceed start_ms in %q", raw, win)
	}
	return ScheduledFault{
		Kind:    kind,
		Target:  target,
		StartMS: startMS,
		EndMS:   endMS,
		Raw:     s,
	}, nil
}
