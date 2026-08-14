package tui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/piperbox/piper/internal/config"
	"github.com/piperbox/piper/internal/relayclient"
)

// seedConfig points HOME at a temp dir and writes cf there, so config
// Load/Save in the view hit an isolated file.
func seedConfig(t *testing.T, cf config.ClientFile) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	if err := config.SaveClientFile(cf); err != nil {
		t.Fatalf("seed config: %v", err)
	}
}

// fakeDialer returns a Dialer that always yields the given result.
func fakeDialer(c API, addr string, remote bool, err error) Dialer {
	return func(config.Box) (API, string, bool, error) { return c, addr, remote, err }
}

// boxWithBaseDomain adds the persisted agent identity used by merge tests.
func boxWithBaseDomain(t *testing.T, box config.Box, baseDomain string) config.Box {
	t.Helper()
	box.BaseDomain = baseDomain
	return box
}

func persistedBaseDomain(t *testing.T, box config.Box) string {
	t.Helper()
	return box.BaseDomain
}

func commandMessages(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		messages := make([]tea.Msg, 0, len(batch))
		for _, subcmd := range batch {
			if submsg := subcmd(); submsg != nil {
				messages = append(messages, submsg)
			}
		}
		return messages
	}
	if msg == nil {
		return nil
	}
	return []tea.Msg{msg}
}

// newRelayBoxesView uses the same root wiring as the product: the root injects
// its relay factory when it pushes the boxes view.
func newRelayBoxesView(t *testing.T, dial Dialer, relay RelayDialer) boxesView {
	t.Helper()
	m := NewModel("local", "", false, fakeAPI{}).WithDialer(dial).WithRelay(relay)
	_, cmd := m.Update(keyRunes('t'))
	push, ok := cmd().(pushMsg)
	if !ok {
		t.Fatalf("t should push boxesView, got %T", cmd())
	}
	return push.view.(boxesView)
}

func agentsServer(t *testing.T, agents []relayclient.Agent) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/agents" {
			t.Errorf("agents request = %s %s, want GET /agents", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer cred-xyz" {
			t.Errorf("agents authorization = %q, want bearer credential", got)
		}
		_ = json.NewEncoder(w).Encode(struct {
			Agents []relayclient.Agent `json:"agents"`
		}{Agents: agents})
	}))
}

func TestBoxesViewLoadsFromConfig(t *testing.T) {
	// refresh reads the seeded config off disk and yields boxesLoadedMsg.
	seedConfig(t, config.ClientFile{
		Boxes:   []config.Box{{Name: "pi4", Addr: "192.168.1.6:8088"}, {Name: "blog", Addr: "192.168.1.9:8088"}},
		Current: "pi4",
	})
	v := newBoxesView(fakeDialer(fakeAPI{}, "", false, nil))
	msg := v.refresh(fakeAPI{})()
	loaded, ok := msg.(boxesLoadedMsg)
	if !ok {
		t.Fatalf("refresh should yield boxesLoadedMsg, got %T", msg)
	}
	if len(loaded.boxes) != 2 || loaded.current != "pi4" {
		t.Fatalf("config not loaded: %+v current=%q", loaded.boxes, loaded.current)
	}
}

func TestBoxesViewListsBoxesAndMarksCurrent(t *testing.T) {
	v := newBoxesView(fakeDialer(fakeAPI{}, "", false, nil))
	vv, _ := v.Update(boxesLoadedMsg{
		boxes:   []config.Box{{Name: "pi4", Addr: "192.168.1.6:8088"}, {Name: "blog", Addr: "192.168.1.9:8088"}},
		current: "pi4",
		viewID:  v.viewID, requestID: 1,
	})
	out := vv.(boxesView).View()
	for _, want := range []string{"pi4", "192.168.1.6:8088", "blog", "current"} {
		if !strings.Contains(out, want) {
			t.Fatalf("boxes view missing %q:\n%s", want, out)
		}
	}
}

func TestTPushesBoxesView(t *testing.T) {
	m := NewModel("pi4", "addr", false, fakeAPI{}).WithDialer(fakeDialer(fakeAPI{}, "", false, nil))
	_, cmd := m.Update(keyRunes('t'))
	if cmd == nil {
		t.Fatal("t should push a boxes view")
	}
	push, ok := cmd().(pushMsg)
	if !ok {
		t.Fatalf("want pushMsg, got %T", cmd())
	}
	if _, ok := push.view.(boxesView); !ok {
		t.Fatalf("want boxesView pushed, got %T", push.view)
	}
}

func TestTDoesNotStackBoxes(t *testing.T) {
	m := NewModel("pi4", "addr", false, fakeAPI{}).WithDialer(fakeDialer(fakeAPI{}, "", false, nil))
	m2, _ := m.Update(pushMsg{newBoxesView(m.dial)})
	m = m2.(Model)
	depth := len(m.stack)
	_, cmd := m.Update(keyRunes('t'))
	if cmd != nil {
		if _, ok := cmd().(pushMsg); ok {
			t.Fatal("t on the boxes view must not push a second boxes view")
		}
	}
	if len(m.stack) != depth {
		t.Fatalf("stack depth changed: %d -> %d", depth, len(m.stack))
	}
}

func TestEnterOnBoxEmitsSwitch(t *testing.T) {
	v := newBoxesView(fakeDialer(fakeAPI{}, "", false, nil))
	vv, _ := v.Update(boxesLoadedMsg{boxes: []config.Box{{Name: "pi4"}, {Name: "blog", Addr: "a"}}, current: "pi4", viewID: v.viewID, requestID: 1})
	v = vv.(boxesView)
	// cursor starts at 0 (pi4, current); move to blog and connect
	vv, _ = v.Update(keyRunes('j'))
	v = vv.(boxesView)
	_, cmd := v.Update(keyEnter())
	if cmd == nil {
		t.Fatal("enter should emit a switch")
	}
	sw, ok := cmd().(switchBoxMsg)
	if !ok || sw.box.Name != "blog" {
		t.Fatalf("want switchBoxMsg for blog, got %#v", cmd())
	}
}

func TestEnterOnLANBoxWithRelayCredsEmitsSwitch(t *testing.T) {
	// A LAN-addressable box that also carries relay creds (a relay-enrolled
	// box on the local network) must still be switchable via its LAN address.
	v := newBoxesView(fakeDialer(fakeAPI{}, "", false, nil))
	vv, _ := v.Update(boxesLoadedMsg{
		boxes:   []config.Box{{Name: "pi4"}, {Name: "cloud", Addr: "192.168.1.6:8088", RelayAPI: "https://r.example"}},
		current: "pi4",
		viewID:  v.viewID, requestID: 1,
	})
	v = vv.(boxesView)
	vv, _ = v.Update(keyRunes('j'))
	v = vv.(boxesView)
	_, cmd := v.Update(keyEnter())
	if cmd == nil {
		t.Fatal("enter on a LAN box with relay creds should emit a switch")
	}
	sw, ok := cmd().(switchBoxMsg)
	if !ok || sw.box.Name != "cloud" {
		t.Fatalf("want switchBoxMsg for cloud, got %#v", cmd())
	}
}

