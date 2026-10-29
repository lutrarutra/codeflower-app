package sandbox

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// Bind mounts a host path into the sandbox.
type Bind struct {
	Source   string
	Target   string
	ReadOnly bool
}

// ParseBind parses SOURCE[:TARGET[:ro|rw]]. A bind without a target maps to
// /data/input/NAME, and binds are read-only unless :rw is given. A Windows source may
// start with a drive letter (C:\data).
func ParseBind(value string) (Bind, error) {
	drive := ""
	if len(value) >= 3 && value[1] == ':' && (value[2] == '\\' || value[2] == '/') && isLetter(value[0]) {
		drive, value = value[:2], value[2:]
	}
	parts := strings.Split(value, ":")
	parts[0] = drive + parts[0]
	target, readOnly := InputsRoot+"/"+folderName(parts[0]), true
	switch len(parts) {
	case 1:
	case 2:
		target = parts[1]
	case 3:
		target = parts[1]
		switch parts[2] {
		case "ro":
		case "rw":
			readOnly = false
		default:
			return Bind{}, fmt.Errorf("bind mode must be 'ro' or 'rw'")
		}
	default:
		return Bind{}, fmt.Errorf("bind must be SOURCE[:TARGET[:ro|rw]]")
	}
	return NewBind(parts[0], target, readOnly)
}

func isLetter(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// NewBind checks a bind: the source must exist (a folder under /data/input,
// /data/output, /data/code and /data/skills) and the target must be an absolute path
// outside the project state.
func NewBind(source, target string, readOnly bool) (Bind, error) {
	if source == "" || !strings.HasPrefix(target, "/") {
		return Bind{}, fmt.Errorf("bind must have an existing source and an absolute container target")
	}
	for _, part := range strings.Split(target, "/") {
		if part == ".." {
			return Bind{}, fmt.Errorf("bind target must not contain '..'")
		}
	}
	target = path.Clean(target)
	if target == ContainerState || strings.HasPrefix(target, ContainerState+"/") {
		return Bind{}, fmt.Errorf("bind target is reserved for instance state")
	}
	if target == ContainerNet || strings.HasPrefix(target, ContainerNet+"/") {
		return Bind{}, fmt.Errorf("bind target is reserved for the sandbox's network")
	}

	resolved, err := filepath.Abs(expandHome(source))
	if err != nil {
		return Bind{}, err
	}
	if real, err := filepath.EvalSymlinks(resolved); err == nil {
		resolved = real
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return Bind{}, fmt.Errorf("bind source does not exist: %s", resolved)
	}
	if folderTarget(target) && !info.IsDir() {
		return Bind{}, fmt.Errorf("%s must be a folder: %s", target, resolved)
	}
	if strings.Contains(strings.TrimPrefix(resolved, filepath.VolumeName(resolved)), ":") {
		return Bind{}, fmt.Errorf("bind source paths must not contain ':'")
	}
	return Bind{Source: resolved, Target: target, ReadOnly: readOnly}, nil
}

// Spec renders the bind for Apptainer and Docker: SOURCE:TARGET[:ro].
func (b Bind) Spec() string {
	if b.ReadOnly {
		return b.Source + ":" + b.Target + ":ro"
	}
	return b.Source + ":" + b.Target
}

func folderTarget(target string) bool {
	for _, root := range []string{InputsRoot, OutputsRoot, CodeRoot, SkillsRoot} {
		if strings.HasPrefix(target, root+"/") {
			return true
		}
	}
	return false
}

// Folders are the user's input, output, code and skills folders.
type Folders struct {
	Inputs  []string
	Outputs []string
	Code    []string
	Skills  []string
}

// Binds mounts each folder in a subfolder of its root named after it: inputs
// and skills read-only, outputs (created if needed) and code read-write. Targets already
// used by taken are skipped, and each folder may be chosen only once.
func (f Folders) Binds(taken []Bind) ([]Bind, error) {
	used := map[string]bool{}
	for _, bind := range taken {
		used[bind.Target] = true
	}
	chosen := map[string]string{}
	var binds []Bind
	for _, group := range []struct {
		kind     string
		root     string
		sources  []string
		readOnly bool
	}{
		{"input", InputsRoot, f.Inputs, true},
		{"output", OutputsRoot, f.Outputs, false},
		{"code", CodeRoot, f.Code, false},
		{"skills", SkillsRoot, f.Skills, true},
	} {
		for _, source := range group.sources {
			if group.kind == "output" {
				if err := os.MkdirAll(expandHome(source), 0o755); err != nil {
					return nil, fmt.Errorf("output folder: %w", err)
				}
			}
			name := folderName(source)
			target := group.root + "/" + name
			for number := 2; used[target]; number++ {
				target = fmt.Sprintf("%s/%s-%d", group.root, name, number)
			}
			bind, err := NewBind(source, target, group.readOnly)
			if err != nil {
				return nil, fmt.Errorf("%s folder: %w", group.kind, err)
			}
			if other, ok := chosen[bind.Source]; ok {
				return nil, fmt.Errorf("%s is chosen as %s %s folder and as %s %s folder; choose each folder once",
					bind.Source, article(other), other, article(group.kind), group.kind)
			}
			chosen[bind.Source] = group.kind
			used[target] = true
			binds = append(binds, bind)
		}
	}
	return binds, nil
}

func article(kind string) string {
	if kind == "input" || kind == "output" {
		return "an"
	}
	return "a"
}

var unsafeNameCharacters = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// folderName is the folder's own name, made safe for a path in the sandbox.
func folderName(source string) string {
	name := filepath.Base(filepath.Clean(expandHome(source)))
	name = strings.TrimLeft(unsafeNameCharacters.ReplaceAllString(name, "_"), ".")
	if strings.Trim(name, "_") == "" {
		return "folder"
	}
	return name
}

func checkTargets(binds []Bind) error {
	// The roots hold one subfolder per user folder; mounting over one would hide them.
	occupied := map[string]bool{
		ContainerState: true, ContainerNet: true, "/": true, "/data": true,
		InputsRoot: true, OutputsRoot: true, CodeRoot: true, SkillsRoot: true,
	}
	for _, bind := range binds {
		if occupied[bind.Target] {
			return fmt.Errorf("duplicate or reserved bind target: %s", bind.Target)
		}
		occupied[bind.Target] = true
	}
	return nil
}

func expandHome(value string) string {
	if value == "~" || strings.HasPrefix(value, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return home + value[1:]
		}
	}
	return value
}
