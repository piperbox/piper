// Package config loads piperd runtime configuration from the environment.
package config

import (
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
)

type Config struct {
	APIAddr     string // control API listen address
	WebhookAddr string // loopback webhook listener (relay mode)
	DataDir     string // directory for the SQLite file
	BaseDomain  string // apps served at <name>.<BaseDomain>
	CaddyAdmin  string // Caddy admin API base URL
	HTTPAddr    string // embedded Caddy HTTP listen address (default :80)
	HTTPSAddr   string // embedded Caddy HTTPS listen address (default :443)

	RelayAddr  string // relay tunnel endpoint; empty ⇒ LAN-only (Plan 1)
	RelayToken string // enrollment token presented to the relay
	Terminated bool   // relay-terminated shared domain: box serves :80, holds no cert
	// WebhookSecret is the HMAC key the relay signs brokered GitHub deliveries
	// with; GitHubBrokered records that the relay holds an App, so this box
	// needs no App credentials of its own.
	WebhookSecret  string
	GitHubBrokered bool
	GitHubAPIBase  string // GitHub API base URL override (tests); empty ⇒ https://api.github.com
	ACMEEmail      string // ACME account email
	ACMECA         string // ACME directory URL; empty ⇒ Let's Encrypt production
	DNSProvider    string // lego DNS provider name (e.g. "cloudflare")
	TLSCertFile    string // static cert path; set ⇒ skip ACME (tests / BYO cert)
	TLSKeyFile     string // static key path

	// PublicIP overrides the relay-observed public IP used by direct serve
	// mode's DNS guidance (split-horizon setups, never-enrolled boxes).
	PublicIP string
	// Serve pins the env-managed BYO domain's serve mode: "" (relay) | "direct".
	// API-managed domains carry serve in the store instead.
	Serve string
}

// DefaultBaseDomain is BaseDomain's built-in default: the LAN-only name apps
// are served at until something — PIPER_BASE_DOMAIN or an enrolled relay's
// relay.json — says what this box is really called. It resolves nowhere
// public, so callers that need a publicly resolvable name must treat it as
// "unset" rather than as an answer.
const DefaultBaseDomain = "piper.localhost"

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// Load builds a Config from env vars and the persisted relay.json, applying
// defaults. Env vars override relay.json, which overrides built-in defaults.
func Load() Config {
	dataDir := env("PIPER_DATA_DIR", DefaultDataDir())
	rf, _, err := LoadRelayFile(dataDir) // best-effort: a corrupt file yields zero values
	if err != nil {
		// A present-but-unreadable relay.json otherwise silently drops the box
		// to LAN-only; log it so the failure is diagnosable.
		log.Printf("piper: ignoring unreadable %s: %v", relayFilePath(dataDir), err)
	}

	return Config{
		APIAddr:     env("PIPER_API_ADDR", "127.0.0.1:8088"),
		WebhookAddr: env("PIPER_WEBHOOK_ADDR", "127.0.0.1:8089"),
		DataDir:     dataDir,
		BaseDomain:  firstNonEmpty(os.Getenv("PIPER_BASE_DOMAIN"), rf.BaseDomain, DefaultBaseDomain),
		CaddyAdmin:  env("PIPER_CADDY_ADMIN", "http://127.0.0.1:2019"),
		HTTPAddr:    env("PIPER_HTTP_ADDR", ":80"),
		HTTPSAddr:   env("PIPER_HTTPS_ADDR", ":443"),

		RelayAddr:      firstNonEmpty(os.Getenv("PIPER_RELAY_ADDR"), rf.RelayAddr),
		RelayToken:     firstNonEmpty(os.Getenv("PIPER_RELAY_TOKEN"), rf.RelayToken),
		Terminated:     os.Getenv("PIPER_RELAY_TERMINATED") == "1" || rf.Terminated,
		WebhookSecret:  firstNonEmpty(os.Getenv("PIPER_WEBHOOK_SECRET"), rf.WebhookSecret),
		GitHubBrokered: os.Getenv("PIPER_GITHUB_BROKERED") == "1" || rf.GitHubBrokered,
		GitHubAPIBase:  env("PIPER_GITHUB_API_BASE", ""),
		ACMEEmail:      env("PIPER_ACME_EMAIL", ""),
		ACMECA:         env("PIPER_ACME_CA", ""),
		DNSProvider:    env("PIPER_DNS_PROVIDER", ""),
		TLSCertFile:    env("PIPER_TLS_CERT_FILE", ""),
		TLSKeyFile:     env("PIPER_TLS_KEY_FILE", ""),
		PublicIP:       env("PIPER_PUBLIC_IP", ""),
		Serve:          env("PIPER_SERVE", ""),
	}
}

// ClientAddr returns the piperd base URL used by the piper CLI.
func ClientAddr() string {
	return env("PIPER_ADDR", "http://127.0.0.1:8088")
}

