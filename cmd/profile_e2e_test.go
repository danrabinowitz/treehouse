package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func addSparseE2ETree(t *testing.T, repoDir string) {
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
	gitCmd(t, repoDir, "add", ".")
	gitCmd(t, repoDir, "commit", "-m", "sparse tree")
	gitCmd(t, repoDir, "push", "origin", "main")
}

type profileLease struct {
	Path    string            `json:"path"`
	Name    string            `json:"name"`
	Profile string            `json:"profile"`
	Env     map[string]string `json:"env"`
	LeaseID string            `json:"lease_id"`
}

type profileStatus struct {
	Path   string `json:"path"`
	Flavor string `json:"flavor"`
	Sparse *struct {
		Enabled bool   `json:"enabled"`
		Cone    bool   `json:"cone"`
		Dirs    int    `json:"dirs"`
		Error   string `json:"error"`
	} `json:"sparse"`
}

func decodeProfileLease(t *testing.T, stdout string) profileLease {
	t.Helper()
	var lease profileLease
	if err := json.Unmarshal([]byte(stdout), &lease); err != nil {
		t.Fatalf("decode lease JSON %q: %v", stdout, err)
	}
	return lease
}

func decodeProfileStatus(t *testing.T, stdout string) []profileStatus {
	t.Helper()
	var statuses []profileStatus
	if err := json.Unmarshal([]byte(stdout), &statuses); err != nil {
		t.Fatalf("decode status JSON %q: %v", stdout, err)
	}
	return statuses
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestGetProfileConeFullDefaultAndStatus(t *testing.T) {
	repoDir, homeDir := setupTestRepo(t)
	addSparseE2ETree(t, repoDir)
	writeRepoConfig(t, repoDir, `
max_trees = 1
default = "cone"

[profiles.cone]
sparse = ["a"]
env = { MY_PROFILE_VAR = "profile-value" }

[profiles.full]
sparse = false
`)

	stdout, stderr, code := runTreehouse(t, repoDir, homeDir, nil, "get", "--lease", "--profile", "cone", "--json")
	if code != 0 {
		t.Fatalf("get cone failed (%d): %s", code, stderr)
	}
	cone := decodeProfileLease(t, stdout)
	if cone.Profile != "cone" || cone.Name == "" || cone.Env["MY_PROFILE_VAR"] != "profile-value" {
		t.Fatalf("cone lease = %+v", cone)
	}
	if !exists(filepath.Join(cone.Path, "a", "b", "f.txt")) {
		t.Fatal("selected directory missing")
	}
	if exists(filepath.Join(cone.Path, "c", "g.txt")) || exists(filepath.Join(cone.Path, "vendor", "h.txt")) {
		t.Fatal("unselected directories materialized")
	}
	if out, err := gitCmdResult(t, cone.Path, "config", "--worktree", "--bool", "--get", "core.sparseCheckout"); err != nil || strings.TrimSpace(out) != "true" {
		t.Fatalf("worktree sparse config = %q, %v", out, err)
	}

	stdout, stderr, code = runTreehouse(t, repoDir, homeDir, nil, "status", "--json")
	if code != 0 {
		t.Fatalf("status failed (%d): %s", code, stderr)
	}
	var found bool
	for _, status := range decodeProfileStatus(t, stdout) {
		if status.Path != cone.Path {
			continue
		}
		found = true
		if status.Sparse == nil || !status.Sparse.Enabled || !status.Sparse.Cone || status.Sparse.Dirs != 1 {
			t.Fatalf("cone status = %+v", status.Sparse)
		}
	}
	if !found {
		t.Fatal("cone slot missing from status")
	}

	if _, stderr, code = runTreehouse(t, repoDir, homeDir, nil, "return", cone.Path); code != 0 {
		t.Fatalf("return failed (%d): %s", code, stderr)
	}

	stdout, stderr, code = runTreehouse(t, repoDir, homeDir, nil, "get", "--lease", "--profile", "full", "--json")
	if code != 0 {
		t.Fatalf("get full failed (%d): %s", code, stderr)
	}
	full := decodeProfileLease(t, stdout)
	if full.Path != cone.Path {
		t.Fatalf("expected reuse of %s, got %s", cone.Path, full.Path)
	}
	if !exists(filepath.Join(full.Path, "vendor", "h.txt")) || !exists(filepath.Join(full.Path, "c", "g.txt")) {
		t.Fatal("full profile did not expand every directory")
	}
	if _, stderr, code = runTreehouse(t, repoDir, homeDir, nil, "return", full.Path); code != 0 {
		t.Fatalf("return full failed (%d): %s", code, stderr)
	}

	// The repo default selects sparse data but must not inject env.
	stdout, stderr, code = runTreehouse(t, repoDir, homeDir, nil, "get", "--lease", "--json")
	if code != 0 {
		t.Fatalf("get default failed (%d): %s", code, stderr)
	}
	def := decodeProfileLease(t, stdout)
	if def.Profile != "cone" || def.Env != nil {
		t.Fatalf("default lease = %+v", def)
	}
	if exists(filepath.Join(def.Path, "c", "g.txt")) {
		t.Fatal("default cone was not applied")
	}
}

func TestGetProfileFailuresHappenBeforeAllocation(t *testing.T) {
	repoDir, homeDir := setupTestRepo(t)
	addSparseE2ETree(t, repoDir)
	writeRepoConfig(t, repoDir, `
max_trees = 1
[profiles.cone]
sparse = ["a"]
[profiles.badfile]
sparse_file = "../escape.txt"
`)

	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "unknown", args: []string{"get", "--lease", "--profile", "nope"}, want: "not defined"},
		{name: "escaping file", args: []string{"get", "--lease", "--profile", "badfile"}, want: "inside the repository"},
		{name: "empty", args: []string{"get", "--lease", "--profile="}, want: "--profile requires"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stdout, stderr, code := runTreehouse(t, repoDir, homeDir, nil, tt.args...)
			if code == 0 {
				t.Fatalf("expected failure, stdout=%q stderr=%q", stdout, stderr)
			}
			if stdout != "" {
				t.Fatalf("stdout = %q", stdout)
			}
			if !strings.Contains(stderr, tt.want) {
				t.Fatalf("stderr = %q, want containing %q", stderr, tt.want)
			}
		})
	}

	stdout, stderr, code := runTreehouse(t, repoDir, homeDir, nil, "status", "--json")
	if code != 0 {
		t.Fatalf("status failed (%d): %s", code, stderr)
	}
	if statuses := decodeProfileStatus(t, stdout); len(statuses) != 0 {
		t.Fatalf("failed profile requests allocated slots: %+v", statuses)
	}
}

