package config

import (
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// SparseModeCone is the only sparse mode treehouse supports. Git's cone mode
// stores a minimal directory set: a listed directory implies its descendants.
const SparseModeCone = "cone"

// maxSparseFileBytes bounds a checked-in cone list. A list larger than this is
// far more likely to be a mistake than a real selection.
const maxSparseFileBytes = 1 << 20

// Profile is one named sparse-checkout selection plus optional environment.
// It is declarative data: no command, hook, or executable field is allowed.
type Profile struct {
	// Sparse is a boolean or an array of repo-relative directories. false
	// selects a full checkout; true requires directories from SparseFile or
	// the array. Absent with no SparseFile also means a full checkout.
	Sparse SparseSelection `toml:"sparse,omitempty"`
	// SparseFile names a checked-in file with one repo-relative directory per
	// line. It is unioned with the inline Sparse directories.
	SparseFile *string `toml:"sparse_file,omitempty"`
	// SparseMode is "" or "cone". Any other value is rejected.
	SparseMode string `toml:"sparse_mode,omitempty"`
	// Env is applied only when --profile names this profile explicitly; a
	// repo default may select the sparse data but never injects environment.
	Env map[string]string `toml:"env,omitempty"`
}

// SparseSelection records the spelling of the `sparse` key. Present and
// Disabled distinguish "not declared" from an explicit `sparse = false`.
type SparseSelection struct {
	Present  bool
	Disabled bool
	Dirs     []string
}

// UnmarshalTOML accepts a boolean or an array of strings, which TOML cannot
// express as one statically typed field.
func (s *SparseSelection) UnmarshalTOML(value any) error {
	switch v := value.(type) {
	case bool:
		s.Present = true
		s.Disabled = !v
	case []any:
		s.Present = true
		for i, item := range v {
			str, ok := item.(string)
			if !ok {
				return fmt.Errorf("sparse[%d] must be a string", i)
			}
			s.Dirs = append(s.Dirs, str)
		}
	case []string:
		s.Present = true
		s.Dirs = append(s.Dirs, v...)
	default:
		return fmt.Errorf("sparse must be a boolean or an array of strings, got %T", value)
	}
	return nil
}

// ResolvedProfile is a profile ready to hand to the pool. Dirs are sorted,
// de-duplicated, and reduced to Git's minimal cone set.
type ResolvedProfile struct {
	Name     string
	Explicit bool
	Full     bool
	Dirs     []string
	// Env is populated only when Explicit is true.
	Env map[string]string
	// EnvDeclared reports that the profile declares env even when Env was
	// suppressed because the profile came from a repo default.
	EnvDeclared bool
}

// ResolveProfile selects and validates the profile for one acquisition.
// explicit is the --profile value, empty when the flag was not passed. It
// returns nil when no profile is selected, which leaves the slot's sparse
// state untouched and preserves behavior for repositories without profiles.
func ResolveProfile(repoRoot string, cfg Config, explicit string) (*ResolvedProfile, error) {
	name := explicit
	isExplicit := explicit != ""
	if !isExplicit {
		name = cfg.DefaultProfile
	}
	if name == "" {
		return nil, nil
	}

	profile, ok := cfg.Profiles[name]
	if !ok {
		return nil, fmt.Errorf("profile %q is not defined (known profiles: %s)", name, knownProfiles(cfg))
	}

	resolved := &ResolvedProfile{Name: name, Explicit: isExplicit}
	switch profile.SparseMode {
	case "", SparseModeCone:
	default:
		return nil, fmt.Errorf("profile %q: sparse_mode %q is not supported; only %q is supported", name, profile.SparseMode, SparseModeCone)
	}

	if profile.Sparse.Disabled {
		if profile.SparseFile != nil {
			return nil, fmt.Errorf("profile %q: sparse_file cannot be set when sparse = false", name)
		}
		if profile.SparseMode != "" {
			return nil, fmt.Errorf("profile %q: sparse_mode cannot be set when sparse = false", name)
		}
		resolved.Full = true
	} else {
		raw := append([]string(nil), profile.Sparse.Dirs...)
		if profile.SparseFile != nil {
			lines, err := readSparseFile(repoRoot, name, *profile.SparseFile)
			if err != nil {
				return nil, err
			}
			raw = append(raw, lines...)
		}
		if len(raw) == 0 {
			if profile.Sparse.Present {
				return nil, fmt.Errorf("profile %q selects no directories; use sparse = false for a full checkout", name)
			}
			if profile.SparseMode != "" {
				return nil, fmt.Errorf("profile %q: sparse_mode cannot be set without sparse directories", name)
			}
			resolved.Full = true
		} else {
			dirs, err := normalizeSparseDirs(raw, name)
			if err != nil {
				return nil, err
			}
			resolved.Dirs = dirs
		}
	}

	if err := validateProfileEnv(name, profile.Env); err != nil {
		return nil, err
	}
	resolved.EnvDeclared = len(profile.Env) > 0
	if isExplicit {
		resolved.Env = profile.Env
	}
	return resolved, nil
}

func knownProfiles(cfg Config) string {
	names := make([]string, 0, len(cfg.Profiles))
	for name := range cfg.Profiles {
		names = append(names, name)
	}
	if len(names) == 0 {
		return "none"
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// normalizeSparseDirs validates every input, de-duplicates, sorts, and reduces
// the set so a parent covers its descendants. The result is directly
// comparable with `git sparse-checkout list` in cone mode.
func normalizeSparseDirs(raw []string, profile string) ([]string, error) {
	dirs := make([]string, 0, len(raw))
	for _, value := range raw {
		cleaned, err := normalizeSparseDir(value)
		if err != nil {
			return nil, fmt.Errorf("profile %q: %w", profile, err)
		}
		dirs = append(dirs, cleaned)
	}
	return reduceConeDirs(dirs), nil
}

func normalizeSparseDir(raw string) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("sparse path is empty")
	}
	if strings.ContainsRune(raw, 0) || strings.ContainsAny(raw, "\r\n") {
		return "", fmt.Errorf("sparse path %q contains a control character", raw)
	}
	if strings.Contains(raw, "\\") {
		return "", fmt.Errorf("sparse path %q must use forward slashes", raw)
	}
	if path.IsAbs(raw) || filepath.IsAbs(raw) || filepath.VolumeName(raw) != "" || hasDriveLetter(raw) {
		return "", fmt.Errorf("sparse path %q must be relative to the repository root", raw)
	}
	if strings.HasPrefix(raw, "-") {
		return "", fmt.Errorf("sparse path %q must not start with '-'", raw)
	}
	for _, segment := range strings.Split(raw, "/") {
		if segment == ".." {
			return "", fmt.Errorf("sparse path %q must not contain '..'", raw)
		}
		if strings.EqualFold(segment, ".git") || strings.EqualFold(segment, ".jj") {
			return "", fmt.Errorf("sparse path %q must not select VCS metadata", raw)
		}
	}
	cleaned := path.Clean(raw)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("sparse path %q must name a directory inside the repository", raw)
	}
	return cleaned, nil
}

