package relay

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
)

// maxWebhookBody mirrors the agent-side cap in internal/webhook.
const maxWebhookBody = 5 << 20

// Deliverer hands a verified, routed webhook to one bound agent.
type Deliverer interface {
	Deliver(ctx context.Context, b Binding, eventType string, payload []byte) error
	DrainFor(ctx context.Context, agentName string)
	// Dispatch runs fn on the bounded delivery pool, tracked so shutdown waits
	// for it and cancelled through fn's context when the relay stops.
	Dispatch(fn func(ctx context.Context))
}

// ghEnvelope is the slice of a GitHub webhook the relay needs to route. Payload
// interpretation stays on the box: the relay reads only the routing keys.
type ghEnvelope struct {
	Action     string `json:"action"`
	Ref        string `json:"ref"`
	Number     int    `json:"number"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	Installation struct {
		ID      int64 `json:"id"`
		Account struct {
			ID    int64  `json:"id"`
			Type  string `json:"type"`
			Login string `json:"login"`
		} `json:"account"`
	} `json:"installation"`
	Sender struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
	} `json:"sender"`
}

// NewGitHubIngress serves the App's single webhook URL. It verifies the App
// signature, keeps installation linkage current, and routes everything else to
// the bound agents of the installation's account. It never routes an event
// whose installation is not linked to an account.
func NewGitHubIngress(st *Store, app *GitHubApp, d Deliverer) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBody))
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		if !app.VerifySignature(r.Header.Get("X-Hub-Signature-256"), body) {
			http.Error(w, "bad signature", http.StatusUnauthorized)
			return
		}

		event := r.Header.Get("X-GitHub-Event")
		if event == "ping" {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "pong")
			return
		}

		var env ghEnvelope
		if err := json.Unmarshal(body, &env); err != nil {
			http.Error(w, "bad payload", http.StatusBadRequest)
			return
		}
		installationID := strconv.FormatInt(env.Installation.ID, 10)

		if event == "installation" || event == "installation_repositories" {
			handleInstallationEvent(st, env, installationID, event)
			w.WriteHeader(http.StatusAccepted)
			return
		}

		accountID, err := st.AccountForInstallation(installationID)
		if err != nil {
			if errors.Is(err, ErrNoInstallation) {
				// Unlinked installation: acknowledge so GitHub stops retrying, but
				// never route. This is the tenancy boundary.
				log.Printf("relay: %s event for unlinked installation %s", event, installationID)
				w.WriteHeader(http.StatusAccepted)
				return
			}
			log.Printf("relay: resolve account for installation %s: %v", installationID, err)
			http.Error(w, "routing error", http.StatusInternalServerError)
			return
		}
		bindings, err := st.BindingsForRepo(accountID, env.Repository.FullName)
		if err != nil {
			http.Error(w, "routing error", http.StatusInternalServerError)
			return
		}

		// Routing is by repository only. Whether the branch matches is the
		// agent's decision, exactly as in BYO mode; two components filtering the
		// same condition is how pushes end up deploying nowhere.
		w.WriteHeader(http.StatusAccepted)
		ref := strings.TrimPrefix(env.Ref, "refs/heads/")
		if env.Number > 0 {
			ref = "pr-" + strconv.Itoa(env.Number)
		}
		for _, b := range bindings {
			// Dispatch, not a bare go: one webhook fans out to every binding
			// for the repo, so the pool is what bounds a burst, and it is what
			// lets shutdown wait for these to park rather than dropping them.
			d.Dispatch(func(ctx context.Context) {
				err := d.Deliver(ctx, b, event, body)
				if err == nil {
					return
				}
				// Park on ANY failure, not just offline: GitHub already got its
				// 202, so an event dropped here would never be retried by
				// anyone. Parking is coalescing and idempotent, so this is
				// safe for transient box-side errors too.
				if !errors.Is(err, ErrAgentOffline) {
					log.Printf("relay: deliver %s to %s/%s: %v (parking)", event, b.AgentName, b.App, err)
				}
				if err := st.ParkEvent(b.AgentName, b.App, ref, event, body); err != nil {
					log.Printf("relay: park %s for %s/%s: %v", event, b.AgentName, b.App, err)
					return
				}
				// Close the park/drain race: the box may have reconnected while
				// the delivery was failing, in which case its reconnect drain
				// already ran and missed this event. DrainFor no-ops while the
				// agent is still offline. Skipped once shutting down — the
				// event is parked, and the sweep or a reconnect will take it.
				if ctx.Err() == nil {
					d.DrainFor(ctx, b.AgentName)
				}
			})
		}
	})
}

// handleInstallationEvent keeps github_installations in step with GitHub. It is
// written to be order-independent: the OAuth redirect and this webhook race.
//
// It takes both installation and installation_repositories. The latter fires
// when a user re-saves the repository selection of an installation that already
// exists — the only linking action GitHub's UI offers once the App is installed
// — and carries the same installation and sender as installation.created, so
// every one of its actions (added/removed) links the same way. Its sender is
// whoever re-saved the selection, though, not necessarily the owner, so it only
// ever recovers an unlinked installation — never reassigns a linked one.
func handleInstallationEvent(st *Store, env ghEnvelope, installationID, event string) {
	switch {
	case event == "installation_repositories",
		env.Action == "created", env.Action == "new_permissions_accepted", env.Action == "unsuspend":
		if event == "installation_repositories" {
			// The sender here is whoever re-saved the repository selection,
			// not necessarily the installation's owner, and the link below
			// upserts account_id. So this is only a recovery link for an
			// unlinked installation: an already-linked one keeps its owner.
			if _, err := st.AccountForInstallation(installationID); err == nil {
				log.Printf("relay: %s for already-linked installation %s; preserving owner", event, installationID)
				return
			} else if !errors.Is(err, ErrNoInstallation) {
				log.Printf("relay: resolve account for installation %s: %v", installationID, err)
				return
			}
		}
		senderID := strconv.FormatInt(env.Sender.ID, 10)
		login := env.Installation.Account.Login
		if env.Installation.Account.Type == "Organization" {
			// Route to the Piper org account this GitHub org is linked to,
			// verified through the installing sender's membership.
			orgGitHubID := strconv.FormatInt(env.Installation.Account.ID, 10)
			if orgID, err := st.OrgForGitHubInstall(orgGitHubID, login, senderID); err == nil {
				if err := st.LinkInstallationForAccount(installationID, orgID, "org", login); err != nil {
					log.Printf("relay: link org installation %s: %v", installationID, err)
				}
				return
			} else if !errors.Is(err, ErrNoOrg) {
				log.Printf("relay: resolve org for installation %s: %v", installationID, err)
			} else {
				// No linked Piper org: fall back to the installing user, so the
				// install still serves their personal boxes (unchanged behavior).
				log.Printf("relay: org-target installation %s has no linked piper org; linking installer %s", installationID, env.Sender.Login)
			}
		}
		typ := "user"
		if env.Installation.Account.Type == "Organization" {
			typ = "org"
		}
		if err := st.LinkInstallation(installationID, senderID, typ, login); err != nil {
			if errors.Is(err, ErrUnknownAccount) {
				// The commonest install failure, and the one a bare "unknown
				// account" leaves unactionable: name the installer so the log
				// says whose piper account is missing.
				log.Printf("relay: link installation %s: no piper account for installer %s (github id %s)",
					installationID, env.Sender.Login, senderID)
			} else {
				log.Printf("relay: link installation %s: %v", installationID, err)
			}
		}
	case env.Action == "deleted", env.Action == "suspend":
		if err := st.UnlinkInstallation(installationID); err != nil {
			log.Printf("relay: unlink installation %s: %v", installationID, err)
		}
	default:
		log.Printf("relay: ignoring %s event with action %q for installation %s", event, env.Action, installationID)
	}
}