func TestEnterOnRelayOnlyBoxEmitsSwitch(t *testing.T) {
	load := boxesLoadedMsg{
		boxes:   []config.Box{{Name: "pi4"}, {Name: "cloud.example", RelayAPI: "https://r.example", AccountCredential: "cred-xyz"}},
		current: "pi4",
	}
	v := newBoxesView(fakeDialer(fakeAPI{}, "", false, nil))
	load.viewID, load.requestID = v.viewID, 1
	vv, _ := v.Update(load)
	v = vv.(boxesView)
	vv, _ = v.Update(keyRunes('j'))
	v = vv.(boxesView)
	_, cmd := v.Update(keyEnter())
	if cmd == nil {
		t.Fatal("enter on a relay-only box should emit a switch")
	}
	msg := cmd()
	sw, ok := msg.(switchBoxMsg)
	if !ok || sw.box.Name != "cloud.example" {
		t.Fatalf("want switchBoxMsg for cloud.example, got %#v", msg)
	}
}

func TestBoxesViewRefreshIncludesRelayAgentsAndUsesRelayLiveness(t *testing.T) {
	relay := agentsServer(t, []relayclient.Agent{{BaseDomain: "cloud.example", Connected: true}})
	defer relay.Close()
	seedConfig(t, config.ClientFile{
		Boxes:   []config.Box{{Name: "local", Addr: "192.168.1.6:8088", RelayAPI: relay.URL, AccountCredential: "cred-xyz"}},
		Current: "local",
	})

	var dialed []string
	dial := func(box config.Box) (API, string, bool, error) {
		dialed = append(dialed, box.Name)
		return fakeAPI{}, "", false, nil
	}
	v := newRelayBoxesView(t, dial, func(base string) RelayAPI { return relayclient.New(base) })
	loaded, ok := v.refresh(nil)().(boxesLoadedMsg)
	if !ok {
		t.Fatal("refresh should yield boxesLoadedMsg")
	}
	vv, cmd := v.Update(loaded)
	v = vv.(boxesView)
	if cmd == nil {
		t.Fatal("local rows should schedule the relay fetch")
	}
	vv, _ = v.Update(cmd())
	v = vv.(boxesView)
	if !strings.Contains(v.View(), "cloud.example") || !strings.Contains(v.View(), "●") {
		t.Fatalf("connected relay agent should be listed as live:\n%s", v.View())
	}
	// A LAN probe result must not override the relay's connected field.
	v.reach["cloud.example"] = false
	if !strings.Contains(v.View(), "●") {
		t.Fatalf("relay liveness should come from connected, not a LAN probe:\n%s", v.View())
	}
	for _, name := range dialed {
		if name == "cloud.example" {
			t.Fatal("relay-only row must not be probed through the LAN dialer")
		}
	}

	vv, _ = v.Update(keyRunes('j'))
	v = vv.(boxesView)
	_, cmd = v.Update(keyEnter())
	if cmd == nil {
		t.Fatal("relay-only row should be switchable")
	}
	msg := cmd()
	sw, ok := msg.(switchBoxMsg)
	if !ok || sw.box.Name != "cloud.example" || sw.box.RelayAPI != relay.URL || sw.box.AccountCredential != "cred-xyz" {
		t.Fatalf("relay row switch = %#v", msg)
	}
}

func TestBoxesViewDeduplicatesRowsFromConfigAndRelay(t *testing.T) {
	relay := agentsServer(t, []relayclient.Agent{{BaseDomain: "cloud.example", Connected: true}})
	defer relay.Close()
	seedConfig(t, config.ClientFile{
		Boxes: []config.Box{
			{Name: "account", Addr: "192.168.1.5:8088", RelayAPI: relay.URL, AccountCredential: "cred-xyz"},
			boxWithBaseDomain(t, config.Box{Name: "cloud.example", Addr: "192.168.1.6:8088"}, "cloud.example"),
		},
		Current: "account",
	})
	v := newRelayBoxesView(t, fakeDialer(fakeAPI{}, "", false, nil), func(base string) RelayAPI { return relayclient.New(base) })
	loaded, ok := v.refresh(nil)().(boxesLoadedMsg)
	if !ok {
		t.Fatal("refresh should yield boxesLoadedMsg")
	}
	vv, cmd := v.Update(loaded)
	v = vv.(boxesView)
	if cmd == nil {
		t.Fatal("local rows should schedule the relay fetch")
	}
	var relayMsg relayAgentsLoadedMsg
	gotRelay := false
	var probe boxProbeMsg
	switch result := cmd().(type) {
	case tea.BatchMsg:
		for _, subcmd := range result {
			switch msg := subcmd().(type) {
			case relayAgentsLoadedMsg:
				relayMsg = msg
				gotRelay = true
			case boxProbeMsg:
				probe = msg
			}
		}
	case relayAgentsLoadedMsg:
		relayMsg = result
		gotRelay = true
	case boxProbeMsg:
		probe = result
	}
	if gotRelay {
		vv, _ = v.Update(relayMsg)
		v = vv.(boxesView)
	}
	if probe.name != "" {
		vv, _ = v.Update(probe)
		v = vv.(boxesView)
	}
	if got := strings.Count(v.View(), "cloud.example"); got != 1 {
		t.Fatalf("box present in config and /agents should render once, got %d rows:\n%s", got, v.View())
	}
	if !strings.Contains(v.View(), "●") {
		t.Fatalf("deduplicated relay row should use relay liveness:\n%s", v.View())
	}
	for _, box := range v.boxes {
		if box.Name == "cloud.example" {
			if box.RelayAPI != relay.URL || box.AccountCredential != "cred-xyz" {
				t.Fatalf("deduplicated row lost relay path: %+v", box)
			}
			return
		}
	}
	t.Fatal("deduplicated relay row missing")
}

