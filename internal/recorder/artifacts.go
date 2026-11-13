package recorder

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"synthesis.dev/synthesis/pkg/schema"
)

const (
	// StateDirName is the per-project state directory.
	StateDirName = ".synthesis"
	// RunsDirName holds one directory per run under StateDirName.
	RunsDirName = "runs"
	// VerdictFileName is the run verdict JSON document.
	VerdictFileName = "verdict.json"
	// WitnessSVGFileName is the rendered SVG witness timeline for a FAIL verdict.
	WitnessSVGFileName = "witness.svg"
)

const allocRunIDAttempts = 64

// StateDir returns the .synthesis directory for a project root.
func StateDir(projectDir string) string {
	return filepath.Join(projectDir, StateDirName)
}

// RunsDir returns the runs directory for a project root.
func RunsDir(projectDir string) string {
	return filepath.Join(StateDir(projectDir), RunsDirName)
}

// ValidRunID reports whether s matches the normative run id format.
func ValidRunID(s string) bool {
	return schema.ValidRunID(s)
}

// AllocRunID creates a fresh run directory under runsDir and returns its id and path.
func AllocRunID(runsDir string, wallUTC time.Time) (runID, dir string, err error) {
	if err := os.MkdirAll(runsDir, 0o755); err != nil {
		return "", "", fmt.Errorf("recorder: create runs dir %s: %w", runsDir, err)
	}
	y, m, d := wallUTC.UTC().Date()
	prefix := fmt.Sprintf("r_%04d_%02d_%02d_", y, int(m), d)
	var buf [2]byte
	for i := 0; i < allocRunIDAttempts; i++ {
		if _, err := rand.Read(buf[:]); err != nil {
			return "", "", fmt.Errorf("recorder: read entropy for run id: %w", err)
		}
		id := prefix + hex.EncodeToString(buf[:])
		p := filepath.Join(runsDir, id)
		err := os.Mkdir(p, 0o755)
		if err == nil {
			return id, p, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", "", fmt.Errorf("recorder: create run dir %s: %w", p, err)
		}
	}
	return "", "", fmt.Errorf("recorder: could not allocate a free run id under %s after %d attempts",
		runsDir, allocRunIDAttempts)
}

var safeNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.\-]{1,64}$`)

var windowsReserved = map[string]bool{
	"con": true, "prn": true, "aux": true, "nul": true,
	"com1": true, "com2": true, "com3": true, "com4": true, "com5": true,
	"com6": true, "com7": true, "com8": true, "com9": true,
	"lpt1": true, "lpt2": true, "lpt3": true, "lpt4": true, "lpt5": true,
	"lpt6": true, "lpt7": true, "lpt8": true, "lpt9": true,
}

// SafeName validates an identifier for use as a path component across platforms.
func SafeName(s string) error {
	if !safeNamePattern.MatchString(s) {
		return fmt.Errorf("recorder: %q is not a safe path component (want 1-64 chars from A-Z a-z 0-9 _ . -)", s)
	}
	if strings.HasSuffix(s, ".") || strings.HasSuffix(s, " ") {
		return fmt.Errorf("recorder: %q ends with a dot or space", s)
	}
	base := strings.ToLower(s)
	if i := strings.IndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	if windowsReserved[base] {
		return fmt.Errorf("recorder: %q is a reserved Windows device name", s)
	}
	return nil
}

// WriteFileAtomic writes data to a sibling temporary file, fsyncs, and renames into place.
func WriteFileAtomic(dest string, data []byte) error {
	dir := filepath.Dir(dest)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("recorder: mkdir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("recorder: create temp in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("recorder: write %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("recorder: sync %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("recorder: close %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, dest); err != nil {
		cleanup()
		return fmt.Errorf("recorder: rename %s -> %s: %w", tmpName, dest, err)
	}
	return nil
}

// SaveVerdictJSON validates, formats, and atomically writes verdict.json into runDir.
func SaveVerdictJSON(runDir string, v *schema.Verdict) (string, error) {
	v.Normalize()
	if err := v.Validate(); err != nil {
		return "", fmt.Errorf("recorder: invalid verdict: %w", err)
	}
	data, err := schema.MarshalVerdict(v)
	if err != nil {
		return "", err
	}
	p := filepath.Join(runDir, VerdictFileName)
	if err := WriteFileAtomic(p, data); err != nil {
		return "", err
	}
	return p, nil
}
