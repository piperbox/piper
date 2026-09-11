// Package docs holds the documentation contract tests: the shape
// docs/manifest.json promises the dashboard's sync script, and the page rules
// docs/README.md states for anything the manifest publishes.
package docs_test

import (
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const docsDir = "../../docs"

type page struct {
	Slug  string `json:"slug"`
	File  string `json:"file"`
	Title string `json:"title"`
}

type manifest struct {
	Sections []struct {
		Title string `json:"title"`
		Pages []page `json:"pages"`
	} `json:"sections"`
}

func loadManifest(t *testing.T) manifest {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(docsDir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m manifest
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("manifest.json: %v", err)
	}
	return m
}

func publishedPages(m manifest) []page {
	var out []page
	for _, s := range m.Sections {
		out = append(out, s.Pages...)
	}
	return out
}

func readLines(t *testing.T, file string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(docsDir, filepath.FromSlash(file)))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(string(b), "\n")
}

var slugRE = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// Slugs are the site's URLs (/docs/<slug>): flat, lowercase, stable, unique.
func TestManifestPagesExistWithUniqueSlugs(t *testing.T) {
	m := loadManifest(t)
	for _, s := range m.Sections {
		if s.Title == "" {
			t.Error("a section has no title")
		}
	}
	seen := map[string]string{}
	for _, p := range publishedPages(m) {
		if !slugRE.MatchString(p.Slug) {
			t.Errorf("slug %q: want lowercase words joined by -", p.Slug)
		}
		if prev, dup := seen[p.Slug]; dup {
			t.Errorf("slug %q used by both %s and %s", p.Slug, prev, p.File)
		}
		seen[p.Slug] = p.File
		if p.Title == "" {
			t.Errorf("%s: empty title", p.File)
		}
		if _, err := os.Stat(filepath.Join(docsDir, filepath.FromSlash(p.File))); err != nil {
			t.Errorf("%s: %v", p.File, err)
		}
	}
}

// A published page opens with one H1 and then a lead paragraph. The dashboard
// index and llms.txt render the lead, so it must be plain prose: not a
// heading, list, fence, table, quote, or indented code. No frontmatter either
// (GitHub renders it as a table above the page), which the H1 check also
// enforces since a frontmatter block starts with ---.
func TestPublishedPagesOpenWithH1AndLead(t *testing.T) {
	for _, p := range publishedPages(loadManifest(t)) {
		lines := readLines(t, p.File)
		if len(lines) == 0 || !strings.HasPrefix(lines[0], "# ") {
			t.Errorf("%s: first line must be an H1", p.File)
			continue
		}
		lead := ""
		for _, l := range lines[1:] {
			if strings.TrimSpace(l) != "" {
				lead = l
				break
			}
		}
		if lead == "" {
			t.Errorf("%s: no lead paragraph after the H1", p.File)
			continue
		}
		for _, bad := range []string{"#", "-", "*", "```", "|", "    ", ">", "1."} {
			if strings.HasPrefix(lead, bad) {
				t.Errorf("%s: lead must be plain prose, got %q", p.File, lead)
				break
			}
		}
	}
}

var linkRE = regexp.MustCompile(`\]\(([^)\s]+)\)`)

// Relative links resolve the way GitHub resolves them: against the page's own
// directory. Anything in the repo is fair game except docs/ops/, which is
// never published and whose readers arrive from GitHub, not the site.
func TestPublishedPagesLinkToRealFilesAndNeverIntoOps(t *testing.T) {
	for _, p := range publishedPages(loadManifest(t)) {
		inFence := false
		for n, line := range readLines(t, p.File) {
			if strings.HasPrefix(strings.TrimSpace(line), "```") {
				inFence = !inFence
				continue
			}
			if inFence {
				continue
			}
			for _, m := range linkRE.FindAllStringSubmatch(line, -1) {
				href := m[1]
				if strings.HasPrefix(href, "http://") || strings.HasPrefix(href, "https://") ||
					strings.HasPrefix(href, "#") || strings.HasPrefix(href, "mailto:") {
					continue
				}
				target, _, _ := strings.Cut(href, "#")
				rel := path.Join(path.Dir(p.File), target)
				if strings.HasPrefix(rel, "ops/") {
					t.Errorf("%s:%d: links into docs/ops/ (%s), which is never published", p.File, n+1, href)
				}
				if _, err := os.Stat(filepath.Join(docsDir, filepath.FromSlash(rel))); err != nil {
					t.Errorf("%s:%d: broken link %s (resolved to docs/%s)", p.File, n+1, href, rel)
				}
			}
		}
	}
}
