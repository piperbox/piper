package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
)

func TestStopConfirmYesEmitsStop(t *testing.T) {
	v := newStopConfirm("blog")
	if !strings.Contains(v.View(), "Stop blog") {
		t.Fatalf("prompt missing:\n%s", v.View())
	}
	_, cmd := v.Update(keyRunes('y'))
	if _, ok := intentIn(t, cmd).(stopAppMsg); !ok {
		t.Fatalf("y should emit stopAppMsg, got %T", intentIn(t, cmd))
	}
}

func TestStartConfirmYesEmitsStart(t *testing.T) {
	v := newStartConfirm("blog")
	if !strings.Contains(v.View(), "Start blog") {
		t.Fatalf("prompt missing:\n%s", v.View())
	}
	_, cmd := v.Update(keyRunes('y'))
	if _, ok := intentIn(t, cmd).(startAppMsg); !ok {
		t.Fatalf("y should emit startAppMsg, got %T", intentIn(t, cmd))
	}
}

func TestStopConfirmNoPops(t *testing.T) {
	_, cmd := newStopConfirm("blog").Update(keyRunes('n'))
	if cmd == nil {
		t.Fatal("n should emit a command")
	}
	if pm, ok := cmd().(popMsg); !ok || pm.n != 1 {
		t.Fatalf("n should emit popMsg{1}, got %#v", cmd())
	}
}

func TestDeleteConfirmGatesOnTypedName(t *testing.T) {
	v := newDeleteConfirm("blog")
	// wrong name: enter does not delete, shows mismatch
	wrong := typeInto2(v, "bloop")
	m, cmd := wrong.Update(keyEnter())
	if cmd != nil {
		t.Fatal("mismatched name must not delete")
	}
	if !strings.Contains(m.(confirmView).View(), "match") {
		t.Fatalf("want mismatch banner:\n%s", m.(confirmView).View())
	}
	// resuming editing clears the stale mismatch banner
	edited := typeInto2(m.(confirmView), "s")
	if strings.Contains(edited.View(), "match") {
		t.Fatalf("mismatch banner should clear once the user resumes typing:\n%s", edited.View())
	}
	// exact name: enter deletes
	right := typeInto2(newDeleteConfirm("blog"), "blog")
	_, cmd = right.Update(keyEnter())
	if _, ok := intentIn(t, cmd).(deleteAppMsg); !ok {
		t.Fatalf("exact name should emit deleteAppMsg, got %v", intentIn(t, cmd))
	}
}

func TestRemoveDomainConfirmYesEmitsRemove(t *testing.T) {
	v := newRemoveDomainConfirm("blog", "blog.example.com")
	if !strings.Contains(v.View(), "Remove blog.example.com") {
		t.Fatalf("prompt missing:\n%s", v.View())
	}
	_, cmd := v.Update(keyRunes('y'))
	rm, ok := intentIn(t, cmd).(removeDomainMsg)
	if !ok || rm.app != "blog" || rm.domain != "blog.example.com" {
		t.Fatalf("want removeDomainMsg{blog, blog.example.com}, got %#v", intentIn(t, cmd))
	}
}

// typeInto2 feeds each rune of s into a confirmView's text input.
func typeInto2(v confirmView, s string) confirmView {
	for _, r := range s {
		m, _ := v.Update(keyRunes(r))
		v = m.(confirmView)
	}
	return v
}

// intentIn returns the non-spinner message a confirm's command carries.
// Confirming now batches the intent with the spinner's first tick (#381), so
// the intent is one message among several rather than the command's whole
// result.
func intentIn(t *testing.T, cmd tea.Cmd) tea.Msg {
	t.Helper()
	if cmd == nil {
		t.Fatal("confirm returned no command")
	}
	msg := cmd()
	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		return msg
	}
	var found tea.Msg
	for _, c := range batch {
		if c == nil {
			continue
		}
		m := c()
		if _, isTick := m.(spinner.TickMsg); isTick {
			continue
		}
		if found != nil {
			t.Fatalf("more than one non-spinner message in the batch: %T and %T", found, m)
		}
		found = m
	}
	if found == nil {
		t.Fatal("no intent message in the batch")
	}
	return found
}

// TestConfirmShowsProgressWhileRunning pins #381: stop and start take real time
// on the box — Start waits up to 30s for a health check — and the modal used to
// keep showing the y/n prompt throughout, so a confirmed action looked like a
// keypress that did nothing.
func TestConfirmShowsProgressWhileRunning(t *testing.T) {
	base := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	v := newStartConfirm("blog")
	v.now = func() time.Time { return base }

	m, _ := v.Update(keyRunes('y'))
	running := m.(confirmView)
	running.now = func() time.Time { return base.Add(12 * time.Second) }

	got := running.View()
	if !strings.Contains(got, "starting…") {
		t.Errorf("want the action verb while in flight:\n%s", got)
	}
	if !strings.Contains(got, "12s") {
		t.Errorf("want the elapsed seconds while in flight:\n%s", got)
	}
	if strings.Contains(got, "y  yes") {
		t.Errorf("the y/n legend should give way to the progress line:\n%s", got)
	}
}

// TestConfirmIgnoresKeysWhileRunning covers the double-submit guard and the
// deliberate choice to hold the modal: the root pops a fixed number of views
// when the result lands, so letting esc pop it early would eat the view beneath.
func TestConfirmIgnoresKeysWhileRunning(t *testing.T) {
	m, _ := newStopConfirm("blog").Update(keyRunes('y'))
	running := m.(confirmView)

	for _, k := range []tea.KeyMsg{keyRunes('y'), keyRunes('n'), {Type: tea.KeyEsc}} {
		next, cmd := running.Update(k)
		if cmd != nil {
			t.Fatalf("%v should be inert while the action is in flight, got a command", k)
		}
		if !next.(confirmView).running {
			t.Fatalf("%v cleared the in-flight state", k)
		}
	}
}

// TestConfirmErrorRestoresPrompt: a failed action must be retryable, not leave
// the modal spinning forever.
func TestConfirmErrorRestoresPrompt(t *testing.T) {
	m, _ := newStartConfirm("blog").Update(keyRunes('y'))
	failed, _ := m.(confirmView).Update(errMsg{errStub("relay unreachable")})

	got := failed.(confirmView).View()
	if failed.(confirmView).running {
		t.Error("still marked running after an error")
	}
	if !strings.Contains(got, "y  yes") {
		t.Errorf("want the y/n legend back so it can be retried:\n%s", got)
	}
	if !strings.Contains(got, "relay unreachable") {
		t.Errorf("want the error banner:\n%s", got)
	}
}

type errStub string

func (e errStub) Error() string { return string(e) }
