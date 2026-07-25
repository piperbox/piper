package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// confirmMode distinguishes a y/n confirm from a type-the-name confirm.
type confirmMode int

const (
	confirmYesNo confirmMode = iota
	confirmTypeName
)

// confirmView is a modal confirm pushed over the app-detail view: y/n for stop,
// type-the-app-name for delete (a stronger guard than the CLI's y/N, since
// delete is irreversible). On confirm it emits the pending intent; "no"/mismatch
// cancels via the root.
type confirmView struct {
	name   string
	prompt string
	mode   confirmMode
	intent func(string) tea.Msg
	input  textinput.Model
	err    error

	// In-flight state. These actions do real work on the box — Start waits up
	// to 30s for a health check, Stop for Docker's stop grace — and the modal
	// used to keep showing the y/n prompt throughout, so confirming looked like
	// a keypress that did nothing (#381). While running, the spinner animates
	// (proving the UI is live) beside the elapsed seconds, and keys are inert:
	// it guards against double-submit, and the root pops a fixed number of
	// views when the result lands, so dismissing the modal early would pop the
	// view beneath it.
	verb    string // "starting", "stopping", … shown while running
	running bool
	spin    spinner.Model
	started time.Time
	now     func() time.Time // seam: deterministic elapsed in tests
}

// newConfirm builds the shared scaffolding every confirm needs.
func newConfirm(name, prompt, verb string, mode confirmMode, intent func(string) tea.Msg) confirmView {
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	return confirmView{
		name:   name,
		prompt: prompt,
		verb:   verb,
		mode:   mode,
		intent: intent,
		spin:   sp,
		now:    time.Now,
	}
}

// clock is v.now with a safe default. newConfirm always sets it, but a
// confirmView assembled as a bare struct literal would otherwise nil-panic the
// moment it was confirmed — a real crash during this change, not a
// hypothetical.
func (v confirmView) clock() time.Time {
	if v.now == nil {
		return time.Now()
	}
	return v.now()
}

// confirm marks the action in flight and fires it alongside the spinner's first
// tick; the spinner's ticks are also what re-render the elapsed counter.
func (v confirmView) confirm() (tea.Model, tea.Cmd) {
	v.running, v.started, v.err = true, v.clock(), nil
	return v, tea.Batch(v.spin.Tick, func() tea.Msg { return v.intent(v.name) })
}

func newStopConfirm(name string) confirmView {
	return newConfirm(name,
		fmt.Sprintf("Stop %s? Its running deployment will be halted.", name),
		"stopping", confirmYesNo,
		func(n string) tea.Msg { return stopAppMsg{n} })
}

func newStartConfirm(name string) confirmView {
	return newConfirm(name,
		fmt.Sprintf("Start %s? Its latest deployment is brought back up.", name),
		"starting", confirmYesNo,
		func(n string) tea.Msg { return startAppMsg{n} })
}

func newRemoveBoxConfirm(name string) confirmView {
	return newConfirm(name,
		fmt.Sprintf("Remove box %s? Its saved credentials are deleted from this machine.", name),
		"removing", confirmYesNo,
		func(n string) tea.Msg { return removeBoxMsg{n} })
}

func newRemoveDomainConfirm(app, dom string) confirmView {
	return newConfirm(dom,
		fmt.Sprintf("Remove %s from %s? Its certificate and route are torn down.", dom, app),
		"removing", confirmYesNo,
		func(d string) tea.Msg { return removeDomainMsg{app: app, domain: d} })
}

func newDeleteConfirm(name string) confirmView {
	in := textinput.New()
	in.Placeholder = name
	in.Focus()
	v := newConfirm(name,
		fmt.Sprintf("Delete %s? This cannot be undone. Type the app name to confirm.", name),
		"deleting", confirmTypeName,
		func(n string) tea.Msg { return deleteAppMsg{n} })
	v.input = in
	return v
}

func (v confirmView) Init() tea.Cmd { return nil }

func (v confirmView) title() string { return "confirm" }

func (v confirmView) refresh(API) tea.Cmd { return nil }

// capturesText is true only in type-name (delete) mode, where the app name is
// typed into a field; the y/n mode leaves the root's shortcuts active.
func (v confirmView) capturesText() bool { return v.mode == confirmTypeName }

func (v confirmView) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case errMsg:
		// The action failed: stop spinning and restore the prompt so it can be
		// retried, rather than leaving the modal running forever.
		v.err, v.running = msg.err, false
		return v, nil
	case spinner.TickMsg:
		if !v.running {
			return v, nil
		}
		var cmd tea.Cmd
		v.spin, cmd = v.spin.Update(msg)
		return v, cmd
	case tea.KeyMsg:
		if v.running {
			return v, nil // inert until the result lands
		}
		if v.mode == confirmYesNo {
			switch msg.String() {
			case "y":
				return v.confirm()
			case "n":
				return v, func() tea.Msg { return popMsg{1} }
			}
			return v, nil
		}
		// type-name mode
		if msg.Type == tea.KeyEnter {
			if strings.TrimSpace(v.input.Value()) == v.name {
				return v.confirm()
			}
			v.err = fmt.Errorf("that doesn't match %q", v.name)
			return v, nil
		}
		// Any editing keystroke clears a stale mismatch banner.
		v.err = nil
		var cmd tea.Cmd
		v.input, cmd = v.input.Update(msg)
		return v, cmd
	}
	return v, nil
}

func (v confirmView) View() string {
	var b strings.Builder
	fmt.Fprintf(&b, "  %s\n\n", v.prompt)
	if v.running {
		fmt.Fprintf(&b, "  %s%s… %s", v.spin.View(), v.verb,
			v.clock().Sub(v.started).Round(time.Second))
		return b.String()
	}
	if v.mode == confirmYesNo {
		b.WriteString("  y  yes      n / esc  no")
	} else {
		fmt.Fprintf(&b, "  %s\n\n  ↵ confirm   esc cancel", v.input.View())
	}
	if v.err != nil {
		fmt.Fprintf(&b, "\n\n ⚠ %v", v.err)
	}
	return b.String()
}
