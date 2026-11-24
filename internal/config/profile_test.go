package config

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

func decodeProfiles(t *testing.T, text string) Config {
	t.Helper()
	cfg := DefaultConfig()
	if _, err := toml.Decode(text, &cfg); err != nil {
		t.Fatalf("decode profiles: %v", err)
	}
	return cfg
}

func TestResolveProfile_InlineAndFileUnionReducesToMinimalCone(t *testing.T) {
	repoDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoDir, "cone.txt"), []byte("a\nc/d\r\n\nb\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := decodeProfiles(t, `
default = "cone"
[profiles.cone]
sparse = ["a", "a/b", "c"]
sparse_file = "cone.txt"
env = { GOFLAGS = "-mod=readonly" }
`)
	profile, err := ResolveProfile(repoDir, cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	if profile == nil {
		t.Fatal("expected the default profile to be selected")
	}
	if profile.Name != "cone" || profile.Explicit || profile.Full {
		t.Fatalf("unexpected profile identity: %+v", profile)
	}
	if want := []string{"a", "b", "c"}; !reflect.DeepEqual(profile.Dirs, want) {
		t.Fatalf("dirs = %v, want %v", profile.Dirs, want)
	}
	if profile.Env != nil {
		t.Fatalf("repo default must not supply env, got %v", profile.Env)
	}
	if !profile.EnvDeclared {
		t.Fatal("expected EnvDeclared so the caller can warn")
	}
}

func TestResolveProfile_ExplicitAppliesEnv(t *testing.T) {
	cfg := decodeProfiles(t, `
[profiles.cone]
sparse = ["a"]
env = { GOFLAGS = "-mod=readonly" }
`)
	profile, err := ResolveProfile(t.TempDir(), cfg, "cone")
	if err != nil {
		t.Fatal(err)
	}
	if !profile.Explicit {
		t.Fatal("expected Explicit")
	}
	if got := profile.Env["GOFLAGS"]; got != "-mod=readonly" {
		t.Fatalf("env GOFLAGS = %q", got)
	}
}

func TestResolveProfile_NoSelectionIsNil(t *testing.T) {
	cfg := decodeProfiles(t, `
[profiles.cone]
sparse = ["a"]
`)
	profile, err := ResolveProfile(t.TempDir(), cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	if profile != nil {
		t.Fatalf("expected no profile, got %+v", profile)
	}
}

func TestResolveProfile_FullSpellings(t *testing.T) {
	cfg := decodeProfiles(t, `
[profiles.false]
sparse = false
[profiles.onlyenv]
env = { GOFLAGS = "-mod=vendor" }
`)
	for _, name := range []string{"false", "onlyenv"} {
		profile, err := ResolveProfile(t.TempDir(), cfg, name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !profile.Full || len(profile.Dirs) != 0 {
			t.Fatalf("%s: expected full checkout, got %+v", name, profile)
		}
	}
}

func TestResolveProfile_Errors(t *testing.T) {
	tests := []struct {
		name    string
		config  string
		profile string
		want    string
	}{
		{
			name:    "unknown explicit",
			config:  "[profiles.cone]\nsparse = [\"a\"]\n",
			profile: "nope",
			want:    `profile "nope" is not defined (known profiles: cone)`,
		},
		{
			name:   "unknown default",
			config: "default = \"nope\"\n[profiles.cone]\nsparse = [\"a\"]\n",
			want:   `profile "nope" is not defined`,
		},
		{
			name:    "empty explicit",
			config:  "[profiles.cone]\nsparse = [\"a\"]\n",
			profile: "missing",
			want:    "not defined",
		},
		{
			name:    "true without dirs",
			config:  "[profiles.cone]\nsparse = true\n",
			profile: "cone",
			want:    "selects no directories",
		},
		{
			name:    "false with file",
			config:  "[profiles.full]\nsparse = false\nsparse_file = \"cone.txt\"\n",
			profile: "full",
			want:    "sparse_file cannot be set",
		},
		{
			name:    "false with mode",
			config:  "[profiles.full]\nsparse = false\nsparse_mode = \"cone\"\n",
			profile: "full",
			want:    "sparse_mode cannot be set",
		},
		{
			name:    "unsupported mode",
			config:  "[profiles.x]\nsparse = [\"a\"]\nsparse_mode = \"sparse\"\n",
			profile: "x",
			want:    `sparse_mode "sparse" is not supported`,
		},
		{
			name:    "mode without dirs",
			config:  "[profiles.x]\nsparse_mode = \"cone\"\n",
			profile: "x",
			want:    "sparse_mode cannot be set without sparse directories",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := decodeProfiles(t, tt.config)
			_, err := ResolveProfile(t.TempDir(), cfg, tt.profile)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestNormalizeSparseDir(t *testing.T) {
	valid := map[string]string{
		"a":       "a",
		"a/b":     "a/b",
		"a/./b":   "a/b",
		"a//b":    "a/b",
		"a/b/":    "a/b",
		"a b/c-d": "a b/c-d",
	}
	for input, want := range valid {
		got, err := normalizeSparseDir(input)
		if err != nil {
			t.Errorf("%q: unexpected error: %v", input, err)
			continue
		}
		if got != want {
			t.Errorf("%q: got %q, want %q", input, got, want)
		}
	}

	invalid := []string{
		"",
		"/abs",
		"C:/x",
		`C:\x`,
		`a\b`,
		"..",
		"../x",
		"a/../b",
		"a/../../b",
		"-x",
		".",
		"a\x00b",
		"a\nb",
		".git",
		"a/.git",
		`a\.GIT\b`,
		"a/.jj",
	}
	for _, input := range invalid {
		if got, err := normalizeSparseDir(input); err == nil {
			t.Errorf("%q: expected an error, got %q", input, got)
		}
	}
}

func TestReduceConeDirs(t *testing.T) {
	got := reduceConeDirs([]string{"a/b/c", "a", "a/b", "b", "b/c", "ab"})
	want := []string{"a", "ab", "b"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestSparseFileValidation(t *testing.T) {
	repoDir := t.TempDir()
	mustWrite := func(rel, contents string) {
		t.Helper()
		path := filepath.Join(repoDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(".treehouse/cone.txt", "a\nb\n")
	mustWrite(".treehouse/empty.txt", "")
	if err := os.Mkdir(filepath.Join(repoDir, ".treehouse", "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Symlink(filepath.Join(repoDir, ".treehouse", "cone.txt"), filepath.Join(repoDir, ".treehouse", "link.txt")); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(".treehouse/large.txt", strings.Repeat("a\n", maxSparseFileBytes))

	tests := []struct {
		name string
		rel  string
		want string
	}{
		{name: "missing", rel: ".treehouse/missing.txt", want: "no such file"},
		{name: "directory", rel: ".treehouse/dir", want: "not a regular file"},
		{name: "escape", rel: "../escape.txt", want: "relative path inside the repository"},
		{name: "absolute", rel: filepath.Join(repoDir, "treehouse.toml"), want: "relative path inside the repository"},
		{name: "too large", rel: ".treehouse/large.txt", want: "exceeds"},
	}
	if runtime.GOOS != "windows" {
		tests = append(tests, struct {
			name string
			rel  string
			want string
		}{name: "symlink", rel: ".treehouse/link.txt", want: "not a regular file"})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := readSparseFile(repoDir, "cone", tt.rel)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want containing %q", err, tt.want)
			}
		})
	}

	dirs, err := readSparseFile(repoDir, "cone", ".treehouse/cone.txt")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"a", "b"}; !reflect.DeepEqual(dirs, want) {
		t.Fatalf("dirs = %v, want %v", dirs, want)
	}
}

func TestValidateProfileEnv(t *testing.T) {
	valid := map[string]string{"GOFLAGS": "-mod=readonly", "CGO_ENABLED": "0"}
	if err := validateProfileEnv("cone", valid); err != nil {
		t.Fatalf("valid env rejected: %v", err)
	}
	invalid := []map[string]string{
		{"": "x"},
		{"A=B": "x"},
		{"A\x00": "x"},
		{"A": "x\x00"},
		{"TREEHOUSE_PROFILE": "x"},
		{"treehouse_root": "x"},
		{"A": "x", "a": "y"},
	}
	for _, env := range invalid {
		if err := validateProfileEnv("cone", env); err == nil {
			t.Errorf("expected env %v to be rejected", env)
		}
	}
}

func TestLoad_RepoProfilesWinAndUserPostClaimSurvives(t *testing.T) {
	repoDir := t.TempDir()
	home := t.TempDir()
	setUserHome(t, home)
	writeRepoConfig(t, repoDir, `
default = "cone"
[profiles.cone]
sparse = ["a"]
[hooks]
post_create = ["./repo-hook"]
`)
	userDir := filepath.Join(home, ".config", "treehouse")
	if err := os.MkdirAll(userDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(userDir, "config.toml"), []byte(`
[profiles.user]
sparse = ["u"]
[hooks]
post_create = ["./user-hook"]
post_claim = ["./claim-hook"]
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Profiles["cone"]; !ok {
		t.Fatalf("repo profile missing: %v", cfg.Profiles)
	}
	if _, ok := cfg.Profiles["user"]; ok {
		t.Fatalf("user profile must not merge into a repo config: %v", cfg.Profiles)
	}
	if cfg.DefaultProfile != "cone" {
		t.Fatalf("default = %q", cfg.DefaultProfile)
	}
	if !reflect.DeepEqual(cfg.Hooks.PostCreate, []string{"./user-hook"}) {
		t.Fatalf("post_create = %v", cfg.Hooks.PostCreate)
	}
	if !reflect.DeepEqual(cfg.Hooks.PostClaim, []string{"./claim-hook"}) {
		t.Fatalf("post_claim = %v", cfg.Hooks.PostClaim)
	}
}

func TestLoad_UserProfilesApplyWithoutRepoConfig(t *testing.T) {
	repoDir := t.TempDir()
	home := t.TempDir()
	setUserHome(t, home)
	userDir := filepath.Join(home, ".config", "treehouse")
	if err := os.MkdirAll(userDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(userDir, "config.toml"), []byte(`
default = "u"
[profiles.u]
sparse = ["x"]
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := ResolveProfile(repoDir, cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	if profile == nil || profile.Name != "u" || !reflect.DeepEqual(profile.Dirs, []string{"x"}) {
		t.Fatalf("unexpected profile: %+v", profile)
	}
}

func TestWarnUnknownProfileKeys(t *testing.T) {
	cfg := Config{}
	var buf strings.Builder
	md, err := toml.Decode(`
[profiles.cone]
sparse = ["a"]
sparce = ["typo"]
`, &cfg)
	if err != nil {
		t.Fatal(err)
	}
	warnUnknownProfileKeys(&buf, "treehouse.toml", md)
	if got := buf.String(); !strings.Contains(got, "profiles.cone.sparce") {
		t.Fatalf("warning = %q", got)
	}
}