func TestBoxesViewDeduplicatedLANRowUsesLANLiveness(t *testing.T) {
	relay := agentsServer(t, []relayclient.Agent{{BaseDomain: "cloud.example", Connected: false}})
	defer relay.Close()
	seedConfig(t, config.ClientFile{
		Boxes: []config.Box{
			{Name: "account", Addr: "192.168.1.5:8088", RelayAPI: relay.URL, AccountCredential: "cred-xyz"},
			boxWithBaseDomain(t, config.Box{Name: "cloud.example", Addr: "192.168.1.6:8088"}, "cloud.example"),
		},
		Current: "account",
	})

	var dialed []string
	dial := func(box config.Box) (API, string, bool, error) {
		dialed = append(dialed, box.Name)
		return fakeAPI{}, box.Addr, false, nil
	}
	v := newRelayBoxesView(t, dial, func(base string) RelayAPI { return relayclient.New(base) })
	loaded, ok := v.refresh(nil)().(boxesLoadedMsg)
	if !ok {
		t.Fatal("refresh should yield boxesLoadedMsg")
	}
	vv, cmd := v.Update(loaded)
	v = vv.(boxesView)
	if cmd == nil {
		t.Fatal("deduplicated LAN row should emit a LAN probe")
	}
	result := cmd()
	var relayMsg relayAgentsLoadedMsg
	gotRelay := false
	var probe boxProbeMsg
	switch result := result.(type) {
	case tea.BatchMsg:
		for _, subcmd := range result {
			switch msg := subcmd().(type) {
			case relayAgentsLoadedMsg:
				relayMsg = msg
				gotRelay = true
			case boxProbeMsg:
				probe = msg
			}
		}
	case boxProbeMsg:
		probe = result
	case relayAgentsLoadedMsg:
		relayMsg = result
		gotRelay = true
	}
	if gotRelay {
		vv, _ = v.Update(relayMsg)
		v = vv.(boxesView)
	}
	if probe.name != "cloud.example" || !probe.reachable {
		t.Fatalf("want reachable cloud.example LAN probe, got %#v", probe)
	}
	if len(dialed) != 1 || dialed[0] != "cloud.example" {
		t.Fatalf("LAN probe dialed %v, want [cloud.example]", dialed)
	}
	vv, _ = v.Update(probe)
	v = vv.(boxesView)
	if !strings.Contains(v.View(), "cloud.example") || !strings.Contains(v.View(), "●") {
		t.Fatalf("deduplicated LAN row should use LAN liveness when relay is disconnected:\n%s", v.View())
	}
}

func TestBoxesViewDeduplicatesByPersistedAgentIdentity(t *testing.T) {
	base := "cloud.example"
	local := boxWithBaseDomain(t, config.Box{Name: "living-room", Addr: "192.168.1.6:8088"}, base)
	v := newBoxesView(fakeDialer(fakeAPI{}, "", false, nil))
	vv, _ := v.Update(boxesLoadedMsg{
		boxes:      []config.Box{local},
		current:    "living-room",
		relayAPI:   "https://relay.example",
		credential: "cred-xyz",
		viewID:     v.viewID, requestID: 1,
	})
	v = vv.(boxesView)
	v.relayRequestID = 1
	vv, _ = v.Update(relayAgentsLoadedMsg{
		agents: []relayclient.Agent{{BaseDomain: base, Connected: true}}, viewID: v.viewID, requestID: v.relayRequestID,
	})
	v = vv.(boxesView)
	if len(v.boxes) != 1 || v.boxes[0].Name != "living-room" {
		t.Fatalf("persisted identity should merge the LAN row, got %+v", v.boxes)
	}
	if got := persistedBaseDomain(t, v.boxes[0]); got != base {
		t.Fatalf("merged row lost agent identity: %+v", v.boxes[0])
	}
}

func TestBoxesViewKeepsMergedRowsAcrossLocalRefresh(t *testing.T) {
	base := "cloud.example"
	local := []config.Box{{Name: "local", Addr: "192.168.1.6:8088"}}
	v := newBoxesView(fakeDialer(fakeAPI{}, "", false, nil))
	vv, _ := v.Update(boxesLoadedMsg{
		boxes:      local,
		current:    "local",
		relayAPI:   "https://relay.example",
		credential: "cred-xyz",
		viewID:     v.viewID, requestID: 1,
	})
	v = vv.(boxesView)
	v.relayRequestID = 1
	vv, _ = v.Update(relayAgentsLoadedMsg{
		agents: []relayclient.Agent{{BaseDomain: base, Connected: true}}, viewID: v.viewID, requestID: v.relayRequestID,
	})
	v = vv.(boxesView)
	for i, box := range v.boxes {
		if persistedBaseDomain(t, box) == base {
			v.cursor = i
		}
	}
	vv, _ = v.Update(boxesLoadedMsg{
		boxes:      local,
		current:    "local",
		relayAPI:   "https://relay.example",
		credential: "cred-xyz",
		viewID:     v.viewID, requestID: 2,
	})
	v = vv.(boxesView)
	if len(v.boxes) != 2 || persistedBaseDomain(t, v.boxes[v.cursor]) != base {
		t.Fatalf("local refresh must preserve the selected merged row: cursor=%d boxes=%+v", v.cursor, v.boxes)
	}
}

func TestBoxesViewKeepsRelaySelectionWhenDisplayNamesCollide(t *testing.T) {
	local := boxWithBaseDomain(t, config.Box{Name: "same", Addr: "192.168.1.6:8088"}, "lan.example")
	v := newBoxesView(fakeDialer(fakeAPI{}, "", false, nil))
	vv, _ := v.Update(boxesLoadedMsg{
		boxes: []config.Box{local}, current: "same",
		relayAPI: "https://relay.example", credential: "cred-xyz",
		viewID: v.viewID, requestID: 1,
	})
	v = vv.(boxesView)
	v.relayRequestID = 1
	vv, _ = v.Update(relayAgentsLoadedMsg{
		agents: []relayclient.Agent{{Name: "same", BaseDomain: "remote.example", Connected: true}}, viewID: v.viewID, requestID: v.relayRequestID,
	})
	v = vv.(boxesView)
	if len(v.boxes) != 2 {
		t.Fatalf("want local and live rows, got %+v", v.boxes)
	}
	v.cursor = 1 // select the live row, which deliberately shares the display name

	vv, _ = v.Update(boxesLoadedMsg{
		boxes: []config.Box{local}, current: "same",
		relayAPI: "https://relay.example", credential: "cred-xyz",
		viewID: v.viewID, requestID: 2,
	})
	v = vv.(boxesView)
	if v.cursor != 1 || persistedBaseDomain(t, v.boxes[v.cursor]) != "remote.example" {
		t.Fatalf("refresh moved selection off the live identity: cursor=%d boxes=%+v", v.cursor, v.boxes)
	}
}

