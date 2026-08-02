package github

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/piperbox/piper/internal/source"
)

func sign(secret, body string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

func newTestProvider(t *testing.T, secret string) *Provider {
	t.Helper()
	p, err := New(Config{AppID: 1, PrivateKeyPEM: testKeyPEM(t), WebhookSecret: secret})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestParsePush(t *testing.T) {
	body, _ := os.ReadFile("testdata/push.json")
	p := newTestProvider(t, "s3cr3t")
	h := http.Header{}
	h.Set("X-GitHub-Event", "push")
	h.Set("X-Hub-Signature-256", sign("s3cr3t", string(body)))

	ev, err := p.Parse(h, body)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := source.Event{
		Repo: "alice/blog", Ref: "refs/heads/main", SHA: "abc123def456",
		Kind: source.KindPush, InstallationID: 99,
	}
	if !reflect.DeepEqual(ev, want) {
		t.Fatalf("got %+v want %+v", ev, want)
	}
}

func TestParsePushCollectsChangedPaths(t *testing.T) {
	body, _ := os.ReadFile("testdata/push_paths.json")
	p := newTestProvider(t, "s3cr3t")
	h := http.Header{}
	h.Set("X-GitHub-Event", "push")
	h.Set("X-Hub-Signature-256", sign("s3cr3t", string(body)))

	ev, err := p.Parse(h, body)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := []string{"apps/web/index.html", "README.md", "apps/web/old.css", "apps/api/main.go"}
	if !reflect.DeepEqual(ev.Paths, want) {
		t.Fatalf("paths = %v want %v", ev.Paths, want)
	}
}

// A push carrying more commits than the payload holds has a truncated commits
// array, so the collected paths would be an incomplete picture of the change.
// Parse reports no paths at all rather than a partial list a caller would
// mistake for the whole push.
func TestParsePushTruncatedCommitsReportsNoPaths(t *testing.T) {
	commits := make([]string, maxPayloadCommits)
	for i := range commits {
		commits[i] = `{"added":["apps/web/f.txt"],"removed":[],"modified":[]}`
	}
	body := `{"ref":"refs/heads/main","after":"abc123def456",` +
		`"repository":{"full_name":"alice/blog"},"installation":{"id":99},` +
		`"commits":[` + strings.Join(commits, ",") + `]}`
	p := newTestProvider(t, "s3cr3t")
	h := http.Header{}
	h.Set("X-GitHub-Event", "push")
	h.Set("X-Hub-Signature-256", sign("s3cr3t", body))

	ev, err := p.Parse(h, []byte(body))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if ev.Paths != nil {
		t.Fatalf("paths = %v want nil for a truncated commits array", ev.Paths)
	}
}

func TestParseBadSignature(t *testing.T) {
	body, _ := os.ReadFile("testdata/push.json")
	p := newTestProvider(t, "s3cr3t")
	h := http.Header{}
	h.Set("X-GitHub-Event", "push")
	h.Set("X-Hub-Signature-256", sign("WRONG", string(body)))

	if _, err := p.Parse(h, body); !errors.Is(err, source.ErrBadSignature) {
		t.Fatalf("want ErrBadSignature, got %v", err)
	}
}

func TestParsePing(t *testing.T) {
	body, _ := os.ReadFile("testdata/ping.json")
	p := newTestProvider(t, "s3cr3t")
	h := http.Header{}
	h.Set("X-GitHub-Event", "ping")
	h.Set("X-Hub-Signature-256", sign("s3cr3t", string(body)))

	ev, err := p.Parse(h, body)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if ev.Kind != source.KindPing {
		t.Fatalf("kind = %v", ev.Kind)
	}
}

func TestParsePullRequest(t *testing.T) {
	cases := []struct {
		file     string
		wantKind source.Kind
		wantSHA  string
	}{
		{"testdata/pr_opened.json", source.KindPROpened, "prsha42"},
		{"testdata/pr_synchronize.json", source.KindPRSynced, "prsha43"},
		{"testdata/pr_closed.json", source.KindPRClosed, "prsha43"},
	}
	p := newTestProvider(t, "s3cr3t")
	for _, c := range cases {
		body, _ := os.ReadFile(c.file)
		h := http.Header{}
		h.Set("X-GitHub-Event", "pull_request")
		h.Set("X-Hub-Signature-256", sign("s3cr3t", string(body)))
		ev, err := p.Parse(h, body)
		if err != nil {
			t.Fatalf("%s: Parse: %v", c.file, err)
		}
		want := source.Event{
			Repo: "alice/blog", Ref: "feature-x", SHA: c.wantSHA,
			Kind: c.wantKind, PR: 42, InstallationID: 99,
		}
		if !reflect.DeepEqual(ev, want) {
			t.Fatalf("%s: got %+v want %+v", c.file, ev, want)
		}
	}
}
