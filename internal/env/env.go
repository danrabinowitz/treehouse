// Package env merges environment lists at the point a child process is
// started. It exists because appending an override to os.Environ() does not
// reliably replace an inherited value: Unix getenv commonly returns the first
// match, so the profile value would lose to the parent's.
package env

import (
	"runtime"
	"sort"
	"strings"
)

// Merge returns base with overrides applied. An entry in base whose key also
// appears in overrides is dropped, then the overrides are appended. Keys are
// case-insensitive on Windows, matching the platform's environment lookup.
func Merge(base, overrides []string) []string {
	return merge(base, overrides, runtime.GOOS == "windows")
}

func merge(base, overrides []string, foldCase bool) []string {
	if len(overrides) == 0 {
		return base
	}
	key := func(entry string) string {
		if index := strings.IndexByte(entry, '='); index >= 0 {
			entry = entry[:index]
		}
		if foldCase {
			return strings.ToUpper(entry)
		}
		return entry
	}
	replaced := make(map[string]struct{}, len(overrides))
	for _, override := range overrides {
		replaced[key(override)] = struct{}{}
	}
	merged := make([]string, 0, len(base)+len(overrides))
	for _, entry := range base {
		if _, ok := replaced[key(entry)]; ok {
			continue
		}
		merged = append(merged, entry)
	}
	return append(merged, overrides...)
}

// Pairs renders a map as sorted KEY=VALUE entries, so hook and subshell
// environments are deterministic across runs.
func Pairs(values map[string]string) []string {
	if len(values) == 0 {
		return nil
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	pairs := make([]string, 0, len(keys))
	for _, key := range keys {
		pairs = append(pairs, key+"="+values[key])
	}
	return pairs
}
