package lock

import (
	"bytes"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// CoveredPaths are the synthesis.yaml paths digested by the lock, in sorted order.
var CoveredPaths = []string{
	"driver.cmd",
	"harness.file",
	"harness.health",
	"harness.nodes",
	"harness.role_probe",
	"harness.steady_state",
	"harness.variant_env",
	"oracles.builtin",
	"oracles.dir",
	"perturber.allow",
	"perturber.budget",
	"perturber.constraints",
	"perturber.deny",
	"profiles",
}

const maxProjectionDepth = 64

// DriverProfilesPath is the config section covered by reference.
const DriverProfilesPath = "driver.profiles"

// CoveredByReference describes driver profiles digested by reference from profiles.*.driver_profile.
const CoveredByReference = "driver.profiles.<name>, for every <name> a profiles.*.driver_profile names"

// ProjectConfig flattens the covered subset of raw synthesis.yaml bytes into sorted leaf entries.
func ProjectConfig(data []byte) ([]ConfigEntry, error) {
	out := []ConfigEntry{}
	if len(bytes.TrimSpace(data)) == 0 {
		return out, nil
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("lock: parse synthesis.yaml: %w", err)
	}
	root := resolveAlias(&doc, 0)
	if root == nil {
		return out, nil
	}
	if root.Kind == yaml.DocumentNode {
		if len(root.Content) == 0 {
			return out, nil
		}
		root = resolveAlias(root.Content[0], 0)
	}
	if root == nil || root.Kind != yaml.MappingNode {
		return out, nil
	}

	for _, p := range CoveredPaths {
		n := lookup(root, strings.Split(p, "."), 0)
		if n == nil {
			continue
		}
		if err := flatten(n, p, &out, 0); err != nil {
			return nil, err
		}
	}
	for _, name := range referencedDriverProfiles(root) {
		n := lookup(root, []string{"driver", "profiles", name}, 0)
		if n == nil {
			continue
		}
		if err := flatten(n, joinPath(DriverProfilesPath, name), &out, 0); err != nil {
			return nil, err
		}
	}
	sortEntries(out)
	return out, nil
}

func referencedDriverProfiles(root *yaml.Node) []string {
	profiles := lookup(root, []string{"profiles"}, 0)
	if profiles == nil || profiles.Kind != yaml.MappingNode {
		return nil
	}
	seen := map[string]bool{}
	for i := 0; i+1 < len(profiles.Content); i += 2 {
		ref := lookup(profiles.Content[i+1], []string{"driver_profile"}, 1)
		if ref == nil || ref.Kind != yaml.ScalarNode || ref.Tag == "!!null" || ref.Value == "" {
			continue
		}
		seen[ref.Value] = true
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func sortEntries(e []ConfigEntry) {
	sort.Slice(e, func(i, j int) bool {
		if e[i].Path != e[j].Path {
			return e[i].Path < e[j].Path
		}
		if e[i].Kind != e[j].Kind {
			return e[i].Kind < e[j].Kind
		}
		return e[i].Value < e[j].Value
	})
}

func lookup(n *yaml.Node, segs []string, depth int) *yaml.Node {
	if depth > maxProjectionDepth {
		return nil
	}
	n = resolveAlias(n, depth)
	if n == nil || len(segs) == 0 {
		return n
	}
	if n.Kind != yaml.MappingNode {
		return nil
	}
	var found *yaml.Node
	for i := 0; i+1 < len(n.Content); i += 2 {
		k := resolveAlias(n.Content[i], depth)
		if k == nil || k.Value != segs[0] {
			continue
		}
		found = n.Content[i+1]
	}
	if found == nil {
		return nil
	}
	return lookup(found, segs[1:], depth+1)
}

func flatten(n *yaml.Node, path string, out *[]ConfigEntry, depth int) error {
	if depth > maxProjectionDepth {
		return fmt.Errorf("lock: synthesis.yaml nests deeper than %d at %s", maxProjectionDepth, path)
	}
	n = resolveAlias(n, depth)
	if n == nil {
		return nil
	}
	switch n.Kind {
	case yaml.ScalarNode:
		if n.Tag == "!!null" {
			*out = append(*out, ConfigEntry{Path: path, Kind: KindNull})
			return nil
		}
		*out = append(*out, ConfigEntry{Path: path, Kind: KindScalar, Value: n.Value})
		return nil

	case yaml.MappingNode:
		if len(n.Content) == 0 {
			*out = append(*out, ConfigEntry{Path: path, Kind: KindEmptyMap})
			return nil
		}
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := resolveAlias(n.Content[i], depth)
			if k == nil {
				continue
			}
			if err := flatten(n.Content[i+1], joinPath(path, k.Value), out, depth+1); err != nil {
				return err
			}
		}
		return nil

	case yaml.SequenceNode:
		if len(n.Content) == 0 {
			*out = append(*out, ConfigEntry{Path: path, Kind: KindEmptySeq})
			return nil
		}
		allScalar := true
		for _, c := range n.Content {
			if r := resolveAlias(c, depth); r == nil || r.Kind != yaml.ScalarNode {
				allScalar = false
				break
			}
		}
		if allScalar {
			for _, c := range n.Content {
				r := resolveAlias(c, depth)
				if r.Tag == "!!null" {
					*out = append(*out, ConfigEntry{Path: path, Kind: KindNull})
					continue
				}
				*out = append(*out, ConfigEntry{Path: path, Kind: KindScalar, Value: r.Value})
			}
			return nil
		}
		for i, c := range n.Content {
			if err := flatten(c, path+"["+strconv.Itoa(i)+"]", out, depth+1); err != nil {
				return err
			}
		}
		return nil

	case yaml.DocumentNode:
		if len(n.Content) == 0 {
			return nil
		}
		return flatten(n.Content[0], path, out, depth+1)

	default:
		return fmt.Errorf("lock: %s: unsupported YAML node kind %d", path, n.Kind)
	}
}

func resolveAlias(n *yaml.Node, depth int) *yaml.Node {
	for i := 0; n != nil && n.Kind == yaml.AliasNode; i++ {
		if depth+i > maxProjectionDepth {
			return nil
		}
		n = n.Alias
	}
	return n
}

func joinPath(base, key string) string {
	if !plainKey(key) {
		key = strconv.Quote(key)
	}
	if base == "" {
		return key
	}
	return base + "." + key
}

func plainKey(k string) bool {
	if k == "" {
		return false
	}
	for i := 0; i < len(k); i++ {
		c := k[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '_' || c == '-':
		default:
			return false
		}
	}
	return true
}