// NoBrowser reports whether PIPER_NO_BROWSER=1 asks the CLI and TUI to keep the
// browser shut — headless boxes, SSH sessions, and test harnesses driving the
// real binary. Only "1" disables the launch.
func NoBrowser() bool {
	return os.Getenv("PIPER_NO_BROWSER") == "1"
}

// defaultDataDir is piperd's SQLite home when PIPER_DATA_DIR is unset:
// ~/.piper/piperd. Falls back to ./data if the home dir can't be resolved.
func defaultDataDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "./data"
	}
	return filepath.Join(home, ".piper", "piperd")
}

// DefaultDataDir is piperd's data-dir default (~/.piper/piperd) when
// PIPER_DATA_DIR is unset. piperd's enrollment socket (applying a `piper
// login` claim) writes relay.json here, the same place it reads it back.
func DefaultDataDir() string { return defaultDataDir() }

// SystemEnvDir is where the shipped systemd install keeps piperd's
// EnvironmentFile. Its presence marks a systemd-managed box as
// operator-managed: piperd's enrollment socket refuses a `piper login` claim
// instead of writing relay.json, since PIPER_RELAY_* set there overrides it
// anyway. A var so tests can point it at a scratch directory.
var SystemEnvDir = "/etc/piper"

// SystemStateDir is piperd's DynamicUser StateDirectory under the shipped
// systemd unit (Environment=PIPER_DATA_DIR= in piperd.service). `piperd token`
// targets it on a systemd-managed box so tokens land in the DB the running
// service reads. A var so tests can point it at a scratch directory.
var SystemStateDir = "/var/lib/piper"

// SystemEnvFile is piperd's EnvironmentFile within SystemEnvDir.
func SystemEnvFile() string { return filepath.Join(SystemEnvDir, "piperd.env") }

// SystemRuntimeSocket is piperd's enrollment socket under the shipped systemd
// unit's RuntimeDirectory=piper; DarwinRootSocket is its equivalent for a root
// (`sudo brew services`) macOS install. Vars so tests can point them at
// scratch paths.
var SystemRuntimeSocket = "/run/piper/piperd.sock"
var DarwinRootSocket = "/var/run/piper/piperd.sock"

// EnrollSocketCandidates lists, in probe order, where a local piperd may be
// serving its enrollment socket: the systemd runtime dir, the darwin root
// path, then the per-user data dir. Probing a path that does not exist on the
// current platform is harmless — connect simply fails.
func EnrollSocketCandidates(dataDir string) []string {
	return []string{SystemRuntimeSocket, DarwinRootSocket, filepath.Join(dataDir, "piperd.sock")}
}

// SystemManaged reports whether piperd is installed under the shipped systemd
// unit, detected by the presence of /etc/piper (the installer creates it). It's
// a plain 0700 root dir, so a non-root login user can still Stat it — statting
// the inode needs only search permission on /etc, not access to the dir itself.
func SystemManaged() bool {
	fi, err := os.Stat(SystemEnvDir)
	return err == nil && fi.IsDir()
}

// ClientConfig is the piper CLI's saved credentials/target. Addr/Token are the
// LAN path (bearer to piperd); RelayAPI/AccountCredential are the relay path
// (device-flow login), written by `piper login` and read by every other
// relay-backed command (e.g. `piper github repos`, remote `piper box`).
type ClientConfig struct {
	Addr              string `json:"addr"`
	Token             string `json:"token"`
	RelayAPI          string `json:"relay_api,omitempty"`
	AccountCredential string `json:"account_credential,omitempty"`
}

// Box is one named piperd target in the piper CLI's config file. Addr/Token
// are the LAN path; RelayAPI/AccountCredential the relay path (wizard-managed).
type Box struct {
	Name              string `json:"name"`
	Addr              string `json:"addr"`
	Token             string `json:"token"`
	RelayAPI          string `json:"relay_api,omitempty"`
	AccountCredential string `json:"account_credential,omitempty"`
}

// ClientFile is the on-disk shape of ~/.piper/piper/config.json: named boxes
// plus the current selection.
type ClientFile struct {
	Boxes   []Box  `json:"boxes"`
	Current string `json:"current"`
}

// CurrentBox returns the box named by Current, falling back to the first box.
func (cf ClientFile) CurrentBox() (Box, bool) {
	for _, b := range cf.Boxes {
		if b.Name == cf.Current {
			return b, true
		}
	}
	if len(cf.Boxes) > 0 {
		return cf.Boxes[0], true
	}
	return Box{}, false
}

// LoadClientFile reads ~/.piper/piper/config.json. A missing file is not an
// error.
func LoadClientFile() (ClientFile, error) {
	var cf ClientFile
	path, err := clientConfigPath()
	if err != nil {
		return cf, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cf, nil
	}
	if err != nil {
		return cf, err
	}
	_ = json.Unmarshal(data, &cf)
	if len(cf.Boxes) > 0 && cf.Current == "" {
		cf.Current = cf.Boxes[0].Name
	}
	return cf, nil
}

