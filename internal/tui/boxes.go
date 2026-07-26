package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/piperbox/piper/internal/config"
)

// boxesView is the depth-1 box switcher/editor: a table of the configured boxes
// read fresh from the client config. ↵ connects (switches the active box), a/e
// add/edit via a form, x removes. It is the one view that owns local config
// state rather than piperd state. Relay-only boxes (no LAN address) are listed
// but not switchable.
type boxesView struct {
	dial    Dialer
	boxes   []config.Box
	current string
	loaded  bool
	cursor  int
	err     error
	notice  string          // why enter was refused; view-local (an errMsg cmd would banner the current box as down), survives reloads, cleared on cursor move
	reach   map[string]bool // box name -> last probe result; absent = probing
}

func newBoxesView(dial Dialer) boxesView {
	return boxesView{dial: dial, reach: map[string]bool{}}
}

func (v boxesView) Init() tea.Cmd { return nil }

func (v boxesView) title() string { return "boxes" }

func (v boxesView) footer() string {
	return "↵ connect · a add · e edit · x remove · esc back"
}

// refresh reloads the client config off the UI thread.
func (v boxesView) refresh(API) tea.Cmd {
	return func() tea.Msg {
		cf, err := config.LoadClientFile()
		if err != nil {
			return errMsg{err}
		}
		return boxesLoadedMsg{boxes: cf.Boxes, current: cf.Current}
	}
}

// relayOnly reports whether the box at i is reachable only through the relay
// (no LAN address, so not switchable here). A box with both a LAN address and
// relay creds — a relay-enrolled box on the local network — is switchable.
func (v boxesView) relayOnly(i int) bool {
	return v.boxes[i].RelayAPI != "" && v.boxes[i].Addr == ""
}

// probe returns a cmd that dials box and calls ListApps; reachable is true iff
// both succeed. One cmd per box keeps a dead box from blocking the others.
func (v boxesView) probe(box config.Box) tea.Cmd {
	dial := v.dial
	return func() tea.Msg {
		c, _, _, err := dial(box)
		if err == nil {
			_, err = c.ListApps()
		}
		return boxProbeMsg{name: box.Name, reachable: err == nil}
	}
}

func (v boxesView) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case boxesLoadedMsg:
		v.boxes, v.current, v.loaded, v.err = msg.boxes, msg.current, true, nil
		v.reach = map[string]bool{}
		if v.cursor >= len(v.boxes) {
			v.cursor = max(0, len(v.boxes)-1)
		}
		var probes []tea.Cmd
		for i, box := range v.boxes {
			if box.Name == v.current || v.relayOnly(i) {
				continue
			}
			probes = append(probes, v.probe(box))
		}
		return v, tea.Batch(probes...)
	case boxProbeMsg:
		v.reach[msg.name] = msg.reachable
	case errMsg:
		v.err = msg.err
	case tea.KeyMsg:
		switch msg.String() {
		case "up", "k":
			v.notice = ""
			if v.cursor > 0 {
				v.cursor--
			}
		case "down", "j":
			v.notice = ""
			if v.cursor < len(v.boxes)-1 {
				v.cursor++
			}
		case "enter":
			if len(v.boxes) == 0 {
				break
			}
			if v.relayOnly(v.cursor) {
				v.notice = fmt.Sprintf("%s has no LAN address: relay boxes are driven with `piper --remote <domain>`", v.boxes[v.cursor].Name)
				return v, nil
			}
			box := v.boxes[v.cursor]
			return v, func() tea.Msg { return switchBoxMsg{box: box} }
		case "a":
			boxes := v.boxes
			return v, func() tea.Msg { return pushMsg{newBoxForm(v.dial, boxes)} }
		case "e":
			if len(v.boxes) > 0 {
				boxes, orig := v.boxes, v.boxes[v.cursor]
				return v, func() tea.Msg { return pushMsg{newBoxFormEdit(v.dial, boxes, orig)} }
			}
		case "x":
			if len(v.boxes) > 0 {
				name := v.boxes[v.cursor].Name
				return v, func() tea.Msg { return pushMsg{newRemoveBoxConfirm(name)} }
			}
		}
	}
	return v, nil
}

func (v boxesView) View() string {
	var b strings.Builder
	if v.err != nil {
		fmt.Fprintf(&b, " ⚠ %v\n\n", v.err)
	}
	if !v.loaded {
		b.WriteString(" loading…")
		return b.String()
	}
	fmt.Fprintf(&b, "  %-16s %-22s %s\n", "NAME", "ADDR", "STATUS")
	for i, box := range v.boxes {
		cursor := "  "
		if i == v.cursor {
			cursor = "▸ "
		}
		fmt.Fprintf(&b, "%s%-16s %-22s %s\n", cursor, box.Name, box.Addr, v.status(i))
	}
	if v.notice != "" {
		fmt.Fprintf(&b, "\n ⚠ %s\n", v.notice)
	}
	return b.String()
}

func (v boxesView) status(i int) string {
	switch {
	case v.boxes[i].Name == v.current:
		return "current"
	case v.relayOnly(i):
		return "—"
	}
	reachable, probed := v.reach[v.boxes[i].Name]
	switch {
	case !probed:
		return "…"
	case reachable:
		return "●"
	default:
		return "○"
	}
}

// saveBox writes box to the client config: it updates the box named replacing
// (whose name may change), else appends box. All other boxes are preserved; a
// first box (empty config) becomes current. replacing == "" means add.
func saveBox(box config.Box, replacing string) error {
	cf, err := config.LoadClientFile()
	if err != nil {
		return err
	}
	if replacing != "" {
		for i := range cf.Boxes {
			if cf.Boxes[i].Name == replacing {
				if cf.Current == replacing {
					cf.Current = box.Name
				}
				cf.Boxes[i] = box
				return config.SaveClientFile(cf)
			}
		}
	}
	cf.Boxes = append(cf.Boxes, box)
	if cf.Current == "" {
		cf.Current = box.Name
	}
	return config.SaveClientFile(cf)
}

// removeBox drops the box named name from the client config. If it was the
// current box, the first remaining box is promoted and returned with
// changed=true. Removing the last box is refused (the CLI always needs one).
func removeBox(name string) (current config.Box, changed bool, err error) {
	cf, err := config.LoadClientFile()
	if err != nil {
		return config.Box{}, false, err
	}
	if len(cf.Boxes) <= 1 {
		return config.Box{}, false, fmt.Errorf("can't remove the last box")
	}
	kept := cf.Boxes[:0]
	for _, b := range cf.Boxes {
		if b.Name != name {
			kept = append(kept, b)
		}
	}
	cf.Boxes = kept
	if cf.Current == name {
		cf.Current = cf.Boxes[0].Name
		current, changed = cf.Boxes[0], true
	}
	return current, changed, config.SaveClientFile(cf)
}