func TestBoxesSelectionSurvivesLiveToConfigProvenanceTransition(t *testing.T) {
	lan := boxWithBaseDomain(t, config.Box{Name: "same", Addr: "192.168.1.6:8088"}, "lan.example")
	v := newBoxesView(fakeDialer(fakeAPI{}, "", false, nil))
	vv, _ := v.Update(boxesLoadedMsg{
		boxes: []config.Box{lan}, current: "same",
		relayAPI: "https://relay.example", credential: "cred-xyz",
		viewID: v.viewID, requestID: 1,
	})
	v = vv.(boxesView)
	v.relayRequestID = 1
	vv, _ = v.Update(relayAgentsLoadedMsg{
		agents: []relayclient.Agent{{Name: "same", BaseDomain: "remote.example", Connected: true}}, viewID: v.viewID, requestID: v.relayRequestID,
	})
	v = vv.(boxesView)
	if len(v.boxes) != 2 {
		t.Fatalf("want LAN and live rows, got %+v", v.boxes)
	}
	v.cursor = 1 // select live remote.example while it is relay-only

	if v.boxes[0].Name != v.boxes[1].Name {
		t.Fatalf("fixture must retain duplicate display names: boxes=%+v", v.boxes)
	}
	remote := boxWithBaseDomain(t, config.Box{Name: "remote"}, "remote.example")
	vv, _ = v.Update(boxesLoadedMsg{
		boxes: []config.Box{remote, lan}, current: "same",
		relayAPI: "https://relay.example", credential: "cred-xyz",
		viewID: v.viewID, requestID: 2,
	})
	v = vv.(boxesView)
	if v.cursor != 0 || persistedBaseDomain(t, v.boxes[v.cursor]) != "remote.example" {
		t.Fatalf("selection lost the BaseDomain across provenance transition: cursor=%d boxes=%+v", v.cursor, v.boxes)
	}
}

func TestRelayRowsDoNotMergeIntoUnidentifiedLocalBoxes(t *testing.T) {
	const base = "ab12-erin.public.getpiper.co"
	seedConfig(t, config.ClientFile{
		Boxes:   []config.Box{{Name: "default", Addr: "192.168.1.6:8088", RelayAPI: "https://relay.example", AccountCredential: "cred-xyz"}},
		Current: "default",
	})
	v := newRelayBoxesView(t, fakeDialer(fakeAPI{}, "", false, nil), relayFor(fakeRelay{
		agents: []relayclient.Agent{{Name: "default", BaseDomain: base, Connected: true}},
	}))
	loaded := v.refresh(nil)().(boxesLoadedMsg)
	vv, cmd := v.Update(loaded)
	v = vv.(boxesView)
	if cmd == nil {
		t.Fatal("saved relay config should fetch its account agents")
	}
	vv, _ = v.Update(cmd())
	v = vv.(boxesView)
	if len(v.boxes) != 2 {
		t.Fatalf("unidentified local box must remain separate from the live relay row: %+v", v.boxes)
	}
	cf, err := config.LoadClientFile()
	if err != nil {
		t.Fatal(err)
	}
	if len(cf.Boxes) != 1 || cf.Boxes[0].BaseDomain != "" {
		t.Fatalf("unidentified local row was unexpectedly rewritten: %+v", cf)
	}
}

func TestBoxesViewRejectsStaleLocalRefresh(t *testing.T) {
	v := newBoxesView(fakeDialer(fakeAPI{}, "", false, nil))
	vv, _ := v.Update(boxesLoadedMsg{
		boxes: []config.Box{{Name: "new", Addr: "new-addr"}}, current: "new",
		viewID: v.viewID, requestID: 2,
	})
	v = vv.(boxesView)
	vv, _ = v.Update(boxesLoadedMsg{
		boxes: []config.Box{{Name: "old", Addr: "old-addr"}}, current: "old",
		viewID: v.viewID, requestID: 1,
	})
	v = vv.(boxesView)
	if len(v.configBoxes) != 1 || v.configBoxes[0].Name != "new" {
		t.Fatalf("stale local refresh replaced newer config: %+v", v.configBoxes)
	}
}

func TestBoxesRefreshKeepsRelayErrorAndCapsRetries(t *testing.T) {
	v := newBoxesView(fakeDialer(fakeAPI{}, "", false, nil))
	v.relay = relayFor(fakeRelay{})
	vv, cmd := v.Update(boxesLoadedMsg{
		boxes: []config.Box{{Name: "local", Addr: "192.168.1.6:8088"}}, current: "local",
		relayAPI: "https://relay.example", credential: "cred-xyz", viewID: v.viewID, requestID: 1,
	})
	v = vv.(boxesView)
	if cmd == nil {
		t.Fatal("initial relay load should start a fetch")
	}
	vv, _ = v.Update(relayAgentsLoadedMsg{err: errors.New("temporary relay outage"), viewID: v.viewID, requestID: v.relayRequestID})
	v = vv.(boxesView)
	if v.relayFetchStarted || v.err == nil {
		t.Fatalf("fixture did not enter the transient error state: started=%v err=%v", v.relayFetchStarted, v.err)
	}
	vv, retry := v.Update(boxesLoadedMsg{
		boxes: []config.Box{{Name: "local", Addr: "192.168.1.6:8088"}}, current: "local",
		relayAPI: "https://relay.example", credential: "cred-xyz", viewID: v.viewID, requestID: 2,
	})
	v = vv.(boxesView)
	if v.err == nil || retry == nil {
		t.Fatalf("local reload must keep the relay error visible while reopening a bounded fetch: err=%v retry=%v", v.err, retry != nil)
	}
	vv, _ = v.Update(relayAgentsLoadedMsg{err: errors.New("temporary relay outage"), viewID: v.viewID, requestID: v.relayRequestID})
	v = vv.(boxesView)
	vv, retry = v.Update(boxesLoadedMsg{
		boxes: []config.Box{{Name: "local", Addr: "192.168.1.6:8088"}}, current: "local",
		relayAPI: "https://relay.example", credential: "cred-xyz", viewID: v.viewID, requestID: 3,
	})
	v = vv.(boxesView)
	if v.err == nil || retry == nil {
		t.Fatalf("second retry must remain visible and bounded: err=%v retry=%v", v.err, retry != nil)
	}
	vv, _ = v.Update(relayAgentsLoadedMsg{err: errors.New("temporary relay outage"), viewID: v.viewID, requestID: v.relayRequestID})
	v = vv.(boxesView)
	vv, retry = v.Update(boxesLoadedMsg{
		boxes: []config.Box{{Name: "local", Addr: "192.168.1.6:8088"}}, current: "local",
		relayAPI: "https://relay.example", credential: "cred-xyz", viewID: v.viewID, requestID: 4,
	})
	v = vv.(boxesView)
	if v.err == nil || retry != nil {
		t.Fatalf("relay retries must stop after the cap while preserving the error: err=%v retry=%v", v.err, retry != nil)
	}
	// A credential change starts a new fetch budget; a successful response is
	// the only path that clears the sticky relay error.
	vv, retry = v.Update(boxesLoadedMsg{
		boxes: []config.Box{{Name: "local", Addr: "192.168.1.6:8088"}}, current: "local",
		relayAPI: "https://relay.example", credential: "cred-2", viewID: v.viewID, requestID: 5,
	})
	v = vv.(boxesView)
	if retry == nil {
		t.Fatal("credential change should start a fresh relay fetch")
	}
	vv, _ = v.Update(relayAgentsLoadedMsg{agents: []relayclient.Agent{}, viewID: v.viewID, requestID: v.relayRequestID})
	v = vv.(boxesView)
	if v.err != nil {
		t.Fatalf("successful relay fetch must clear the sticky error: %v", v.err)
	}
}

