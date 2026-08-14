package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	t.Setenv("PIPER_API_ADDR", "")
	// Isolate the data dir so Load() can't read the developer's real
	// ~/.piper relay.json (an enrolled box's apex would shadow the default).
	t.Setenv("PIPER_DATA_DIR", t.TempDir())
	t.Setenv("PIPER_BASE_DOMAIN", "")
	c := Load()
	if c.APIAddr != "127.0.0.1:8088" {
		t.Errorf("APIAddr = %q, want 127.0.0.1:8088", c.APIAddr)
	}
	if c.BaseDomain != "piper.localhost" {
		t.Errorf("BaseDomain = %q, want piper.localhost", c.BaseDomain)
	}
	if c.CaddyAdmin != "http://127.0.0.1:2019" {
		t.Errorf("CaddyAdmin = %q", c.CaddyAdmin)
	}
}

func TestLoadEnvOverride(t *testing.T) {
	t.Setenv("PIPER_DATA_DIR", t.TempDir())
	t.Setenv("PIPER_API_ADDR", "0.0.0.0:9000")
	if got := Load().APIAddr; got != "0.0.0.0:9000" {
		t.Errorf("APIAddr = %q, want 0.0.0.0:9000", got)
	}
}

// Only "1" keeps the browser shut; every other value (including the common
// "0") leaves the launch on, so a stray export can't silently disable it.
func TestNoBrowser(t *testing.T) {
	for _, tc := range []struct {
		val  string
		want bool
	}{
		{"", false},
		{"0", false},
		{"true", false},
		{"1", true},
	} {
		t.Setenv("PIPER_NO_BROWSER", tc.val)
		if got := NoBrowser(); got != tc.want {
			t.Errorf("PIPER_NO_BROWSER=%q: NoBrowser() = %v, want %v", tc.val, got, tc.want)
		}
	}
}

func TestLoadRelayFields(t *testing.T) {
	t.Setenv("PIPER_DATA_DIR", t.TempDir())
	t.Setenv("PIPER_RELAY_ADDR", "relay.example.com:7000")
	t.Setenv("PIPER_RELAY_TOKEN", "tok-xyz")
	t.Setenv("PIPER_ACME_EMAIL", "me@example.com")
	cfg := Load()
	if cfg.RelayAddr != "relay.example.com:7000" {
		t.Errorf("RelayAddr = %q", cfg.RelayAddr)
	}
	if cfg.RelayToken != "tok-xyz" {
		t.Errorf("RelayToken = %q", cfg.RelayToken)
	}
	if cfg.ACMEEmail != "me@example.com" {
		t.Errorf("ACMEEmail = %q", cfg.ACMEEmail)
	}
}

func TestDefaultDataDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PIPER_DATA_DIR", "")
	got := Load().DataDir
	want := filepath.Join(home, ".piper", "piperd")
	if got != want {
		t.Fatalf("DataDir = %q, want %q", got, want)
	}
}

func TestLoadClientDefault(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PIPER_ADDR", "")
	t.Setenv("PIPER_TOKEN", "")
	cc, err := LoadClient()
	if err != nil {
		t.Fatal(err)
	}
	if cc.Addr != "http://127.0.0.1:8088" || cc.Token != "" {
		t.Fatalf("cc = %+v", cc)
	}
}

func TestSaveAndLoadClient(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PIPER_ADDR", "")
	t.Setenv("PIPER_TOKEN", "")
	if err := SaveClient(ClientConfig{Addr: "http://box:8088", Token: "secret"}); err != nil {
		t.Fatal(err)
	}
	cc, err := LoadClient()
	if err != nil {
		t.Fatal(err)
	}
	if cc.Addr != "http://box:8088" || cc.Token != "secret" {
		t.Fatalf("cc = %+v", cc)
	}
}

func TestLoadClientEnvOverridesFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PIPER_ADDR", "")
	t.Setenv("PIPER_TOKEN", "")
	if err := SaveClient(ClientConfig{Addr: "http://box:8088", Token: "filetok"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PIPER_TOKEN", "envtok")
	cc, _ := LoadClient()
	if cc.Token != "envtok" {
		t.Fatalf("token = %q, want envtok", cc.Token)
	}
}

func TestClientConfigRoundTripsRelayFields(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PIPER_ADDR", "")
	t.Setenv("PIPER_TOKEN", "")
	if err := SaveClient(ClientConfig{
		Addr: "http://127.0.0.1:8088", RelayAPI: "https://api.public.getpiper.co",
		AccountCredential: "cred-xyz",
	}); err != nil {
		t.Fatalf("SaveClient: %v", err)
	}
	cc, err := LoadClient()
	if err != nil {
		t.Fatalf("LoadClient: %v", err)
	}
	if cc.RelayAPI != "https://api.public.getpiper.co" || cc.AccountCredential != "cred-xyz" {
		t.Fatalf("cc = %+v", cc)
	}
}

func TestSaveClientPreservesPersistedAgentIdentity(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PIPER_ADDR", "")
	t.Setenv("PIPER_TOKEN", "")
	path := filepath.Join(home, ".piper", "piper", "config.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"boxes":[{"name":"living-room","addr":"http://box:8088","base_domain":"cloud.example"}],"current":"living-room"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cc, err := LoadClient()
	if err != nil {
		t.Fatal(err)
	}
	if cc.BaseDomain != "cloud.example" {
		t.Fatalf("LoadClient dropped persisted agent identity: %q", cc.BaseDomain)
	}
	if err := SaveClient(cc); err != nil {
		t.Fatal(err)
	}
	cf, err := LoadClientFile()
	if err != nil || len(cf.Boxes) != 1 {
		t.Fatalf("saved config = %+v (%v)", cf, err)
	}
	data, err := json.Marshal(cf.Boxes[0])
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if got, _ := raw["base_domain"].(string); got != "cloud.example" {
		t.Fatalf("SaveClient dropped persisted agent identity: %q", got)
	}
}

func TestRelayFileRoundTripAndMissing(t *testing.T) {
	dir := t.TempDir()
	if _, found, err := LoadRelayFile(dir); err != nil || found {
		t.Fatalf("missing relay file: found=%v err=%v", found, err)
	}
	rf := RelayFile{RelayAddr: "relay:7000", RelayToken: "enr-1", BaseDomain: "ab12-alice.public.getpiper.co"}
	if err := SaveRelayFile(dir, rf); err != nil {
		t.Fatalf("SaveRelayFile: %v", err)
	}
	got, found, err := LoadRelayFile(dir)
	if err != nil || !found {
		t.Fatalf("LoadRelayFile: found=%v err=%v", found, err)
	}
	if got != rf {
		t.Fatalf("relay file = %+v, want %+v", got, rf)
	}
}

func TestSaveRelayFileSetsRestrictedMode(t *testing.T) {
	dir := t.TempDir()
	if err := SaveRelayFile(dir, RelayFile{RelayAddr: "relay:7000", RelayToken: "secret"}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, "relay.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Fatalf("relay.json mode = %04o, want 0600", got)
	}
}

func TestSaveRelayFileLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	if err := SaveRelayFile(dir, RelayFile{RelayAddr: "relay:7000", RelayToken: "secret"}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "relay.json" {
			t.Fatalf("unexpected leftover file in data dir: %s", e.Name())
		}
	}
}

func TestLoadReadsRelayFileWhenEnvUnset(t *testing.T) {
	dir := t.TempDir()
	if err := SaveRelayFile(dir, RelayFile{
		RelayAddr: "relay:7000", RelayToken: "enr-1", BaseDomain: "ab12-alice.public.getpiper.co",
	}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PIPER_DATA_DIR", dir)
	t.Setenv("PIPER_RELAY_ADDR", "")
	t.Setenv("PIPER_RELAY_TOKEN", "")
	t.Setenv("PIPER_BASE_DOMAIN", "")
	cfg := Load()
	if cfg.RelayAddr != "relay:7000" || cfg.RelayToken != "enr-1" ||
		cfg.BaseDomain != "ab12-alice.public.getpiper.co" {
		t.Fatalf("cfg = %+v", cfg)
	}
}

func TestLoadEnvOverridesRelayFile(t *testing.T) {
	dir := t.TempDir()
	if err := SaveRelayFile(dir, RelayFile{
		RelayAddr: "relay:7000", RelayToken: "enr-1", BaseDomain: "ab12-alice.public.getpiper.co",
	}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PIPER_DATA_DIR", dir)
	t.Setenv("PIPER_RELAY_ADDR", "override:9000")
	t.Setenv("PIPER_RELAY_TOKEN", "")
	t.Setenv("PIPER_BASE_DOMAIN", "")
	cfg := Load()
	if cfg.RelayAddr != "override:9000" {
		t.Fatalf("RelayAddr = %q, want env override", cfg.RelayAddr)
	}
	if cfg.RelayToken != "enr-1" { // env unset ⇒ file value
		t.Fatalf("RelayToken = %q, want file value", cfg.RelayToken)
	}
}

func TestSystemManaged(t *testing.T) {
	old := SystemEnvDir
	defer func() { SystemEnvDir = old }()

	dir := t.TempDir()
	SystemEnvDir = dir
	if !SystemManaged() {
		t.Fatal("SystemManaged = false with an existing /etc/piper, want true")
	}
	if got, want := SystemEnvFile(), filepath.Join(dir, "piperd.env"); got != want {
		t.Fatalf("SystemEnvFile = %q, want %q", got, want)
	}

	SystemEnvDir = filepath.Join(dir, "absent")
	if SystemManaged() {
		t.Fatal("SystemManaged = true with an absent dir, want false")
	}
}

func TestLoadIgnoresCorruptRelayFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "relay.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PIPER_DATA_DIR", dir)
	t.Setenv("PIPER_RELAY_ADDR", "")
	t.Setenv("PIPER_RELAY_TOKEN", "")
	t.Setenv("PIPER_BASE_DOMAIN", "")
	cfg := Load() // must not panic; degrades to zero relay values + default domain
	if cfg.RelayAddr != "" || cfg.RelayToken != "" {
		t.Fatalf("corrupt relay.json leaked values: %+v", cfg)
	}
	if cfg.BaseDomain != "piper.localhost" {
		t.Fatalf("BaseDomain = %q, want default", cfg.BaseDomain)
	}
}

func TestRelayFileTerminatedRoundTripAndLoad(t *testing.T) {
	dir := t.TempDir()
	if err := SaveRelayFile(dir, RelayFile{
		RelayAddr: "relay:7000", RelayToken: "enr-1",
		BaseDomain: "aaaa-alice.public.getpiper.co", Terminated: true,
	}); err != nil {
		t.Fatal(err)
	}
	got, _, err := LoadRelayFile(dir)
	if err != nil || !got.Terminated {
		t.Fatalf("relay file terminated = %v (err %v)", got.Terminated, err)
	}

	t.Setenv("PIPER_DATA_DIR", dir)
	t.Setenv("PIPER_RELAY_ADDR", "")
	t.Setenv("PIPER_RELAY_TOKEN", "")
	t.Setenv("PIPER_BASE_DOMAIN", "")
	t.Setenv("PIPER_RELAY_TERMINATED", "")
	if cfg := Load(); !cfg.Terminated {
		t.Fatal("Load did not read terminated from relay.json")
	}
	t.Setenv("PIPER_RELAY_TERMINATED", "0")
	// An explicit env value of 0 should win over the file's true only if we treat
	// env as authoritative; keep the rule simple: env "1" forces on, otherwise the
	// file decides. Document via this assertion:
	if cfg := Load(); !cfg.Terminated {
		t.Fatal("non-1 env must not override a terminated relay.json")
	}
}

func TestLoadWebhookSecretPrecedence(t *testing.T) {
	dir := t.TempDir()
	if err := SaveRelayFile(dir, RelayFile{
		RelayAddr: "relay:7000", RelayToken: "enr-1", WebhookSecret: "file-secret",
	}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PIPER_DATA_DIR", dir)
	t.Setenv("PIPER_RELAY_ADDR", "")
	t.Setenv("PIPER_RELAY_TOKEN", "")
	t.Setenv("PIPER_BASE_DOMAIN", "")

	t.Setenv("PIPER_WEBHOOK_SECRET", "env-secret")
	if got := Load().WebhookSecret; got != "env-secret" {
		t.Fatalf("env set, file set: WebhookSecret = %q, want env value", got)
	}

	t.Setenv("PIPER_WEBHOOK_SECRET", "")
	if got := Load().WebhookSecret; got != "file-secret" {
		t.Fatalf("env unset, file set: WebhookSecret = %q, want file value", got)
	}

	if err := SaveRelayFile(dir, RelayFile{RelayAddr: "relay:7000", RelayToken: "enr-1"}); err != nil {
		t.Fatal(err)
	}
	if got := Load().WebhookSecret; got != "" {
		t.Fatalf("env unset, file unset: WebhookSecret = %q, want zero value", got)
	}

	t.Setenv("PIPER_WEBHOOK_SECRET", "env-secret")
	if got := Load().WebhookSecret; got != "env-secret" {
		t.Fatalf("env set, file unset: WebhookSecret = %q, want env value", got)
	}
}

func TestLoadGitHubBrokeredPrecedence(t *testing.T) {
	dir := t.TempDir()
	if err := SaveRelayFile(dir, RelayFile{
		RelayAddr: "relay:7000", RelayToken: "enr-1", GitHubBrokered: true,
	}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PIPER_DATA_DIR", dir)
	t.Setenv("PIPER_RELAY_ADDR", "")
	t.Setenv("PIPER_RELAY_TOKEN", "")
	t.Setenv("PIPER_BASE_DOMAIN", "")

	// env unset, file true ⇒ true (the OR with the file value).
	t.Setenv("PIPER_GITHUB_BROKERED", "")
	if got := Load().GitHubBrokered; !got {
		t.Fatal("env unset, file true: GitHubBrokered = false, want true")
	}

	// env "1", file false ⇒ true (env wins).
	if err := SaveRelayFile(dir, RelayFile{RelayAddr: "relay:7000", RelayToken: "enr-1", GitHubBrokered: false}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PIPER_GITHUB_BROKERED", "1")
	if got := Load().GitHubBrokered; !got {
		t.Fatal("env=1, file false: GitHubBrokered = false, want true")
	}

	// env unset, file false ⇒ false.
	t.Setenv("PIPER_GITHUB_BROKERED", "")
	if got := Load().GitHubBrokered; got {
		t.Fatal("env unset, file false: GitHubBrokered = true, want false")
	}

	// Neither set ⇒ zero value (false).
	if err := SaveRelayFile(dir, RelayFile{RelayAddr: "relay:7000", RelayToken: "enr-1"}); err != nil {
		t.Fatal(err)
	}
	if got := Load().GitHubBrokered; got {
		t.Fatal("env unset, file unset: GitHubBrokered = true, want false")
	}

	// Only "1" turns it on via env; other truthy-looking strings do not, and
	// with the file also false the result stays false — pinning the actual
	// (not merely expected) behaviour.
	for _, v := range []string{"true", "0", "yes"} {
		t.Setenv("PIPER_GITHUB_BROKERED", v)
		if got := Load().GitHubBrokered; got {
			t.Fatalf("env=%q, file false: GitHubBrokered = true, want false (only exact %q enables it)", v, "1")
		}
	}
}

// A config.json that isn't in the boxes shape (e.g. the pre-v2 flat form)
// loads as empty, same as a missing file — there is no migration pre-1.0.
func TestLoadClientFileIgnoresNonV2File(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".piper", "piper")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	flat := `{"addr":"http://192.168.1.6:8088","token":"tok","relay_api":"https://api.relay","account_credential":"cred"}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(flat), 0o600); err != nil {
		t.Fatal(err)
	}

	cf, err := LoadClientFile()
	if err != nil {
		t.Fatal(err)
	}
	if len(cf.Boxes) != 0 || cf.Current != "" {
		t.Fatalf("want empty ClientFile, got %+v", cf)
	}
}

func TestLoadClientFileMissingIsEmptyNotError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cf, err := LoadClientFile()
	if err != nil {
		t.Fatal(err)
	}
	if len(cf.Boxes) != 0 {
		t.Fatalf("want no boxes, got %+v", cf)
	}
}

func TestSaveClientFileRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	in := ClientFile{
		Boxes: []Box{
			{Name: "pi4", Addr: "http://192.168.1.6:8088", Token: "a"},
			{Name: "local", Addr: "http://127.0.0.1:8088", Token: "b", RelayAPI: "https://api.relay", AccountCredential: "cred"},
		},
		Current: "pi4",
	}
	if err := SaveClientFile(in); err != nil {
		t.Fatal(err)
	}
	out, err := LoadClientFile()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("round trip mismatch:\n in: %+v\nout: %+v", in, out)
	}
}

func TestSaveCurrentBoxBaseDomainTargetsLocalDaemonAndClearsDuplicates(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PIPER_ADDR", "http://127.0.0.1:8088")
	t.Setenv("PIPER_API_ADDR", "")
	if err := SaveClientFile(ClientFile{
		Boxes: []Box{
			{Name: "remote", Addr: "http://192.168.1.6:8088", BaseDomain: "local.example"},
			{Name: "local", Addr: "127.0.0.1:8088"},
			{Name: "duplicate", Addr: "http://192.168.1.9:8088", BaseDomain: "local.example"},
			{Name: "other", Addr: "http://192.168.1.10:8088", BaseDomain: "other.example"},
		},
		Current: "remote",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := SaveCurrentBoxBaseDomain(" local.example "); err != nil {
		t.Fatal(err)
	}

	cf, err := LoadClientFile()
	if err != nil {
		t.Fatal(err)
	}
	if cf.Current != "remote" {
		t.Fatalf("identity persistence changed Current: %+v", cf)
	}
	if got := cf.Boxes[1].BaseDomain; got != "local.example" {
		t.Fatalf("local daemon box BaseDomain = %q, want local.example: %+v", got, cf.Boxes)
	}
	if cf.Boxes[0].BaseDomain != "" || cf.Boxes[2].BaseDomain != "" {
		t.Fatalf("duplicate BaseDomain identities were not cleared: %+v", cf.Boxes)
	}
	if got := cf.Boxes[3].BaseDomain; got != "other.example" {
		t.Fatalf("unrelated BaseDomain was cleared: %q", got)
	}
}

func TestSaveCurrentBoxBaseDomainNoOpDoesNotRewrite(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PIPER_ADDR", "http://127.0.0.1:8088")
	t.Setenv("PIPER_API_ADDR", "")
	if err := SaveClientFile(ClientFile{
		Boxes:   []Box{{Name: "local", Addr: "http://127.0.0.1:8088", BaseDomain: "local.example"}},
		Current: "local",
	}); err != nil {
		t.Fatal(err)
	}
	path, err := clientConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if _, err := SaveCurrentBoxBaseDomain("local.example"); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("no-op identity persistence rewrote config: before=%v after=%v", before.ModTime(), after.ModTime())
	}
}

// Enrollment always applies to the piperd on this box, reached over its Unix
// socket, so the row that receives the identity must be the one addressing
// that local daemon for every documented PIPER_API_ADDR value. PIPER_API_ADDR
// is a bind address: a wildcard or empty host means "all interfaces", which
// includes loopback, and must not stop a loopback row matching.
func TestSaveCurrentBoxBaseDomainMatchesLocalDaemonForEveryBindAddress(t *testing.T) {
	const (
		remoteRow = "remote"
		localRow  = "local"
	)
	rows := func() []Box {
		return []Box{
			{Name: remoteRow, Addr: "http://192.168.1.6:8088"},
			{Name: localRow, Addr: "http://127.0.0.1:8088"},
		}
	}
	for _, tc := range []struct {
		name    string
		apiAddr string
		boxes   []Box
		want    string
	}{
		{name: "wildcard IPv4 bind", apiAddr: "0.0.0.0:8088", boxes: rows(), want: localRow},
		{name: "port-only bind", apiAddr: ":8088", boxes: rows(), want: localRow},
		{name: "wildcard IPv6 bind", apiAddr: "[::]:8088", boxes: rows(), want: localRow},
		{name: "loopback bind", apiAddr: "127.0.0.1:8088", boxes: rows(), want: localRow},
		{name: "localhost bind", apiAddr: "localhost:8088", boxes: rows(), want: localRow},
		{name: "unset", apiAddr: "", boxes: rows(), want: localRow},
		{name: "specific host bind", apiAddr: "192.168.1.6:8088", boxes: rows(), want: remoteRow},
		{
			name:    "wildcard bind with an IPv6 loopback row",
			apiAddr: "0.0.0.0:8088",
			boxes:   []Box{{Name: remoteRow, Addr: "http://192.168.1.6:8088"}, {Name: localRow, Addr: "http://[::1]:8088"}},
			want:    localRow,
		},
		{
			name:    "specific host bind with no row addressing it",
			apiAddr: "192.168.1.7:8088",
			boxes:   rows(),
			want:    "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			// The identity follows the enrolled local daemon, never the
			// client's remote-pinned dial address.
			t.Setenv("PIPER_ADDR", "http://192.168.1.6:8088")
			t.Setenv("PIPER_API_ADDR", tc.apiAddr)
			if err := SaveClientFile(ClientFile{Boxes: tc.boxes, Current: remoteRow}); err != nil {
				t.Fatal(err)
			}
			matched, err := SaveCurrentBoxBaseDomain("mine.example")
			if err != nil {
				t.Fatal(err)
			}
			if matched != (tc.want != "") {
				t.Fatalf("matched = %v, want %v for PIPER_API_ADDR=%q", matched, tc.want != "", tc.apiAddr)
			}
			cf, err := LoadClientFile()
			if err != nil {
				t.Fatal(err)
			}
			for _, box := range cf.Boxes {
				want := ""
				if box.Name == tc.want {
					want = "mine.example"
				}
				if box.BaseDomain != want {
					t.Fatalf("box %q BaseDomain = %q, want %q for PIPER_API_ADDR=%q: %+v",
						box.Name, box.BaseDomain, want, tc.apiAddr, cf.Boxes)
				}
			}
		})
	}
}

func TestSaveClientFileSetsRestrictedMode(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := SaveClientFile(ClientFile{
		Boxes: []Box{{Name: "pi4", Addr: "http://192.168.1.6:8088", Token: "secret"}},
	}); err != nil {
		t.Fatal(err)
	}

	path, err := clientConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Fatalf("config mode = %04o, want 0600", got)
	}
}

func TestSaveClientFileLeavesNoTempFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := SaveClientFile(ClientFile{
		Boxes: []Box{{Name: "pi4", Addr: "http://192.168.1.6:8088", Token: "secret"}},
	}); err != nil {
		t.Fatal(err)
	}

	dir := filepath.Join(home, ".piper", "piper")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "config.json" && e.Name() != "config.json.lock" {
			t.Fatalf("unexpected leftover file in config dir: %s", e.Name())
		}
	}
}

func TestSaveClientFileOverwritesExisting(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	old := ClientFile{
		Boxes:   []Box{{Name: "old", Addr: "http://old:8088", Token: "oldtok"}},
		Current: "old",
	}
	if err := SaveClientFile(old); err != nil {
		t.Fatal(err)
	}
	newCfg := ClientFile{
		Boxes:   []Box{{Name: "new", Addr: "http://new:8088", Token: "newtok"}},
		Current: "new",
	}
	if err := SaveClientFile(newCfg); err != nil {
		t.Fatal(err)
	}

	out, err := LoadClientFile()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(newCfg, out) {
		t.Fatalf("overwrite mismatch:\nwant: %+v\n got: %+v", newCfg, out)
	}
}

func TestCurrentBoxFallsBackToFirst(t *testing.T) {
	cf := ClientFile{Boxes: []Box{{Name: "a"}, {Name: "b"}}, Current: "missing"}
	b, ok := cf.CurrentBox()
	if !ok || b.Name != "a" {
		t.Fatalf("want first box fallback, got %+v ok=%v", b, ok)
	}
	if _, ok := (ClientFile{}).CurrentBox(); ok {
		t.Fatal("empty file must report no box")
	}
}

func TestLoadClientReadsCurrentBox(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PIPER_ADDR", "")
	t.Setenv("PIPER_TOKEN", "")
	if err := SaveClientFile(ClientFile{
		Boxes: []Box{
			{Name: "pi4", Addr: "http://192.168.1.6:8088", Token: "pt"},
			{Name: "local", Addr: "http://127.0.0.1:8088", Token: "lt"},
		},
		Current: "pi4",
	}); err != nil {
		t.Fatal(err)
	}
	cc, err := LoadClient()
	if err != nil {
		t.Fatal(err)
	}
	if cc.Addr != "http://192.168.1.6:8088" || cc.Token != "pt" {
		t.Fatalf("want current box pi4, got %+v", cc)
	}
}

func TestLoadClientEnvOverridesStillApply(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PIPER_ADDR", "http://env.test:9000")
	t.Setenv("PIPER_TOKEN", "envtok")
	cc, err := LoadClient()
	if err != nil {
		t.Fatal(err)
	}
	if cc.Addr != "http://env.test:9000" || cc.Token != "envtok" {
		t.Fatalf("env overrides lost: %+v", cc)
	}
}

func TestSaveClientUpdatesOnlyCurrentBox(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := SaveClientFile(ClientFile{
		Boxes: []Box{
			{Name: "pi4", Addr: "http://192.168.1.6:8088", Token: "old", RelayAPI: "https://api.relay", AccountCredential: "cred"},
			{Name: "local", Addr: "http://127.0.0.1:8088", Token: "lt"},
		},
		Current: "pi4",
	}); err != nil {
		t.Fatal(err)
	}
	if err := SaveClient(ClientConfig{Addr: "http://192.168.1.6:8088", Token: "new", RelayAPI: "https://api.relay", AccountCredential: "cred"}); err != nil {
		t.Fatal(err)
	}
	cf, err := LoadClientFile()
	if err != nil {
		t.Fatal(err)
	}
	if len(cf.Boxes) != 2 {
		t.Fatalf("other boxes lost: %+v", cf)
	}
	b, _ := cf.CurrentBox()
	if b.Token != "new" || b.RelayAPI != "https://api.relay" {
		t.Fatalf("current box not updated: %+v", b)
	}
	if cf.Boxes[1].Token != "lt" {
		t.Fatalf("sibling box mutated: %+v", cf.Boxes[1])
	}
}

func TestSaveClientRepairsStaleCurrentToCurrentBox(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := SaveClientFile(ClientFile{
		Boxes: []Box{
			{Name: "a", Addr: "http://a:8088", Token: "at"},
			{Name: "b", Addr: "http://b:8088", Token: "bt"},
		},
		Current: "ghost",
	}); err != nil {
		t.Fatal(err)
	}
	if err := SaveClient(ClientConfig{Addr: "http://a:8088", Token: "new"}); err != nil {
		t.Fatal(err)
	}
	cf, err := LoadClientFile()
	if err != nil {
		t.Fatal(err)
	}
	if len(cf.Boxes) != 2 {
		t.Fatalf("want 2 boxes, got %+v", cf)
	}
	if cf.Current != "a" {
		t.Fatalf("Current = %q, want repaired to a", cf.Current)
	}
	if cf.Boxes[0].Token != "new" {
		t.Fatalf("fallback box a not updated: %+v", cf.Boxes[0])
	}
	if cf.Boxes[1].Token != "bt" {
		t.Fatalf("sibling box mutated: %+v", cf.Boxes[1])
	}
	for _, b := range cf.Boxes {
		if b.Name == "ghost" {
			t.Fatalf("stale Current created a bogus ghost box: %+v", cf)
		}
	}
}

func TestLoadListenDefaults(t *testing.T) {
	t.Setenv("PIPER_DATA_DIR", t.TempDir())
	t.Setenv("PIPER_HTTP_ADDR", "")
	t.Setenv("PIPER_HTTPS_ADDR", "")
	c := Load()
	if c.HTTPAddr != ":80" {
		t.Errorf("HTTPAddr = %q, want :80", c.HTTPAddr)
	}
	if c.HTTPSAddr != ":443" {
		t.Errorf("HTTPSAddr = %q, want :443", c.HTTPSAddr)
	}
}

func TestLoadListenOverride(t *testing.T) {
	t.Setenv("PIPER_DATA_DIR", t.TempDir())
	t.Setenv("PIPER_HTTP_ADDR", ":8080")
	t.Setenv("PIPER_HTTPS_ADDR", ":8443")
	c := Load()
	if c.HTTPAddr != ":8080" {
		t.Errorf("HTTPAddr = %q, want :8080", c.HTTPAddr)
	}
	if c.HTTPSAddr != ":8443" {
		t.Errorf("HTTPSAddr = %q, want :8443", c.HTTPSAddr)
	}
}

func TestGitHubAPIBaseFromEnv(t *testing.T) {
	t.Setenv("PIPER_DATA_DIR", t.TempDir())
	t.Setenv("PIPER_GITHUB_API_BASE", "http://127.0.0.1:9999")
	cfg := Load()
	if cfg.GitHubAPIBase != "http://127.0.0.1:9999" {
		t.Fatalf("GitHubAPIBase = %q, want the env override", cfg.GitHubAPIBase)
	}
}

func TestEnrollSocketCandidatesOrder(t *testing.T) {
	got := EnrollSocketCandidates("/tmp/dd")
	want := []string{SystemRuntimeSocket, DarwinRootSocket, filepath.Join("/tmp/dd", "piperd.sock")}
	if len(got) != len(want) {
		t.Fatalf("candidates = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("candidates[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestLoadPublicIPAndServe(t *testing.T) {
	t.Setenv("PIPER_PUBLIC_IP", "203.0.113.7")
	t.Setenv("PIPER_SERVE", "direct")
	c := Load()
	if c.PublicIP != "203.0.113.7" {
		t.Errorf("PublicIP = %q", c.PublicIP)
	}
	if c.Serve != "direct" {
		t.Errorf("Serve = %q", c.Serve)
	}
}