// hasDriveLetter rejects a Windows volume spelling on every platform so the
// same config is accepted or rejected identically everywhere.
func hasDriveLetter(value string) bool {
	return len(value) >= 2 && value[1] == ':' &&
		((value[0] >= 'a' && value[0] <= 'z') || (value[0] >= 'A' && value[0] <= 'Z'))
}

func reduceConeDirs(dirs []string) []string {
	sort.Strings(dirs)
	reduced := make([]string, 0, len(dirs))
	for _, dir := range dirs {
		covered := false
		for _, parent := range reduced {
			if dir == parent || strings.HasPrefix(dir, parent+"/") {
				covered = true
				break
			}
		}
		if !covered {
			reduced = append(reduced, dir)
		}
	}
	return reduced
}

// readSparseFile reads a checked-in cone list from the main checkout. os.Root
// makes an escape above repoRoot impossible, and Lstat rejects symlinks so a
// checked-in link cannot redirect the read elsewhere.
func readSparseFile(repoRoot, profile, rel string) ([]string, error) {
	if rel == "" {
		return nil, fmt.Errorf("profile %q: sparse_file must not be empty", profile)
	}
	if strings.ContainsRune(rel, 0) {
		return nil, fmt.Errorf("profile %q: sparse_file %q contains a NUL byte", profile, rel)
	}
	name := filepath.FromSlash(rel)
	if !filepath.IsLocal(name) {
		return nil, fmt.Errorf("profile %q: sparse_file %q must be a relative path inside the repository", profile, rel)
	}

	root, err := os.OpenRoot(repoRoot)
	if err != nil {
		return nil, fmt.Errorf("profile %q: sparse_file %q: %w", profile, rel, err)
	}
	defer root.Close()

	info, err := root.Lstat(name)
	if err != nil {
		return nil, fmt.Errorf("profile %q: sparse_file %q: %w", profile, rel, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("profile %q: sparse_file %q is not a regular file", profile, rel)
	}
	if info.Size() > maxSparseFileBytes {
		return nil, fmt.Errorf("profile %q: sparse_file %q exceeds %d bytes", profile, rel, maxSparseFileBytes)
	}

	file, err := root.Open(name)
	if err != nil {
		return nil, fmt.Errorf("profile %q: sparse_file %q: %w", profile, rel, err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxSparseFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("profile %q: sparse_file %q: %w", profile, rel, err)
	}
	if len(data) > maxSparseFileBytes {
		return nil, fmt.Errorf("profile %q: sparse_file %q exceeds %d bytes", profile, rel, maxSparseFileBytes)
	}

	var dirs []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			dirs = append(dirs, line)
		}
	}
	return dirs, nil
}

func validateProfileEnv(profile string, env map[string]string) error {
	seen := make(map[string]string, len(env))
	for key, value := range env {
		if key == "" || strings.ContainsAny(key, "=\x00") {
			return fmt.Errorf("profile %q: invalid env key %q", profile, key)
		}
		if strings.ContainsRune(value, 0) {
			return fmt.Errorf("profile %q: env value for %q contains a NUL byte", profile, key)
		}
		upper := strings.ToUpper(key)
		if strings.HasPrefix(upper, "TREEHOUSE_") {
			return fmt.Errorf("profile %q: env key %q is reserved", profile, key)
		}
		if previous, ok := seen[upper]; ok {
			return fmt.Errorf("profile %q: env keys %q and %q differ only by case", profile, previous, key)
		}
		seen[upper] = key
	}
	return nil
}

// warnUnknownProfileKeys reports misspelled profile keys, which would
// otherwise silently turn a sparse profile into a full checkout. Unknown keys
// outside [profiles] keep their existing silently-ignored behavior.
func warnUnknownProfileKeys(w io.Writer, source string, md toml.MetaData) {
	var unknown []string
	for _, key := range md.Undecoded() {
		if len(key) >= 2 && key[0] == "profiles" {
			unknown = append(unknown, key.String())
		}
	}
	if len(unknown) == 0 {
		return
	}
	sort.Strings(unknown)
	fmt.Fprintf(w, "🌳 Warning: ignoring unknown profile key(s) in %s: %s\n", source, strings.Join(unknown, ", "))
}
