package tui

import (
	"fmt"
	"time"

	"github.com/piperbox/piper/internal/api"
)

// pluralApps renders an app count for the status bar ("1 app", "3 apps").
func pluralApps(n int) string {
	if n == 1 {
		return "1 app"
	}
	return fmt.Sprintf("%d apps", n)
}

// statusIcon maps a deployment status to its one-glyph indicator; "" (never
// deployed) and unknown values render as "—".
func statusIcon(status string) string {
	switch status {
	case "running":
		return "●"
	case "building":
		return "◐"
	case "failed":
		return "✗"
	case "stopped":
		return "○"
	}
	return "—"
}

// domainStatusIcon maps a per-app custom-domain status (#285) to its one-glyph
// indicator; unknown values render as "—".
func domainStatusIcon(status string) string {
	switch status {
	case "active":
		return "●"
	case "issuing":
		return "◐"
	case "pending":
		return "◌"
	case "failed":
		return "✗"
	}
	return "—"
}

// relTime renders a compact "time ago" for the deployments table.
func relTime(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	switch d := time.Since(t); {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// shortID trims a deployment id for table display.
func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// repoLine renders an app's source for the detail header.
func repoLine(a api.App) string {
	if a.Repo == "" {
		return "(no repo)"
	}
	if a.Branch != "" {
		return a.Repo + "@" + a.Branch
	}
	return a.Repo
}