func TestLiveRelayNameCannotGateLocalRowWithoutIdentity(t *testing.T) {
	v := boxesView{relayRows: map[string]bool{"same": true}}
	if v.liveRelayRow(config.Box{Name: "same", Addr: "192.168.1.6:8088"}) {
		t.Fatal("relay display name incorrectly marked an unidentifiable local row live")
	}
}

func TestEditingMergedLANRowDoesNotCarryDisplayOnlyRelayCredentials(t *testing.T) {
	local := config.Box{Name: "local", Addr: "192.168.1.6:8088", BaseDomain: "local.example"}
	v := newBoxesView(fakeDialer(fakeAPI{}, "", false, nil))
	v.relay = relayFor(fakeRelay{})
	vv, _ := v.Update(boxesLoadedMsg{
		boxes: []config.Box{local}, current: "local", relayAPI: "https://relay.example", credential: "cred-xyz",
		viewID: v.viewID, requestID: 1,
	})
	v = vv.(boxesView)
	vv, _ = v.Update(relayAgentsLoadedMsg{
		agents: []relayclient.Agent{{BaseDomain: "local.example", Name: "local", Connected: true}}, viewID: v.viewID,
		requestID: v.relayRequestID,
	})
	v = vv.(boxesView)
	if len(v.boxes) != 1 || v.boxes[0].RelayAPI == "" || v.boxes[0].AccountCredential == "" {
		t.Fatalf("fixture did not create a merged relay display row: %+v", v.boxes)
	}
	vv, cmd := v.Update(keyRunes('e'))
	if cmd == nil {
		t.Fatal("editing a local merged row should open the form")
	}
	push, ok := cmd().(pushMsg)
	if !ok {
		t.Fatalf("edit should emit pushMsg, got %T", cmd())
	}
	form, ok := push.view.(boxFormView)
	if !ok {
		t.Fatalf("edit should push boxFormView, got %T", push.view)
	}
	if form.orig.RelayAPI != "" || form.orig.AccountCredential != "" {
		t.Fatalf("form captured relay credentials that only came from the merged display row: %+v", form.orig)
	}
	_ = vv
}

func TestRelayOnlyRowsCannotBeEditedOrRemoved(t *testing.T) {
	v := newBoxesView(fakeDialer(fakeAPI{}, "", false, nil))
	vv, _ := v.Update(boxesLoadedMsg{
		boxes:      []config.Box{{Name: "local", Addr: "192.168.1.6:8088"}},
		current:    "local",
		relayAPI:   "https://relay.example",
		credential: "cred-xyz",
		viewID:     v.viewID, requestID: 1,
	})
	v = vv.(boxesView)
	v.relayRequestID = 1
	vv, _ = v.Update(relayAgentsLoadedMsg{
		agents: []relayclient.Agent{{BaseDomain: "cloud.example", Connected: true}}, viewID: v.viewID, requestID: v.relayRequestID,
	})
	v = vv.(boxesView)
	v.cursor = len(v.boxes) - 1
	if _, cmd := v.Update(keyRunes('x')); cmd != nil {
		t.Fatal("x on a relay-only live row must not open a local remove action")
	}
	if _, cmd := v.Update(keyRunes('e')); cmd != nil {
		t.Fatal("e on a relay-only live row must not open a local edit action")
	}
}

func TestBoxesRefreshUsesRelayCredentialsFromAnySavedBox(t *testing.T) {
	relay := agentsServer(t, []relayclient.Agent{{BaseDomain: "cloud.example", Connected: true}})
	defer relay.Close()
	seedConfig(t, config.ClientFile{
		Boxes: []config.Box{
			{Name: "local", Addr: "192.168.1.6:8088"},
			{Name: "account", RelayAPI: relay.URL, AccountCredential: "cred-xyz"},
		},
		Current: "local",
	})
	v := newRelayBoxesView(t, fakeDialer(fakeAPI{}, "", false, nil), func(base string) RelayAPI { return relayclient.New(base) })
	loaded := v.refresh(nil)().(boxesLoadedMsg)
	vv, cmd := v.Update(loaded)
	v = vv.(boxesView)
	for _, msg := range commandMessages(cmd) {
		vv, _ = v.Update(msg)
		v = vv.(boxesView)
	}
	for _, box := range v.boxes {
		if persistedBaseDomain(t, box) == "cloud.example" {
			return
		}
	}
	t.Fatalf("relay rows should load from a sibling box's credentials: %+v", v.boxes)
}

func TestBoxesRefreshDoesNotRefetchRelayOnEveryPoll(t *testing.T) {
	relay := agentsServer(t, []relayclient.Agent{{BaseDomain: "cloud.example", Connected: true}})
	defer relay.Close()
	seedConfig(t, config.ClientFile{
		Boxes:   []config.Box{{Name: "local", Addr: "192.168.1.6:8088", RelayAPI: relay.URL, AccountCredential: "cred-xyz"}},
		Current: "local",
	})
	v := newRelayBoxesView(t, fakeDialer(fakeAPI{}, "", false, nil), func(base string) RelayAPI { return relayclient.New(base) })
	loaded := v.refresh(nil)().(boxesLoadedMsg)
	vv, cmd := v.Update(loaded)
	v = vv.(boxesView)
	var relayMsg tea.Msg
	for _, msg := range commandMessages(cmd) {
		if _, ok := msg.(relayAgentsLoadedMsg); ok {
			relayMsg = msg
		}
		vv, _ = v.Update(msg)
		v = vv.(boxesView)
	}
	if relayMsg == nil {
		t.Fatal("initial boxes load should fetch relay agents")
	}
	loaded = v.refresh(nil)().(boxesLoadedMsg)
	vv, cmd = v.Update(loaded)
	_ = vv
	for _, msg := range commandMessages(cmd) {
		if _, ok := msg.(relayAgentsLoadedMsg); ok {
			t.Fatal("a later poll must not refetch relay agents")
		}
	}
}

type deadlineAgentsRelay struct {
	fakeRelay
	deadline chan<- bool
}

func (r deadlineAgentsRelay) Agents(ctx context.Context, cred string) ([]relayclient.Agent, error) {
	_, hasDeadline := ctx.Deadline()
	r.deadline <- hasDeadline
	return nil, nil
}

func TestBoxesRelayFetchUsesBoundedContext(t *testing.T) {
	seedConfig(t, config.ClientFile{
		Boxes:   []config.Box{{Name: "local", RelayAPI: "https://relay.example", AccountCredential: "cred-xyz"}},
		Current: "local",
	})
	deadline := make(chan bool, 1)
	relay := deadlineAgentsRelay{deadline: deadline}
	v := newRelayBoxesView(t, fakeDialer(fakeAPI{}, "", false, nil), func(string) RelayAPI { return relay })
	loaded := v.refresh(nil)().(boxesLoadedMsg)
	_, cmd := v.Update(loaded)
	_ = commandMessages(cmd)
	if !<-deadline {
		t.Fatal("relay agent fetch must carry a timeout context")
	}
}

