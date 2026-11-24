package pool

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/treehouse/v3/internal/vcs"
)

func addSparseTree(t *testing.T, repoDir string) {
	t.Helper()
	for _, name := range []string{"a/b/f.txt", "c/g.txt", "vendor/h.txt"} {
		path := filepath.Join(repoDir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runGit(t, repoDir, "add", "-A")
	runGit(t, repoDir, "commit", "-m", "tree")
	runGit(t, repoDir, "push", "origin", "main")
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestAcquire_AppliesSparseProfileOnCreateAndReuse(t *testing.T) {
	repoDir, poolDir := setupRepo(t)
	addSparseTree(t, repoDir)

	cone := func(dirs ...string) AcquireOptions {
		return AcquireOptions{Sparse: &SparseSelection{Dirs: dirs}, Profile: "cone"}
	}

	first, err := AcquireWithOptions(repoDir, poolDir, 1, nil, cone("a"))
	if err != nil {
		t.Fatal(err)
	}
	if !pathExists(filepath.Join(first, "a", "b", "f.txt")) {
		t.Fatal("selected directory missing after creation")
	}
	if pathExists(filepath.Join(first, "c", "g.txt")) {
		t.Fatal("unselected directory materialized after creation")
	}
	if err := Release(poolDir, first); err != nil {
		t.Fatal(err)
	}

	second, err := AcquireWithOptions(repoDir, poolDir, 1, nil, cone("c"))
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatalf("expected reuse of %s, got %s", first, second)
	}
	if pathExists(filepath.Join(second, "a", "b", "f.txt")) {
		t.Fatal("old selection survived the switch")
	}
	if !pathExists(filepath.Join(second, "c", "g.txt")) {
		t.Fatal("new selection missing after the switch")
	}
	if err := Release(poolDir, second); err != nil {
		t.Fatal(err)
	}

	third, err := AcquireWithOptions(repoDir, poolDir, 1, nil, AcquireOptions{
		Sparse: &SparseSelection{Full: true}, Profile: "full",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !pathExists(filepath.Join(third, "a", "b", "f.txt")) || !pathExists(filepath.Join(third, "vendor", "h.txt")) {
		t.Fatal("full profile did not expand the slot")
	}
	if err := Release(poolDir, third); err != nil {
		t.Fatal(err)
	}

	// The same profile applied to a slot that already matches must be a no-op.
	if _, err := AcquireWithOptions(repoDir, poolDir, 1, nil, AcquireOptions{
		Sparse: &SparseSelection{Full: true}, Profile: "full",
	}); err != nil {
		t.Fatal(err)
	}
}

func TestAcquire_SparseFailureQuarantinesAndSkipsHooks(t *testing.T) {
	originalApply := applySparseCheckout
	t.Cleanup(func() { applySparseCheckout = originalApply })
	applySparseCheckout = func(string, []string, bool) ([]string, bool, error) {
		return nil, false, errors.New("injected failure")
	}

	repoDir, poolDir := setupRepo(t)
	addSparseTree(t, repoDir)
	postCreate := "echo created > create-sentinel.txt"
	postClaim := "echo claimed > claim-sentinel.txt"

	_, err := AcquireWithOptions(repoDir, poolDir, 1, []string{postCreate}, AcquireOptions{
		Sparse:    &SparseSelection{Dirs: []string{"a"}},
		Profile:   "cone",
		PostClaim: []string{postClaim},
	})
	if err == nil || !strings.Contains(err.Error(), "sparse profile") {
		t.Fatalf("Acquire error = %v", err)
	}

	state, err := ReadState(poolDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Worktrees) != 1 {
		t.Fatalf("worktrees = %d", len(state.Worktrees))
	}
	entry := state.Worktrees[0]
	if !entry.Leased || entry.LeaseHolder != sparseCheckoutFailedLeaseHolder {
		t.Fatalf("entry not quarantined: leased=%v holder=%q", entry.Leased, entry.LeaseHolder)
	}
	if pathExists(filepath.Join(entry.Path, "create-sentinel.txt")) ||
		pathExists(filepath.Join(entry.Path, "claim-sentinel.txt")) {
		t.Fatal("hooks ran for a slot whose profile failed")
	}

	// The quarantined slot must not be handed out, and the full pool refuses.
	_, err = AcquireWithOptions(repoDir, poolDir, 1, nil, AcquireOptions{
		Sparse: &SparseSelection{Dirs: []string{"a"}}, Profile: "cone",
	})
	if err == nil {
		t.Fatal("quarantined slot was handed out")
	}
}

func claimEnvScript(sentinel string) string {
	quoted := quoteForShell(sentinel)
	if runtime.GOOS == "windows" {
		return "(echo [%TREEHOUSE_PROFILE%]& echo [%TREEHOUSE_SLOT%]& echo [%TREEHOUSE_LEASE_ID%]& echo [%TREEHOUSE_REPO%]& echo [%MY_PROFILE_VAR%])> " + quoted
	}
	return `printf '[%s]\n' "$TREEHOUSE_PROFILE" "$TREEHOUSE_SLOT" "$TREEHOUSE_LEASE_ID" "$TREEHOUSE_REPO" "$MY_PROFILE_VAR" > ` + quoted
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		lines = append(lines, strings.Trim(strings.TrimSpace(line), "[]"))
	}
	return lines
}

func TestAcquire_PostClaimEnvForOwnerAndLease(t *testing.T) {
	repoDir, poolDir := setupRepo(t)
	addSparseTree(t, repoDir)
	sentinel := "claim-env.txt"
	options := AcquireOptions{
		Sparse:     &SparseSelection{Dirs: []string{"a"}},
		Profile:    "cone",
		ProfileEnv: map[string]string{"MY_PROFILE_VAR": "profile-value"},
		PostClaim:  []string{claimEnvScript(sentinel)},
	}

	wtPath, err := AcquireWithOptions(repoDir, poolDir, 2, nil, options)
	if err != nil {
		t.Fatal(err)
	}
	lines := readLines(t, filepath.Join(wtPath, sentinel))
	if len(lines) != 5 {
		t.Fatalf("env lines = %v", lines)
	}
	if lines[0] != "cone" || lines[1] == "" || lines[2] != "" || lines[3] != repoDir || lines[4] != "profile-value" {
		t.Fatalf("owner env = %v", lines)
	}

	lease, err := AcquireLeaseInfoWithOptions(repoDir, poolDir, 2, nil, "holder", options)
	if err != nil {
		t.Fatal(err)
	}
	lines = readLines(t, filepath.Join(lease.Path, sentinel))
	if lines[0] != "cone" || lines[1] != lease.Name || lines[2] != lease.LeaseID || lines[3] != repoDir || lines[4] != "profile-value" {
		t.Fatalf("lease env = %v (lease %+v)", lines, lease)
	}
	if lease.Profile != "cone" || lease.Env["MY_PROFILE_VAR"] != "profile-value" {
		t.Fatalf("lease info = %+v", lease)
	}
}

func TestList_ReportsLiveSparseStateAndReadFailure(t *testing.T) {
	repoDir, poolDir := setupRepo(t)
	addSparseTree(t, repoDir)
	wtPath, err := AcquireWithOptions(repoDir, poolDir, 1, nil, AcquireOptions{
		Sparse: &SparseSelection{Dirs: []string{"a"}}, Profile: "cone",
	})
	if err != nil {
		t.Fatal(err)
	}

	statuses, err := List(poolDir)
	if err != nil {
		t.Fatal(err)
	}
	var found *WorktreeStatus
	for i := range statuses {
		if statuses[i].Path == wtPath {
			found = &statuses[i]
		}
	}
	if found == nil {
		t.Fatal("slot missing from List")
	}
	if !found.SparseEnabled || !found.SparseCone || found.SparseDirs != 1 || found.SparseErr != "" {
		t.Fatalf("sparse status = %+v", found)
	}

	originalRead := readSparseCheckout
	t.Cleanup(func() { readSparseCheckout = originalRead })
	readSparseCheckout = func(string) (vcs.SparseCheckoutInfo, error) {
		return vcs.SparseCheckoutInfo{}, errors.New("injected read failure")
	}
	statuses, err = List(poolDir)
	if err != nil {
		t.Fatalf("List failed on a sparse read error: %v", err)
	}
	for _, status := range statuses {
		if status.Path == wtPath && status.SparseErr == "" {
			t.Fatal("expected SparseErr")
		}
	}
}

func TestSparseAcquisitionDoesNotPersistProfileInState(t *testing.T) {
	repoDir, poolDir := setupRepo(t)
	addSparseTree(t, repoDir)
	if _, err := AcquireWithOptions(repoDir, poolDir, 1, nil, AcquireOptions{
		Sparse: &SparseSelection{Dirs: []string{"a"}}, Profile: "cone",
	}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(poolDir, "treehouse-state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"profile"`) {
		t.Fatalf("state file records a profile: %s", data)
	}
}
