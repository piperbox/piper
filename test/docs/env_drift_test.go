package docs_test

import (
	"regexp"
	"strings"
	"testing"
)

var envNameRE = regexp.MustCompile(`^PIPER_[A-Z_]+$`)

// envDirs are the packages that read PIPER_* variables: piperd's config
// loader and the four binaries. Test helper packages are not binaries and
// stay out.
var envDirs = []string{"internal/config", "cmd/piperd", "cmd/piper", "cmd/piper-relay", "cmd/piper-edge"}

// Every PIPER_* name a binary reads appears backticked in reference/env.md.
// Defaults and meanings are not checked; they stay human-reviewed.
func TestEnvReferenceCoversEveryVariable(t *testing.T) {
	page := readDoc(t, "reference/env.md")
	total := 0
	for _, dir := range envDirs {
		names := stringLiterals(t, dir, envNameRE.MatchString)
		total += len(names)
		for _, n := range names {
			if !strings.Contains(page, "`"+n+"`") {
				t.Errorf("reference/env.md lacks `%s` (read in %s)", n, dir)
			}
		}
	}
	if total == 0 {
		t.Fatal("no PIPER_* literals found; the predicate or paths are wrong")
	}
}