type exactDeadlineAgentsRelay struct {
	deadline chan<- time.Time
}

func (r exactDeadlineAgentsRelay) CLILoginStart(context.Context) (string, string, error) {
	return "", "", nil
}
func (r exactDeadlineAgentsRelay) CLILoginPoll(context.Context, string) (relayclient.Account, error) {
	return relayclient.Account{}, nil
}
func (r exactDeadlineAgentsRelay) GitHubStatus(context.Context, string) (relayclient.Status, error) {
	return relayclient.Status{}, nil
}
func (r exactDeadlineAgentsRelay) GitHubRepos(context.Context, string, string) ([]relayclient.Repo, error) {
	return nil, nil
}
func (r exactDeadlineAgentsRelay) Agents(ctx context.Context, _ string) ([]relayclient.Agent, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return nil, errors.New("relay request has no deadline")
	}
	r.deadline <- deadline
	return nil, nil
}

func TestBoxesRelayFetchUsesConfiguredTimeout(t *testing.T) {
	seedConfig(t, config.ClientFile{
		Boxes:   []config.Box{{Name: "local", RelayAPI: "https://relay.example", AccountCredential: "cred-xyz"}},
		Current: "local",
	})
	deadline := make(chan time.Time, 1)
	relay := exactDeadlineAgentsRelay{deadline: deadline}
	v := newRelayBoxesView(t, fakeDialer(fakeAPI{}, "", false, nil), func(string) RelayAPI { return relay })
	loaded := v.refresh(nil)().(boxesLoadedMsg)
	_, cmd := v.Update(loaded)
	started := time.Now()
	_ = commandMessages(cmd)
	got := <-deadline
	finished := time.Now()
	const (
		wantTimeout = 5 * time.Second
		tolerance   = 500 * time.Millisecond
	)
	if got.Before(started.Add(wantTimeout-tolerance)) || got.After(finished.Add(wantTimeout+tolerance)) {
		t.Fatalf("relay deadline = %s, want approximately %s from request start", got, wantTimeout)
	}
}

type blockingAgentsRelay struct {
	fakeRelay
	started chan<- struct{}
	release <-chan struct{}
}

func (r blockingAgentsRelay) Agents(context.Context, string) ([]relayclient.Agent, error) {
	close(r.started)
	<-r.release
	return r.fakeRelay.agents, r.fakeRelay.agentsErr
}

func TestBoxesRefreshRendersLocalRowsBeforeRelayReturns(t *testing.T) {
	seedConfig(t, config.ClientFile{
		Boxes:   []config.Box{{Name: "local", Addr: "192.168.1.6:8088", RelayAPI: "https://relay.example", AccountCredential: "cred-xyz"}},
		Current: "local",
	})
	started := make(chan struct{})
	release := make(chan struct{})
	relay := blockingAgentsRelay{
		fakeRelay: fakeRelay{agents: []relayclient.Agent{{BaseDomain: "cloud.example", Connected: true}}},
		started:   started,
		release:   release,
	}
	v := newRelayBoxesView(t, fakeDialer(fakeAPI{}, "", false, nil), func(string) RelayAPI { return relay })

	result := make(chan tea.Msg, 1)
	go func() { result <- v.refresh(nil)() }()
	select {
	case msg := <-result:
		loaded, ok := msg.(boxesLoadedMsg)
		if !ok {
			t.Fatalf("refresh should yield local boxesLoadedMsg, got %T", msg)
		}
		vv, relayCmd := v.Update(loaded)
		v = vv.(boxesView)
		if relayCmd == nil {
			t.Fatal("loaded local rows should schedule the relay fetch")
		}
		relayResult := make(chan tea.Msg, 1)
		go func() { relayResult <- relayCmd() }()
		<-started
		if !strings.Contains(v.View(), "local") || strings.Contains(v.View(), "loading") {
			t.Fatalf("local rows should render before relay returns:\n%s", v.View())
		}
		close(release)
		vv, _ = v.Update(<-relayResult)
		v = vv.(boxesView)
		if !strings.Contains(v.View(), "cloud.example") {
			t.Fatalf("relay row should arrive after the controlled release:\n%s", v.View())
		}
	case <-started:
		close(release)
		msg := <-result
		t.Fatalf("refresh waited for relay before returning local rows; got %T after release", msg)
	}
}

func TestBoxesRefreshRejectsLateRelayResultAfterCredentialRefresh(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	relay := blockingAgentsRelay{
		fakeRelay: fakeRelay{agents: []relayclient.Agent{{Name: "old", BaseDomain: "old-agent.example", Connected: true}}},
		started:   started,
		release:   release,
	}
	v := newBoxesView(fakeDialer(fakeAPI{}, "", false, nil))
	v.relay = func(string) RelayAPI { return relay }
	vv, cmd := v.Update(boxesLoadedMsg{
		boxes:   []config.Box{{Name: "local", Addr: "192.168.1.6:8088", RelayAPI: "https://old.example", AccountCredential: "old-cred"}},
		current: "local", relayAPI: "https://old.example", credential: "old-cred",
		viewID: v.viewID, requestID: 1,
	})
	v = vv.(boxesView)
	if cmd == nil {
		t.Fatal("initial config should start a relay fetch")
	}
	result := make(chan tea.Msg, 1)
	go func() { result <- cmd() }()
	<-started

	vv, _ = v.Update(boxesLoadedMsg{
		boxes: []config.Box{{Name: "local", Addr: "192.168.1.6:8088"}}, current: "local",
		viewID: v.viewID, requestID: 2,
	})
	v = vv.(boxesView)
	close(release)
	vv, _ = v.Update(<-result)
	v = vv.(boxesView)
	if v.relayLoaded || len(v.boxes) != 1 || persistedBaseDomain(t, v.boxes[0]) != "" {
		t.Fatalf("stale relay response resurrected old-account state: relayLoaded=%v boxes=%+v", v.relayLoaded, v.boxes)
	}
}

