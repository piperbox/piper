package docs_test

import (
	"strings"
	"testing"
)

// Every usage line the CLI can print appears verbatim (minus the "usage: "
// prefix) in reference/cli.md, so a verb cannot be added or reshaped without
// its reference entry moving with it. Prose and flag descriptions are not
// checked; they stay human-reviewed.
func TestCLIReferenceCoversEveryUsageLine(t *testing.T) {
	usages := stringLiterals(t, "cmd/piper", func(s string) bool {
		return strings.HasPrefix(s, "usage: piper")
	})
	if len(usages) == 0 {
		t.Fatal("no \"usage: piper\" literals found under cmd/piper; the predicate or path is wrong")
	}
	page := readDoc(t, "reference/cli.md")
	for _, u := range usages {
		synopsis := strings.TrimPrefix(u, "usage: ")
		if !strings.Contains(page, synopsis) {
			t.Errorf("reference/cli.md lacks the synopsis %q", synopsis)
		}
	}
}
