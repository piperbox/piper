package source_test

import (
	"testing"

	"github.com/piperbox/piper/internal/source"
)

func TestKindString(t *testing.T) {
	cases := map[source.Kind]string{
		source.KindPush:  "push",
		source.KindPing:  "ping",
		source.KindOther: "other",
	}
	for k, want := range cases {
		if got := k.String(); got != want {
			t.Errorf("Kind(%d).String() = %q, want %q", k, got, want)
		}
	}
}

func TestValidRepo(t *testing.T) {
	valid := []string{"octo/blog", "Alice/My_Repo.js", "a/b"}
	for _, r := range valid {
		if !source.ValidRepo(r) {
			t.Errorf("ValidRepo(%q) = false, want true", r)
		}
	}
	// "next" is the shape that motivated this (#333): accepted at link time,
	// then silently breaks every consumer downstream.
	invalid := []string{"next", "", "/", "next/", "/next", "octo/blog/extra"}
	for _, r := range invalid {
		if source.ValidRepo(r) {
			t.Errorf("ValidRepo(%q) = true, want false", r)
		}
	}
}

func TestStatusInactiveDistinct(t *testing.T) {
	all := []source.Status{
		source.StatusPending, source.StatusSuccess,
		source.StatusFailure, source.StatusInactive,
	}
	seen := map[source.Status]bool{}
	for _, s := range all {
		if seen[s] {
			t.Fatalf("duplicate status value %d", s)
		}
		seen[s] = true
	}
}
