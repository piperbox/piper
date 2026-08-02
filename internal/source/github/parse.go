package github

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/piperbox/piper/internal/source"
)

// maxPayloadCommits is the cap GitHub documents on a push payload's commits
// array. At the cap the array may be truncated, so the changed files it lists
// are only part of the push and Parse leaves Event.Paths nil instead.
const maxPayloadCommits = 2048

func (p *Provider) verify(headers http.Header, body []byte) error {
	sig := headers.Get("X-Hub-Signature-256")
	m := hmac.New(sha256.New, []byte(p.secret))
	m.Write(body)
	want := "sha256=" + hex.EncodeToString(m.Sum(nil))
	if !hmac.Equal([]byte(sig), []byte(want)) {
		return source.ErrBadSignature
	}
	return nil
}

func (p *Provider) Parse(headers http.Header, body []byte) (source.Event, error) {
	if err := p.verify(headers, body); err != nil {
		return source.Event{}, err
	}
	var payload struct {
		Ref     string `json:"ref"`
		After   string `json:"after"`
		Action  string `json:"action"`
		Number  int    `json:"number"`
		Commits []struct {
			Added    []string `json:"added"`
			Removed  []string `json:"removed"`
			Modified []string `json:"modified"`
		} `json:"commits"`
		PullRequest struct {
			Head struct {
				Ref string `json:"ref"`
				SHA string `json:"sha"`
			} `json:"head"`
		} `json:"pull_request"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		Installation struct {
			ID int64 `json:"id"`
		} `json:"installation"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return source.Event{}, fmt.Errorf("parse payload: %w", err)
	}
	ev := source.Event{
		Repo:           payload.Repository.FullName,
		InstallationID: payload.Installation.ID,
	}
	switch headers.Get("X-GitHub-Event") {
	case "ping":
		ev.Kind = source.KindPing
	case "push":
		ev.Kind = source.KindPush
		ev.Ref = payload.Ref
		ev.SHA = payload.After
		if len(payload.Commits) < maxPayloadCommits {
			for _, c := range payload.Commits {
				ev.Paths = append(ev.Paths, c.Added...)
				ev.Paths = append(ev.Paths, c.Removed...)
				ev.Paths = append(ev.Paths, c.Modified...)
			}
		}
	case "pull_request":
		ev.PR = payload.Number
		ev.Ref = payload.PullRequest.Head.Ref
		ev.SHA = payload.PullRequest.Head.SHA
		switch payload.Action {
		case "opened", "reopened":
			ev.Kind = source.KindPROpened
		case "synchronize":
			ev.Kind = source.KindPRSynced
		case "closed":
			ev.Kind = source.KindPRClosed
		default:
			ev.Kind = source.KindOther
		}
	default:
		ev.Kind = source.KindOther
	}
	return ev, nil
}