// SaveClientFile writes cf to ~/.piper/piper/config.json with 0600 perms,
// creating the directory if needed. The write is atomic: bytes are staged to a
// temp file in the same directory, fsync'd, and renamed over the real path so a
// crash mid-write cannot leave the config truncated or half-written.
func SaveClientFile(cf ClientFile) error {
	path, err := clientConfigPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cf, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteFile(path, data)
}

// atomicWriteFile writes data to path atomically: bytes are staged to a temp
// file in the destination directory, fsync'd, and renamed over path. The temp
// file is removed if any error occurs before the rename. The file is created
// with 0600 permissions because callers store tokens and relay credentials.
func atomicWriteFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	// Stage the write in the destination directory so the rename stays within
	// one filesystem and is atomic on POSIX. Use a restrictive mode because the
	// file holds tokens and relay credentials.
	f, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	// Remove the temp file if anything fails before the rename; after a
	// successful rename this is a no-op.
	defer os.Remove(tmp)

	if err := os.Chmod(tmp, 0o600); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	// Sync before renaming so a crash after the rename finds the bytes on disk,
	// not just in the page cache.
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func clientConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".piper", "piper", "config.json"), nil
}

// LoadClient reads the current box from ~/.piper/piper/config.json, then
// applies PIPER_ADDR / PIPER_TOKEN env overrides and the localhost default
// for Addr. A missing file is not an error.
func LoadClient() (ClientConfig, error) {
	var cc ClientConfig
	cf, err := LoadClientFile()
	if err != nil {
		return cc, err
	}
	if b, ok := cf.CurrentBox(); ok {
		cc = ClientConfig{Addr: b.Addr, Token: b.Token, RelayAPI: b.RelayAPI, AccountCredential: b.AccountCredential}
	}
	if v := os.Getenv("PIPER_ADDR"); v != "" {
		cc.Addr = v
	}
	if cc.Addr == "" {
		cc.Addr = "http://127.0.0.1:8088"
	}
	if v := os.Getenv("PIPER_TOKEN"); v != "" {
		cc.Token = v
	}
	return cc, nil
}

// SaveClient writes cc into the current box of ~/.piper/piper/config.json
// (creating a "default" box if none exists), preserving all other boxes.
func SaveClient(cc ClientConfig) error {
	cf, err := LoadClientFile()
	if err != nil {
		return err
	}
	name := cf.Current
	if name == "" {
		name = "default"
	}
	if b, ok := cf.CurrentBox(); ok {
		name = b.Name
	}
	updated := false
	for i := range cf.Boxes {
		if cf.Boxes[i].Name == name {
			cf.Boxes[i].Addr = cc.Addr
			cf.Boxes[i].Token = cc.Token
			cf.Boxes[i].RelayAPI = cc.RelayAPI
			cf.Boxes[i].AccountCredential = cc.AccountCredential
			updated = true
			break
		}
	}
	if !updated {
		cf.Boxes = append(cf.Boxes, Box{Name: name, Addr: cc.Addr, Token: cc.Token, RelayAPI: cc.RelayAPI, AccountCredential: cc.AccountCredential})
	}
	cf.Current = name
	return SaveClientFile(cf)
}

// RelayFile is the persisted relay enrollment written by piperd (applying a
// `piper login` claim over its enrollment socket) and read by piperd at
// startup. Environment variables override these values.
type RelayFile struct {
	RelayAddr  string `json:"relay_addr"`
	RelayToken string `json:"relay_token"`
	BaseDomain string `json:"base_domain"`
	Terminated bool   `json:"terminated,omitempty"`
	// WebhookSecret is the HMAC key the relay signs brokered GitHub deliveries
	// with; GitHubBrokered records that the relay holds an App, so this box
	// needs no App credentials of its own.
	WebhookSecret  string `json:"webhook_secret,omitempty"`
	GitHubBrokered bool   `json:"github_brokered,omitempty"`
}

func relayFilePath(dataDir string) string { return filepath.Join(dataDir, "relay.json") }

// SaveRelayFile writes rf to <dataDir>/relay.json with 0600 perms, creating the
// directory if needed. The write is atomic: bytes are staged to a temp file in
// the same directory, fsync'd, and renamed over the real path.
func SaveRelayFile(dataDir string, rf RelayFile) error {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(rf, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteFile(relayFilePath(dataDir), data)
}

// LoadRelayFile reads <dataDir>/relay.json. A missing file is not an error:
// found is false and rf is the zero value.
func LoadRelayFile(dataDir string) (RelayFile, bool, error) {
	var rf RelayFile
	data, err := os.ReadFile(relayFilePath(dataDir))
	if errors.Is(err, os.ErrNotExist) {
		return rf, false, nil
	}
	if err != nil {
		return rf, false, err
	}
	if err := json.Unmarshal(data, &rf); err != nil {
		return rf, false, err
	}
	return rf, true, nil
}

// firstNonEmpty returns the first non-empty string, or "".
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
