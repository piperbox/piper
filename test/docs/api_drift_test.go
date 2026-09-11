package docs_test

import (
	"regexp"
	"strings"
	"testing"
)

// Route patterns as registered on the mux: "METHOD /v1/...".
var routeRE = regexp.MustCompile(`^([A-Z]+ )?/v1/`)

// Every route internal/api registers has its own "### METHOD /v1/path"
// heading in reference/api.md. Bodies and status lists are not checked; they
// stay human-reviewed.
func TestAPIReferenceCoversEveryRoute(t *testing.T) {
	routes := stringLiterals(t, "internal/api", routeRE.MatchString)
	if len(routes) == 0 {
		t.Fatal("no route literals found under internal/api; the predicate or path is wrong")
	}
	page := readDoc(t, "reference/api.md")
	for _, r := range routes {
		if !strings.Contains(page, "\n### "+r+"\n") {
			t.Errorf("reference/api.md lacks a heading `### %s`", r)
		}
	}
}
