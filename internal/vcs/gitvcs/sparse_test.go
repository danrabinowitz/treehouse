package gitvcs

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func setupSparseRepo(t *testing.T) (repoDir, worktreePath string) {
	t.Helper()
	repoDir = t.TempDir()
	initRepoWithFormat(t, repoDir, "")

	for _, dir := range []string{"a/b", "c", "vendor"} {
		if err := os.MkdirAll(filepath.Join(repoDir, filepath.FromSlash(dir)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, file := range []string{"a/b/f.txt", "c/g.txt", "vendor/h.txt"} {
		if err := os.WriteFile(filepath.Join(repoDir, filepath.FromSlash(file)), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustGit(t, repoDir, "add", "-A")
	mustGit(t, repoDir, "commit", "-m", "tree")

	worktreePath = filepath.Join(t.TempDir(), "wt")
	mustGit(t, repoDir, "worktree", "add", "--detach", worktreePath, "HEAD")
	return repoDir, worktreePath
}

func fileExists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	if err == nil {
		return true
	}
	if os.IsNotExist(err) {
		return false
	}
	t.Fatal(err)
	return false
}

func TestReadSparseCheckout_NonSparse(t *testing.T) {
	_, worktreePath := setupSparseRepo(t)
	enabled, cone, sparseIndex, dirs, err := ReadSparseCheckout(worktreePath)
	if err != nil {
		t.Fatal(err)
	}
	if enabled || cone || sparseIndex || len(dirs) != 0 {
		t.Fatalf("unexpected sparse state: enabled=%v cone=%v index=%v dirs=%v", enabled, cone, sparseIndex, dirs)
	}
}

func TestApplySparseCheckout_ConeIdempotentAndWorktreeScoped(t *testing.T) {
	repoDir, worktreePath := setupSparseRepo(t)

	before, changed, err := ApplySparseCheckout(worktreePath, []string{"a"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !changed || len(before) != 0 {
		t.Fatalf("first apply: before=%v changed=%v", before, changed)
	}
	if !fileExists(t, filepath.Join(worktreePath, "a", "b", "f.txt")) {
		t.Fatal("selected directory was not materialized")
	}
	if fileExists(t, filepath.Join(worktreePath, "c", "g.txt")) {
		t.Fatal("unselected directory was materialized")
	}

	enabled, cone, sparseIndex, dirs, err := ReadSparseCheckout(worktreePath)
	if err != nil {
		t.Fatal(err)
	}
	if !enabled || !cone || !sparseIndex || !reflect.DeepEqual(dirs, []string{"a"}) {
		t.Fatalf("state = enabled=%v cone=%v index=%v dirs=%v", enabled, cone, sparseIndex, dirs)
	}
	if got := mustGitOutput(t, worktreePath, "config", "--worktree", "--bool", "--get", "core.sparseCheckout"); got != "true" {
		t.Fatalf("worktree config = %q", got)
	}
	if !fileExists(t, filepath.Join(repoDir, "vendor", "h.txt")) {
		t.Fatal("the main worktree must stay full")
	}

	before, changed, err = ApplySparseCheckout(worktreePath, []string{"a"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if changed || !reflect.DeepEqual(before, []string{"a"}) {
		t.Fatalf("second apply: before=%v changed=%v", before, changed)
	}
}

func TestApplySparseCheckout_SwitchAndFull(t *testing.T) {
	_, worktreePath := setupSparseRepo(t)

	if _, _, err := ApplySparseCheckout(worktreePath, []string{"a"}, false); err != nil {
		t.Fatal(err)
	}
	before, changed, err := ApplySparseCheckout(worktreePath, []string{"c"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !changed || !reflect.DeepEqual(before, []string{"a"}) {
		t.Fatalf("switch: before=%v changed=%v", before, changed)
	}
	if fileExists(t, filepath.Join(worktreePath, "a", "b", "f.txt")) {
		t.Fatal("old selection was not removed")
	}
	if !fileExists(t, filepath.Join(worktreePath, "c", "g.txt")) {
		t.Fatal("new selection was not materialized")
	}

	before, changed, err = ApplySparseCheckout(worktreePath, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if !changed || !reflect.DeepEqual(before, []string{"c"}) {
		t.Fatalf("full: before=%v changed=%v", before, changed)
	}
	if !fileExists(t, filepath.Join(worktreePath, "a", "b", "f.txt")) ||
		!fileExists(t, filepath.Join(worktreePath, "vendor", "h.txt")) {
		t.Fatal("full checkout did not restore every file")
	}
	enabled, _, _, _, err := ReadSparseCheckout(worktreePath)
	if err != nil {
		t.Fatal(err)
	}
	if enabled {
		t.Fatal("worktree still reports sparse after disable")
	}

	_, changed, err = ApplySparseCheckout(worktreePath, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("second full apply should be a no-op")
	}
}

func TestApplySparseCheckout_ResetPreservesSparse(t *testing.T) {
	_, worktreePath := setupSparseRepo(t)
	if _, _, err := ApplySparseCheckout(worktreePath, []string{"a"}, false); err != nil {
		t.Fatal(err)
	}

	mustGit(t, worktreePath, "read-tree", "--reset", "-u", "HEAD")
	mustGit(t, worktreePath, "clean", "-fd")

	enabled, cone, _, dirs, err := ReadSparseCheckout(worktreePath)
	if err != nil {
		t.Fatal(err)
	}
	if !enabled || !cone || !reflect.DeepEqual(dirs, []string{"a"}) {
		t.Fatalf("reset changed sparse state: enabled=%v cone=%v dirs=%v", enabled, cone, dirs)
	}
	if fileExists(t, filepath.Join(worktreePath, "c", "g.txt")) {
		t.Fatal("reset materialized excluded files")
	}
}

func TestApplySparseCheckout_MissingDirectoryIsAccepted(t *testing.T) {
	_, worktreePath := setupSparseRepo(t)
	_, changed, err := ApplySparseCheckout(worktreePath, []string{"does/not/exist"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected a change")
	}
	_, _, _, dirs, err := ReadSparseCheckout(worktreePath)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(dirs, []string{"does/not/exist"}) {
		t.Fatalf("dirs = %v", dirs)
	}
}

func TestApplySparseCheckout_EmptySelectionIsAnError(t *testing.T) {
	_, worktreePath := setupSparseRepo(t)
	if _, _, err := ApplySparseCheckout(worktreePath, nil, false); err == nil {
		t.Fatal("expected an error for an empty cone")
	}
}

func TestNormalizeSparseList(t *testing.T) {
	got, err := normalizeSparseList([]string{"a/b", "a/b", "c"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"a/b", "c"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for _, raw := range []string{"", "/abs", "../x", "-x", `a\b`, "a\x00b", "a\nb"} {
		if _, err := normalizeSparseList([]string{raw}); err == nil {
			t.Errorf("%q: expected an error", raw)
		}
	}
}

func TestParseGitVersion(t *testing.T) {
	tests := []struct {
		raw   string
		major int
		minor int
		ok    bool
	}{
		{"2.36.0", 2, 36, true},
		{"2.56.1", 2, 56, true},
		{"2.39.3", 2, 39, true},
		{"2.45.1.windows.1", 2, 45, true},
		{"2.35.9", 2, 35, true},
		{"not-a-version", 0, 0, false},
	}
	for _, tt := range tests {
		major, minor, ok := parseGitVersion(tt.raw)
		if major != tt.major || minor != tt.minor || ok != tt.ok {
			t.Errorf("parseGitVersion(%q) = %d, %d, %v", tt.raw, major, minor, ok)
		}
	}
}
