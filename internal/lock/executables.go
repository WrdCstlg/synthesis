package lock

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"synthesis.dev/synthesis/pkg/schema"
)

// ExecutableFingerprint records what an external oracle or driver command resolved to at lock time.
type ExecutableFingerprint struct {
	Oracle     string `json:"oracle"`
	Cmd        string `json:"cmd"`
	Resolved   string `json:"resolved"`
	SHA256     string `json:"sha256,omitempty"`
	Size       int64  `json:"size,omitempty"`
	Unresolved string `json:"unresolved,omitempty"`
}

// DriverProgram identifies the driver command entry in Executables.
const DriverProgram = "driver.cmd"

// FingerprintExecutables hashes the resolved program of every external oracle declared under oraclesDir.
func FingerprintExecutables(projectDir, oraclesDir string) ([]ExecutableFingerprint, error) {
	if oraclesDir == "" {
		return nil, nil
	}
	dir := oraclesDir
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(projectDir, dir)
	}
	info, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("lock: stat oracle definitions in %s: %w", dir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("lock: %s is not a directory", dir)
	}

	var out []ExecutableFingerprint
	walkErr := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(d.Name()))
		if ext != ".yaml" && ext != ".yml" {
			return nil
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		def, err := schema.DecodeOracleDef(raw)
		if err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		out = append(out, fingerprintProgram(projectDir, def.Name, def.Cmd))
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("lock: discover oracle definitions in %s: %w", dir, walkErr)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Oracle < out[j].Oracle })
	return out, nil
}

// FingerprintDriver hashes the program resolved from driver.cmd.
func FingerprintDriver(projectDir, cmd string) ExecutableFingerprint {
	return fingerprintProgram(projectDir, DriverProgram, cmd)
}

func fingerprintProgram(projectDir, name, cmd string) ExecutableFingerprint {
	fp := ExecutableFingerprint{Oracle: name, Cmd: cmd}

	argv, err := splitCommandLine(cmd)
	if err != nil || len(argv) == 0 || argv[0] == "" {
		fp.Unresolved = "the cmd could not be split into an argv"
		return fp
	}

	resolved := resolveProgramPath(argv[0], projectDir)
	fp.Resolved = relativeToProject(projectDir, resolved)

	info, err := os.Stat(resolved)
	switch {
	case err != nil:
		fp.Unresolved = fmt.Sprintf("no file at %s (a PATH lookup or an unbuilt program resolves at run time)", fp.Resolved)
		return fp
	case info.IsDir():
		fp.Unresolved = fmt.Sprintf("%s is a directory", fp.Resolved)
		return fp
	}

	sum, size, err := hashFile(resolved)
	if err != nil {
		fp.Unresolved = fmt.Sprintf("could not read %s: %v", fp.Resolved, err)
		return fp
	}
	fp.SHA256, fp.Size = sum, size
	return fp
}

func splitCommandLine(cmd string) ([]string, error) {
	var args []string
	var cur strings.Builder
	inSingle := false
	inDouble := false
	escaped := false
	started := false

	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		if escaped {
			cur.WriteByte(c)
			escaped = false
			started = true
			continue
		}
		if c == '\\' && !inSingle {
			// Preserve backslashes inside unquoted Windows paths unless escaping quotes/spaces.
			if i+1 < len(cmd) && (cmd[i+1] == '"' || cmd[i+1] == ' ' || cmd[i+1] == '\t') {
				escaped = true
				continue
			}
		}
		if c == '\'' && !inDouble {
			inSingle = !inSingle
			started = true
			continue
		}
		if c == '"' && !inSingle {
			inDouble = !inDouble
			started = true
			continue
		}
		if (c == ' ' || c == '\t') && !inSingle && !inDouble {
			if started {
				args = append(args, cur.String())
				cur.Reset()
				started = false
			}
			continue
		}
		cur.WriteByte(c)
		started = true
	}
	if inSingle || inDouble || escaped {
		return nil, fmt.Errorf("unterminated quote or escape in command %q", cmd)
	}
	if started {
		args = append(args, cur.String())
	}
	return args, nil
}

func resolveProgramPath(prog, projectDir string) string {
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

func relativeToProject(projectDir, p string) string {
	if projectDir == "" {
		return filepath.ToSlash(p)
	}
	rel, err := filepath.Rel(projectDir, p)
	if err != nil || strings.HasPrefix(rel, "..") {
		return filepath.ToSlash(p)
	}
	return filepath.ToSlash(rel)
}

// ExecutableChange records one program whose digest moved relative to the lock.
type ExecutableChange struct {
	Oracle string
	Was    string
	Now    string
	Detail string
}

// DiffExecutables compares locked executable fingerprints against current measurements.
func DiffExecutables(locked, now []ExecutableFingerprint) []ExecutableChange {
	if len(locked) == 0 {
		return nil
	}
	byName := make(map[string]ExecutableFingerprint, len(now))
	for _, f := range now {
		byName[f.Oracle] = f
	}

	var out []ExecutableChange
	for _, was := range locked {
		is, present := byName[was.Oracle]
		switch {
		case !present:
			out = append(out, ExecutableChange{
				Oracle: was.Oracle, Was: was.SHA256,
				Detail: "the oracle is no longer declared, so its program was not checked",
			})
		case was.SHA256 == "" && is.SHA256 == "":
		case was.SHA256 == "":
			out = append(out, ExecutableChange{
				Oracle: was.Oracle, Now: is.SHA256,
				Detail: fmt.Sprintf("could not be fingerprinted at lock time (%s) and now resolves to %s",
					was.Unresolved, is.Resolved),
			})
		case is.SHA256 == "":
			out = append(out, ExecutableChange{
				Oracle: was.Oracle, Was: was.SHA256,
				Detail: fmt.Sprintf("the program recorded at %s is no longer readable: %s",
					was.Resolved, is.Unresolved),
			})
		case was.SHA256 != is.SHA256:
			out = append(out, ExecutableChange{
				Oracle: was.Oracle, Was: was.SHA256, Now: is.SHA256,
				Detail: fmt.Sprintf("%s changed (%s -> %s)", is.Resolved,
					shortSum(was.SHA256), shortSum(is.SHA256)),
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Oracle < out[j].Oracle })
	return out
}

func shortSum(s string) string {
	s = strings.TrimPrefix(s, "sha256:")
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// FormatExecutableDrift formats fatal same-platform executable drift.
func FormatExecutableDrift(changes []ExecutableChange, platform string) string {
	if len(changes) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "synthesis: ORACLE DRIFT: %d program(s) the verdict depends on differ from the lock on %s:\n",
		len(changes), platform)
	for _, c := range changes {
		fmt.Fprintf(&b, "  %s: %s\n", c.Oracle, c.Detail)
	}
	return b.String()
}

// FormatExecutableChanges formats cross-platform executable warnings.
func FormatExecutableChanges(changes []ExecutableChange) string {
	if len(changes) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "synthesis: WARNING: %d program(s) the verdict depends on differ from the lock:\n", len(changes))
	for _, c := range changes {
		fmt.Fprintf(&b, "  %s: %s\n", c.Oracle, c.Detail)
	}
	return b.String()
}
