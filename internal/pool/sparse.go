package pool

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	envpkg "github.com/kunchenguid/treehouse/v3/internal/env"
)

// sparseCheckoutFailedLeaseHolder quarantines a slot whose sparse profile
// could not be applied. The slot stays leased so no later acquisition reuses a
// half-configured checkout.
const sparseCheckoutFailedLeaseHolder = "quarantined: sparse checkout failed"

// SparseSelection is a normalized sparse-checkout selection produced from a
// config profile. Dirs are repo-relative slash paths in Git's minimal cone
// set; they are ignored when Full is true.
type SparseSelection struct {
	Full bool
	Dirs []string
}

// applySparse enforces selection on a worktree that has just been reset or
// created, before seeding, branch creation, sharing, or publication. A nil
// selection is a no-op, preserving behavior for repositories without
// profiles. The before/after diff is reported so a caller can see which slot
// it was handed and what changed.
func applySparse(worktreePath string, selection *SparseSelection, profile string, stderr io.Writer) error {
	if selection == nil {
		return nil
	}
	before, changed, err := applySparseCheckout(worktreePath, selection.Dirs, selection.Full)
	if err != nil {
		return err
	}
	label := profile
	if label == "" {
		label = "default"
	}
	if selection.Full {
		if changed {
			fmt.Fprintf(stderr, "🌳 Sparse checkout disabled for profile %q: full checkout.\n", label)
		} else {
			fmt.Fprintf(stderr, "🌳 Sparse checkout already full for profile %q.\n", label)
		}
		return nil
	}
	if !changed {
		fmt.Fprintf(stderr, "🌳 Sparse checkout already matches profile %q (%d dirs).\n", label, len(selection.Dirs))
	} else {
		added, removed := sparseDirDiff(before, selection.Dirs)
		fmt.Fprintf(stderr, "🌳 Sparse checkout set for profile %q: %d dirs (+%d, -%d).\n", label, len(selection.Dirs), added, removed)
	}
	warnMissingSparseDirs(worktreePath, selection.Dirs, stderr)
	return nil
}

func sparseDirDiff(before, after []string) (added, removed int) {
	beforeSet := make(map[string]struct{}, len(before))
	afterSet := make(map[string]struct{}, len(after))
	for _, dir := range before {
		beforeSet[dir] = struct{}{}
	}
	for _, dir := range after {
		afterSet[dir] = struct{}{}
		if _, ok := beforeSet[dir]; !ok {
			added++
		}
	}
	for _, dir := range before {
		if _, ok := afterSet[dir]; !ok {
			removed++
		}
	}
	return added, removed
}

// warnMissingSparseDirs reports a declared directory that the checkout does
// not contain. Git accepts the pattern anyway; a claim should still surface a
// stale cone file rather than silently materialize nothing.
func warnMissingSparseDirs(worktreePath string, dirs []string, stderr io.Writer) {
	for _, dir := range dirs {
		info, err := os.Lstat(filepath.Join(worktreePath, filepath.FromSlash(dir)))
		if err != nil || !info.IsDir() {
			fmt.Fprintf(stderr, "🌳 Warning: sparse profile selects %q, which is not a directory in this checkout; it was not materialized.\n", dir)
		}
	}
}

// claimHookEnv is the environment for post_claim. TREEHOUSE_LEASE_ID is empty
// for a plain get, which holds an owner reservation rather than a durable
// lease. Profile env is included only when --profile named the profile.
func claimHookEnv(repoRoot string, opts acquireOptions, acquired LeaseInfo) []string {
	entries := []string{
		"TREEHOUSE_PROFILE=" + opts.profile,
		"TREEHOUSE_SLOT=" + acquired.Name,
		"TREEHOUSE_LEASE_ID=" + acquired.LeaseID,
		"TREEHOUSE_REPO=" + repoRoot,
	}
	return append(entries, envpkg.Pairs(opts.profileEnv)...)
}