func TestBoxesViewRejectsRelayResultFromPreviousView(t *testing.T) {
	relay := fakeRelay{agents: []relayclient.Agent{{Name: "old", BaseDomain: "old-agent.example", Connected: true}}}
	old := newBoxesView(fakeDialer(fakeAPI{}, "", false, nil))
	old.relay = relayFor(relay)
	oldModel, cmd := old.Update(boxesLoadedMsg{
		boxes:   []config.Box{{Name: "local", Addr: "192.168.1.6:8088", RelayAPI: "https://relay.example", AccountCredential: "cred-xyz"}},
		current: "local", relayAPI: "https://relay.example", credential: "cred-xyz",
		viewID: old.viewID, requestID: 1,
	})
	old = oldModel.(boxesView)
	if cmd == nil {
		t.Fatal("old view should start a relay fetch")
	}
	stale := cmd()

	fresh := newBoxesView(fakeDialer(fakeAPI{}, "", false, nil))
	freshModel, _ := fresh.Update(boxesLoadedMsg{
		boxes: []config.Box{{Name: "local", Addr: "192.168.1.6:8088"}}, current: "local",
		viewID: fresh.viewID, requestID: 1,
	})
	fresh = freshModel.(boxesView)
	freshModel, _ = fresh.Update(stale)
	fresh = freshModel.(boxesView)
	if fresh.relayLoaded || len(fresh.boxes) != 1 {
		t.Fatalf("relay response from a previous view was accepted: relayLoaded=%v boxes=%+v", fresh.relayLoaded, fresh.boxes)
	}
}

func TestRelayFetchRearmsWhenBoxesViewRegainsTop(t *testing.T) {
	seedConfig(t, config.ClientFile{
		Boxes:   []config.Box{{Name: "local", Addr: "192.168.1.6:8088", RelayAPI: "https://relay.example", AccountCredential: "cred-xyz"}},
		Current: "local",
	})
	relay := relayFor(fakeRelay{agents: []relayclient.Agent{{BaseDomain: "cloud.example", Connected: true}}})
	m := NewModel("local", "", false, fakeAPI{}).WithDialer(fakeDialer(fakeAPI{}, "", false, nil)).WithRelay(relay)
	boxes := newBoxesView(m.dial)
	boxes.relay = relay
	next, _ := m.Update(pushMsg{view: boxes})
	m = next.(Model)

	boxes = m.top().(boxesView)
	next, cmd := m.Update(boxesLoadedMsg{
		boxes:   []config.Box{{Name: "local", Addr: "192.168.1.6:8088", RelayAPI: "https://relay.example", AccountCredential: "cred-xyz"}},
		current: "local", relayAPI: "https://relay.example", credential: "cred-xyz",
		viewID: boxes.viewID, requestID: 1,
	})
	m = next.(Model)
	if cmd == nil {
		t.Fatal("initial boxes load should start a relay fetch")
	}
	var stale tea.Msg
	for _, msg := range commandMessages(cmd) {
		if _, ok := msg.(relayAgentsLoadedMsg); ok {
			stale = msg
			break
		}
	}
	if stale == nil {
		t.Fatal("initial boxes load did not produce a relay result")
	}

	next, _ = m.Update(pushMsg{view: newBoxForm(m.dial, nil)})
	m = next.(Model)
	// The result lands while the form is on top and is intentionally dropped by
	// the root's top-view routing.
	next, _ = m.Update(stale)
	m = next.(Model)

	next, refresh := m.Update(keyEsc())
	m = next.(Model)
	if refresh == nil {
		t.Fatal("returning to boxes should refresh the view")
	}
	loaded := refresh()
	next, retry := m.Update(loaded)
	m = next.(Model)
	if retry == nil {
		t.Fatal("boxes view must re-arm a relay fetch after regaining the top")
	}
	for _, msg := range commandMessages(retry) {
		next, _ = m.Update(msg)
		m = next.(Model)
	}
	if !m.top().(boxesView).relayLoaded {
		t.Fatalf("relay result after returning to boxes was lost: %s", m.View())
	}
}

func TestEditingMergedRowWithDuplicateBaseDomainUsesSelectedName(t *testing.T) {
	first := config.Box{Name: "first", Addr: "first:8088", Token: "first-token", BaseDomain: "shared.example"}
	second := config.Box{Name: "second", Addr: "second:8088", Token: "second-token", BaseDomain: "shared.example"}
	v := newBoxesView(fakeDialer(fakeAPI{}, "", false, nil))
	vv, _ := v.Update(boxesLoadedMsg{
		boxes: []config.Box{first, second}, current: "first",
		relayAPI: "https://relay.example", credential: "cred-xyz",
		viewID: v.viewID, requestID: 1,
	})
	v = vv.(boxesView)
	vv, _ = v.Update(relayAgentsLoadedMsg{
		agents: []relayclient.Agent{{Name: "second", BaseDomain: "shared.example", Connected: true}},
		viewID: v.viewID, requestID: v.relayRequestID,
	})
	v = vv.(boxesView)
	if len(v.boxes) != 2 {
		t.Fatalf("duplicate identity fixture should retain both local rows: %+v", v.boxes)
	}
	for i, box := range v.boxes {
		if box.Name == "second" {
			v.cursor = i
		}
	}
	vv, cmd := v.Update(keyRunes('e'))
	if cmd == nil {
		t.Fatal("selected local row should open the edit form")
	}
	push, ok := cmd().(pushMsg)
	if !ok {
		t.Fatalf("edit should emit pushMsg, got %T", cmd())
	}
	form, ok := push.view.(boxFormView)
	if !ok {
		t.Fatalf("edit should push boxFormView, got %T", push.view)
	}
	if form.orig.Name != "second" || form.orig.Token != "second-token" {
		t.Fatalf("edit selected the wrong duplicate-identity row: orig=%+v", form.orig)
	}
	_ = vv
}

func TestBoxesRefreshProbesLANBoxWithRelayCreds(t *testing.T) {
	// Without a live /agents entry, a LAN box carrying stale relay creds still
	// gets a local reachability probe like any other LAN box.
	v := newBoxesView(fakeDialer(fakeAPI{}, "", false, nil))
	_, cmd := v.Update(boxesLoadedMsg{
		boxes: []config.Box{
			{Name: "pi4"},
			{Name: "cloud", Addr: "192.168.1.6:8088", RelayAPI: "https://r.example"},
			{Name: "faraway", RelayAPI: "https://r.example"},
		},
		current: "pi4",
		viewID:  v.viewID, requestID: 1,
	})
	if cmd == nil {
		t.Fatal("loading boxes should emit reachability probes")
	}
	// tea.Batch collapses a single cmd, so exactly one probe (cloud) yields a
	// bare boxProbeMsg; faraway (relay-only) and pi4 (current) are skipped.
	probe, ok := cmd().(boxProbeMsg)
	if !ok || probe.name != "cloud" {
		t.Fatalf("want a single probe for cloud, got %#v", cmd())
	}
}

