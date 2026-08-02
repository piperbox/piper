// Package source defines the provider seam: normalizing a git host's webhook
// into an Event, fetching the repo at a commit, and reporting status back.
package source

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

type Kind int

const (
	KindOther Kind = iota
	KindPing
	KindPush
	KindPROpened
	KindPRSynced
	KindPRClosed
)

func (k Kind) String() string {
	switch k {
	case KindPing:
		return "ping"
	case KindPush:
		return "push"
	case KindPROpened:
		return "pr_opened"
	case KindPRSynced:
		return "pr_synced"
	case KindPRClosed:
		return "pr_closed"
	default:
		return "other"
	}
}

type Status int

const (
	StatusPending Status = iota
	StatusSuccess
	StatusFailure
	StatusInactive
)

// Event is a normalized git host event.
type Event struct {
	Repo           string // "owner/name"
	Ref            string // "refs/heads/main"
	SHA            string
	Kind           Kind
	PR             int
	InstallationID int64
	// Paths are the repo-relative files the push changed, unordered and
	// possibly repeated across commits. Nil means the host didn't report a
	// complete list — too many commits to fit the payload, or a force push,
	// whose commits describe what it added rather than how the branch moved —
	// and callers must read it as "assume everything changed" rather than
	// "nothing changed".
	Paths []string
}

// Provider drives a deploy from a git host.
type Provider interface {
	// Parse verifies the signature and normalizes a raw webhook into an Event.
	Parse(headers http.Header, body []byte) (Event, error)
	// Fetch downloads the repo tree at ev.SHA into destDir.
	Fetch(ctx context.Context, ev Event, destDir string) error
	// Report posts a deploy status back to the git host (url set on success).
	Report(ctx context.Context, ev Event, status Status, url string) error
}

// ErrBadSignature is returned by Parse when signature verification fails; the
// webhook handler maps it to HTTP 401.
var ErrBadSignature = errors.New("source: bad webhook signature")

// RepoShape is the human-readable form ValidRepo accepts, for error messages.
const RepoShape = "owner/name"

// ValidRepo reports whether repo is shaped like an Event.Repo — exactly one
// "/" with both halves non-empty. Everything downstream of a link assumes it:
// the relay cuts the owner off to match installation target logins, push
// events are matched against the payload's full_name, and tarball fetches
// build /repos/<repo>/tarball/<ref>. A bare name satisfies none of those and
// fails silently rather than loudly, so it is rejected where it enters (#333).
func ValidRepo(repo string) bool {
	owner, name, ok := strings.Cut(repo, "/")
	return ok && owner != "" && name != "" && !strings.Contains(name, "/")
}