func TestGetWithoutProfileConfigKeepsFullCheckout(t *testing.T) {
	repoDir, homeDir := setupTestRepo(t)
	addSparseE2ETree(t, repoDir)

	stdout, stderr, code := runTreehouse(t, repoDir, homeDir, nil, "get", "--lease", "--json")
	if code != 0 {
		t.Fatalf("get failed (%d): %s", code, stderr)
	}
	lease := decodeProfileLease(t, stdout)
	if lease.Profile != "" || lease.Env != nil {
		t.Fatalf("no-config lease = %+v", lease)
	}
	for _, name := range []string{"a/b/f.txt", "c/g.txt", "vendor/h.txt"} {
		if !exists(filepath.Join(lease.Path, filepath.FromSlash(name))) {
			t.Fatalf("no-config acquisition is not a full checkout: %s missing", name)
		}
	}

	stdout, stderr, code = runTreehouse(t, repoDir, homeDir, nil, "status", "--json")
	if code != 0 {
		t.Fatalf("status failed (%d): %s", code, stderr)
	}
	for _, status := range decodeProfileStatus(t, stdout) {
		if status.Path == lease.Path {
			if status.Sparse == nil || status.Sparse.Enabled {
				t.Fatalf("no-config status = %+v", status.Sparse)
			}
			return
		}
	}
	t.Fatal("slot missing from status")
}

func TestGetProfileRejectedForJJ(t *testing.T) {
	requireJJ(t)
	repoDir, homeDir := setupJJTestRepo(t)
	writeRepoConfig(t, repoDir, "vcs = \"jj\"\n\n[profiles.cone]\nsparse = [\"a\"]\n")

	stdout, stderr, code := runTreehouse(t, repoDir, homeDir, nil, "get", "--lease", "--profile", "cone")
	if code == 0 {
		t.Fatalf("expected jj refusal, stdout=%q stderr=%q", stdout, stderr)
	}
	if !strings.Contains(stderr, "only supported by the git backend") {
		t.Fatalf("stderr = %q", stderr)
	}
}