func TestRootSwitchSwapsBoxAndResetsStack(t *testing.T) {
	m := NewModel("pi4", "192.168.1.6:8088", false, fakeAPI{}).
		WithDialer(fakeDialer(fakeAPI{apps: nil}, "192.168.1.9:8088", false, nil))
	// go deep so we can prove the stack resets
	m2, _ := m.Update(pushMsg{newBoxesView(m.dial)})
	m = m2.(Model)
	m2, _ = m.Update(switchBoxMsg{box: config.Box{Name: "blog", Addr: "192.168.1.9:8088"}})
	m = m2.(Model)
	if len(m.stack) != 1 {
		t.Fatalf("switch should reset to a single apps view, got depth %d", len(m.stack))
	}
	m = pump(t, m, m.refresh())
	out := m.View()
	if !strings.Contains(out, "blog") || !strings.Contains(out, "192.168.1.9:8088") {
		t.Fatalf("status bar did not switch to blog:\n%s", out)
	}
}

func TestRootSwitchFailureBannersAndKeepsBox(t *testing.T) {
	m := NewModel("pi4", "192.168.1.6:8088", false, fakeAPI{}).
		WithDialer(fakeDialer(nil, "", false, errors.New("dial refused")))
	m2, _ := m.Update(pushMsg{newBoxesView(m.dial)})
	m = m2.(Model)
	m2, _ = m.Update(switchBoxMsg{box: config.Box{Name: "blog", Addr: "x"}})
	m = m2.(Model)
	if m.box != "pi4" {
		t.Fatalf("failed switch must keep the old box, got %q", m.box)
	}
	if !strings.Contains(m.View(), "dial refused") {
		t.Fatalf("switch error should banner in the boxes view:\n%s", m.View())
	}
}

func TestBoxesRefreshEmitsProbePerRemoteBox(t *testing.T) {
	v := newBoxesView(fakeDialer(fakeAPI{}, "", false, nil))
	// current box (pi4) is not probed; blog and shop are.
	vv, cmd := v.Update(boxesLoadedMsg{
		boxes:   []config.Box{{Name: "pi4"}, {Name: "blog", Addr: "a"}, {Name: "shop", Addr: "b"}},
		current: "pi4",
		viewID:  v.viewID, requestID: 1,
	})
	_ = vv
	if cmd == nil {
		t.Fatal("loading boxes should emit reachability probes")
	}
	msg := cmd() // tea.Batch aggregates into a BatchMsg of cmds
	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		t.Fatalf("want tea.BatchMsg of probes, got %T", msg)
	}
	if len(batch) != 2 {
		t.Fatalf("want 2 probes (non-current, non-relay), got %d", len(batch))
	}
}

func TestBoxProbeMsgFlipsRowStatus(t *testing.T) {
	v := newBoxesView(fakeDialer(fakeAPI{}, "", false, nil))
	vv, _ := v.Update(boxesLoadedMsg{boxes: []config.Box{{Name: "pi4"}, {Name: "blog", Addr: "a"}}, current: "pi4", viewID: v.viewID, requestID: 1})
	v = vv.(boxesView)

	vv, _ = v.Update(boxProbeMsg{name: "blog", reachable: true})
	if out := vv.(boxesView).View(); !strings.Contains(out, "●") {
		t.Fatalf("reachable box should show ●:\n%s", out)
	}

	vv, _ = v.Update(boxProbeMsg{name: "blog", reachable: false})
	if out := vv.(boxesView).View(); !strings.Contains(out, "○") {
		t.Fatalf("unreachable box should show ○:\n%s", out)
	}
}

func TestBoxProbeReflectsDialerResult(t *testing.T) {
	// a dialer whose client ListApps errors => unreachable
	v := newBoxesView(fakeDialer(fakeAPI{err: errors.New("refused")}, "", false, nil))
	probe := v.probe(config.Box{Name: "blog", Addr: "a"})
	msg := probe().(boxProbeMsg)
	if msg.name != "blog" || msg.reachable {
		t.Fatalf("want blog unreachable, got %#v", msg)
	}
}

func TestXOpensRemoveConfirm(t *testing.T) {
	v := newBoxesView(fakeDialer(fakeAPI{}, "", false, nil))
	vv, _ := v.Update(boxesLoadedMsg{boxes: []config.Box{{Name: "pi4"}, {Name: "blog", Addr: "a"}}, current: "pi4", viewID: v.viewID, requestID: 1})
	v = vv.(boxesView)
	vv, _ = v.Update(keyRunes('j')) // move to blog
	v = vv.(boxesView)
	_, cmd := v.Update(keyRunes('x'))
	push, ok := cmd().(pushMsg)
	if !ok {
		t.Fatalf("x should push a confirm, got %T", cmd())
	}
	if _, ok := push.view.(confirmView); !ok {
		t.Fatalf("x should push confirmView, got %T", push.view)
	}
}

func TestRemoveBoxDropsIt(t *testing.T) {
	seedConfig(t, config.ClientFile{Boxes: []config.Box{{Name: "pi4"}, {Name: "blog"}}, Current: "pi4"})
	current, changed, err := removeBox("blog")
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatalf("removing a non-current box should not change current: %+v", current)
	}
	cf, _ := config.LoadClientFile()
	if len(cf.Boxes) != 1 || cf.Boxes[0].Name != "pi4" {
		t.Fatalf("blog not removed: %+v", cf.Boxes)
	}
}

func TestRemoveCurrentBoxPromotesFirst(t *testing.T) {
	seedConfig(t, config.ClientFile{Boxes: []config.Box{{Name: "pi4"}, {Name: "blog"}}, Current: "pi4"})
	current, changed, err := removeBox("pi4")
	if err != nil {
		t.Fatal(err)
	}
	if !changed || current.Name != "blog" {
		t.Fatalf("removing current should promote blog, got changed=%v current=%+v", changed, current)
	}
	cf, _ := config.LoadClientFile()
	if cf.Current != "blog" {
		t.Fatalf("current not promoted on disk: %q", cf.Current)
	}
}

func TestRemoveLastBoxRefused(t *testing.T) {
	seedConfig(t, config.ClientFile{Boxes: []config.Box{{Name: "pi4"}}, Current: "pi4"})
	if _, _, err := removeBox("pi4"); err == nil {
		t.Fatal("removing the last box must be refused")
	}
	cf, _ := config.LoadClientFile()
	if len(cf.Boxes) != 1 {
		t.Fatalf("refused remove must not write: %+v", cf.Boxes)
	}
}

func TestRootBoxRemovedNonCurrentPopsToBoxes(t *testing.T) {
	m := NewModel("pi4", "a", false, fakeAPI{}).WithDialer(fakeDialer(fakeAPI{}, "", false, nil))
	m2, _ := m.Update(pushMsg{newBoxesView(m.dial)})
	m = m2.(Model)
	m2, _ = m.Update(pushMsg{newRemoveBoxConfirm("blog")})
	m = m2.(Model)
	if len(m.stack) != 3 {
		t.Fatalf("setup: want depth 3, got %d", len(m.stack))
	}
	m2, _ = m.Update(boxRemovedMsg{changed: false})
	m = m2.(Model)
	if len(m.stack) != 2 {
		t.Fatalf("removed (non-current) should pop to boxes (depth 2), got %d", len(m.stack))
	}
}
