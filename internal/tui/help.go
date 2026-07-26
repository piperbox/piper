package tui

import tea "github.com/charmbracelet/bubbletea"

// helpView is a static, pushed overlay listing the full keymap. It holds no
// state and polls nothing; the root's esc/q pop dismisses it.
type helpView struct{}

func (helpView) Init() tea.Cmd { return nil }

func (helpView) title() string { return "help" }

func (helpView) refresh(API) tea.Cmd { return nil }

func (v helpView) Update(tea.Msg) (tea.Model, tea.Cmd) { return v, nil }

func (helpView) View() string {
	return "  Global      esc back/cancel · q quit (root) / back · t boxes · ? help · ctrl+c quit\n" +
		"  Apps list   ↑/k ↓/j move · enter open · n new app · L login · g github\n" +
		"  App detail  ↑/k ↓/j move · enter logs / domain · d deploy · s stop / start · x delete app / remove domain · l link · a add domain\n" +
		"  GitHub      ↵ sign in / open repos · ↑↓ move · m manifest app · o open install page · r retry repos\n" +
		"  Domain      live status + the CNAME to create · esc back\n" +
		"  Logs        f toggle follow · esc back\n" +
		"  Boxes       ↑/k ↓/j move · enter connect · a add · e edit · x remove\n\n" +
		"  esc back"
}
