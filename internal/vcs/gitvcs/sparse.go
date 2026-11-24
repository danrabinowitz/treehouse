package gitvcs

import (
	"fmt"
	"os/exec"
	"path"
	"sort"
	"strconv"
	"strings"
)

// minSparseGitVersion is the oldest Git treehouse supports for sparse
// profiles. `git sparse-checkout set` upgrades a repository to worktree
// specific config at this version, which is what keeps one slot's cone from
// affecting its siblings. The post-set verification below is authoritative if
// Git's exact behavior differs; this check only produces a clearer error.
const minSparseGitVersion = "2.36"

// ReadSparseCheckout reports a worktree's sparse configuration. A non-sparse
// worktree returns all-zero values rather than an error.
func ReadSparseCheckout(worktreePath string) (bool, bool, bool, []string, error) {
	enabled, set, err := gitConfigBool(worktreePath, "core.sparseCheckout")
	if err != nil {
		return false, false, false, nil, err
	}
	if !set || !enabled {
		return false, false, false, nil, nil
	}
	cone, _, err := gitConfigBool(worktreePath, "core.sparseCheckoutCone")
	if err != nil {
		return false, false, false, nil, err
	}
	sparseIndex, _, err := gitConfigBool(worktreePath, "index.sparse")
	if err != nil {
		return false, false, false, nil, err
	}
	dirs, err := sparseCheckoutList(worktreePath)
	if err != nil {
		return false, false, false, nil, err
	}
	return true, cone, sparseIndex, dirs, nil
}

// ApplySparseCheckout enforces a full checkout or a cone-mode directory
// selection. It is idempotent: applying the same selection twice reports
// changed=false and runs no mutating Git command.
func ApplySparseCheckout(worktreePath string, dirs []string, full bool) ([]string, bool, error) {
	enabled, cone, sparseIndex, before, err := ReadSparseCheckout(worktreePath)
	if err != nil {
		return nil, false, err
	}

	if full {
		if !enabled && !cone && !sparseIndex {
			return before, false, nil
		}
		if err := checkSparseCheckoutSupport(worktreePath); err != nil {
			return nil, false, err
		}
		if _, err := runGit(worktreePath, "sparse-checkout", "disable"); err != nil {
			return nil, false, err
		}
		stillEnabled, stillCone, stillIndex, _, err := ReadSparseCheckout(worktreePath)
		if err != nil {
			return nil, false, err
		}
		if stillEnabled || stillCone || stillIndex {
			return nil, false, fmt.Errorf("git sparse-checkout disable did not restore a full checkout")
		}
		return before, true, nil
	}

	desired, err := normalizeSparseList(dirs)
	if err != nil {
		return nil, false, err
	}
	if len(desired) == 0 {
		return nil, false, fmt.Errorf("sparse checkout requires at least one directory")
	}
	if enabled && cone && sparseIndex && equalStrings(before, desired) {
		return before, false, nil
	}

	if err := checkSparseCheckoutSupport(worktreePath); err != nil {
		return nil, false, err
	}
	args := append([]string{"sparse-checkout", "set", "--cone", "--sparse-index", "--"}, desired...)
	if _, err := runGit(worktreePath, args...); err != nil {
		return nil, false, err
	}
	if err := verifyWorktreeSparseConfig(worktreePath); err != nil {
		return nil, false, err
	}
	afterEnabled, afterCone, afterIndex, afterDirs, err := ReadSparseCheckout(worktreePath)
	if err != nil {
		return nil, false, err
	}
	if !afterEnabled || !afterCone || !afterIndex || !equalStrings(afterDirs, desired) {
		return nil, false, fmt.Errorf("git sparse-checkout set did not produce the requested state")
	}
	return before, true, nil
}

// gitConfigBool reads a boolean config key in dir. set is false when the key
// is unset (git exits 1), distinguishing "unset" from an explicit false.
func gitConfigBool(dir, key string) (value, set bool, err error) {
	cmd := exec.Command("git", "-c", "submodule.recurse=false", "config", "--bool", "--get", key)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return false, false, nil
		}
		return false, false, fmt.Errorf("git config %s in %s: %w", key, dir, err)
	}
	switch strings.TrimSpace(string(out)) {
	case "true":
		return true, true, nil
	case "false":
		return false, true, nil
	default:
		return false, false, fmt.Errorf("git config %s in %s returned an unexpected boolean", key, dir)
	}
}

func sparseCheckoutList(dir string) ([]string, error) {
	out, err := gitOutput(dir, nil, "sparse-checkout", "list")
	if err != nil {
		return nil, fmt.Errorf("git sparse-checkout list in %s: %w", dir, err)
	}
	var dirs []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			dirs = append(dirs, path.Clean(strings.ReplaceAll(line, "\\", "/")))
		}
	}
	sort.Strings(dirs)
	return dirs, nil
}

// normalizeSparseList is defense in depth behind config validation: the same
// inputs can reach this package through other callers.
func normalizeSparseList(dirs []string) ([]string, error) {
	seen := make(map[string]struct{}, len(dirs))
	normalized := make([]string, 0, len(dirs))
	for _, raw := range dirs {
		if raw == "" || strings.ContainsAny(raw, "\x00\r\n\\") {
			return nil, fmt.Errorf("sparse path %q is empty or contains a control character", raw)
		}
		if path.IsAbs(raw) || strings.HasPrefix(raw, "-") {
			return nil, fmt.Errorf("sparse path %q must be a relative directory", raw)
		}
		cleaned := path.Clean(raw)
		if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
			return nil, fmt.Errorf("sparse path %q must name a directory inside the repository", raw)
		}
		if _, ok := seen[cleaned]; ok {
			continue
		}
		seen[cleaned] = struct{}{}
		normalized = append(normalized, cleaned)
	}
	sort.Strings(normalized)
	return normalized, nil
}

func verifyWorktreeSparseConfig(dir string) error {
	out, err := gitOutput(dir, nil, "config", "--worktree", "--bool", "--get", "core.sparseCheckout")
	if err != nil {
		return fmt.Errorf("sparse-checkout was not recorded as worktree-specific config (profiles require Git >= %s): %w", minSparseGitVersion, err)
	}
	if strings.TrimSpace(string(out)) != "true" {
		return fmt.Errorf("sparse-checkout was not recorded as worktree-specific config (profiles require Git >= %s)", minSparseGitVersion)
	}
	return nil
}

func checkSparseCheckoutSupport(dir string) error {
	out, err := gitOutput(dir, nil, "version")
	if err != nil {
		return fmt.Errorf("cannot determine Git version for sparse-checkout profiles: %w", err)
	}
	fields := strings.Fields(strings.TrimSpace(string(out)))
	if len(fields) < 3 {
		// Unparseable versions fall through to the post-set verification,
		// which fails closed if worktree config was not recorded.
		return nil
	}
	major, minor, ok := parseGitVersion(fields[2])
	if !ok {
		return nil
	}
	if major < 2 || (major == 2 && minor < 36) {
		return fmt.Errorf("sparse-checkout profiles require Git >= %s (found %s)", minSparseGitVersion, fields[2])
	}
	return nil
}

func parseGitVersion(raw string) (major, minor int, ok bool) {
	parts := strings.SplitN(raw, ".", 3)
	if len(parts) < 2 {
		return 0, 0, false
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, false
	}
	minor, err = strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, false
	}
	return major, minor, true
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
