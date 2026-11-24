package env

import (
	"reflect"
	"testing"
)

func TestMerge_OverridesAndDropsDuplicates(t *testing.T) {
	base := []string{"A=1", "B=2", "PATH=/bin"}
	overrides := []string{"A=9", "C=3"}
	got := merge(base, overrides, false)
	want := []string{"B=2", "PATH=/bin", "A=9", "C=3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("merge = %v, want %v", got, want)
	}
}

func TestMerge_NoOverridesReturnsBase(t *testing.T) {
	base := []string{"A=1"}
	if got := merge(base, nil, false); !reflect.DeepEqual(got, base) {
		t.Fatalf("merge = %v, want %v", got, base)
	}
}

func TestMerge_CaseFolding(t *testing.T) {
	base := []string{"Path=/bin", "A=1"}
	overrides := []string{"PATH=/usr/bin"}

	if got := merge(base, overrides, true); !reflect.DeepEqual(got, []string{"A=1", "PATH=/usr/bin"}) {
		t.Fatalf("case-folded merge = %v", got)
	}
	if got := merge(base, overrides, false); !reflect.DeepEqual(got, []string{"Path=/bin", "A=1", "PATH=/usr/bin"}) {
		t.Fatalf("case-sensitive merge = %v", got)
	}
}

func TestPairs_Sorted(t *testing.T) {
	got := Pairs(map[string]string{"B": "2", "A": "1"})
	if want := []string{"A=1", "B=2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("pairs = %v, want %v", got, want)
	}
	if Pairs(nil) != nil {
		t.Fatal("expected nil for an empty map")
	}
}
