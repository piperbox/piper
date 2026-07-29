# App Environment Variables Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Per-app environment variables saved on the box and injected into the app's container, Vercel-style: applied on the next deploy or restart, managed via `piper env` and `/v1/apps/{name}/env`.

**Architecture:** A new `app_env` table in the agent store; the two `runtime.Run` call sites in `internal/deploy` merge the stored vars under the reserved `PORT`; three REST endpoints in `internal/api` (which the dashboard reaches through the existing relay control proxy — zero relay changes); client methods and a `piper env` command on top.

**Tech Stack:** Go, modernc.org/sqlite (pure Go), net/http mux patterns already in `internal/api`.

**Spec:** `docs/superpowers/specs/2026-07-29-app-env-vars-design.md`

## Global Constraints

- **No cgo** — everything must build with `CGO_ENABLED=0` (`make cross` proves arm64).
- **Layering:** `store` persistence only; `deploy` reads env via the store it already holds; `api` is transport; `client`/CLI sit above `api`. Nothing imports up.
- **Deployment status strings** unchanged: `"building"`, `"running"`, `"failed"`, `"stopped"`.
- **Pre-1.x:** `schema.sql` edited in place; no migrations; old DBs unsupported.
- **Env key rule:** `^[A-Za-z_][A-Za-z0-9_]*$`, and `PORT` (case-insensitive) is reserved — rejected at the API, overwritten at run time.
- **Commits:** conventional style, one per task step group, each ending with:
  `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>` and referencing the tracking issue (`Part of #<N>`; the PR body carries `Closes #<N>`).
- Branch: `ozykhan/app-env-vars` (already exists, carries the spec commit).
- **Always finish a task by running its tests; finish the plan with `make verify`.**

---

### Task 0: File the tracking issue

**Files:** none (GitHub).

- [ ] **Step 1: Create the issue and note its number**

```bash
gh issue create \
  --title "[agent] per-app environment variables, injected at run time" \
  --label enhancement --label P2 --label "size/M" --label agent --label cli \
  --body "Apps have no way to receive configuration — the dashboard cannot log in because a required env var is unset on its container (motivating case). Design: docs/superpowers/specs/2026-07-29-app-env-vars-design.md — app_env table, merge under reserved PORT at both runtime.Run sites, GET/POST/DELETE /v1/apps/{name}/env, piper env set/ls/rm. Vercel semantics: saved now, applied on next deploy or restart. Runtime-only (no build args), one env set per app (previews inherit), plaintext at the existing dns_token trust tier."
```

Record the issue number; every commit below says `Part of #<N>`.

---

### Task 1: Store — `app_env` table and CRUD

**Files:**
- Modify: `internal/store/schema.sql`
- Modify: `internal/store/store.go` (new methods + `DeleteApp` cascade)
- Test: `internal/store/store_test.go`

**Interfaces:**
- Produces: `(*Store).SetAppEnv(app, key, value string) error` (upsert),
  `(*Store).DeleteAppEnv(app, key string) error` (idempotent),
  `(*Store).AppEnv(app string) (map[string]string, error)` (empty non-nil map when none). Tasks 2 and 3 call these.

- [ ] **Step 1: Write the failing tests** (append to `internal/store/store_test.go`, same package `store`; use the file's existing temp-DB constructor — grep `store_test.go` for `Open(` and mirror the first test's setup lines exactly):

```go
func TestAppEnvRoundTrip(t *testing.T) {
	s := newTestStoreForEnv(t) // replace with the file's existing constructor helper
	if _, err := s.CreateApp("blog", 8080); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	if err := s.SetAppEnv("blog", "SESSION_SECRET", "one"); err != nil {
		t.Fatalf("SetAppEnv: %v", err)
	}
	if err := s.SetAppEnv("blog", "SESSION_SECRET", "two"); err != nil {
		t.Fatalf("SetAppEnv overwrite: %v", err)
	}
	env, err := s.AppEnv("blog")
	if err != nil {
		t.Fatalf("AppEnv: %v", err)
	}
	if len(env) != 1 || env["SESSION_SECRET"] != "two" {
		t.Errorf("env = %v, want map[SESSION_SECRET:two]", env)
	}
	if err := s.DeleteAppEnv("blog", "SESSION_SECRET"); err != nil {
		t.Fatalf("DeleteAppEnv: %v", err)
	}
	if err := s.DeleteAppEnv("blog", "SESSION_SECRET"); err != nil {
		t.Fatalf("DeleteAppEnv must be idempotent: %v", err)
	}
	env, err = s.AppEnv("blog")
	if err != nil {
		t.Fatalf("AppEnv after delete: %v", err)
	}
	if env == nil || len(env) != 0 {
		t.Errorf("env = %v, want empty non-nil map", env)
	}
}

func TestDeleteAppRemovesEnv(t *testing.T) {
	s := newTestStoreForEnv(t) // replace with the file's existing constructor helper
	if _, err := s.CreateApp("blog", 8080); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	if err := s.SetAppEnv("blog", "FOO", "bar"); err != nil {
		t.Fatalf("SetAppEnv: %v", err)
	}
	if err := s.DeleteApp("blog"); err != nil {
		t.Fatalf("DeleteApp: %v", err)
	}
	var n int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM app_env WHERE app='blog'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("app_env rows after DeleteApp = %d, want 0", n)
	}
}
```

If the store exposes no `DB()` accessor, assert via `s.AppEnv("blog")` returning an empty map after `DeleteApp` instead of raw SQL — do not add an accessor just for the test.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/store/ -run 'TestAppEnv|TestDeleteAppRemovesEnv' -v`
Expected: compile FAIL — `s.SetAppEnv undefined`.

- [ ] **Step 3: Implement.** In `internal/store/schema.sql`, after the `app_domains` table:

```sql
CREATE TABLE IF NOT EXISTS app_env (
    app   TEXT NOT NULL REFERENCES apps(name),
    key   TEXT NOT NULL,
    value TEXT NOT NULL,
    PRIMARY KEY (app, key)
);
```

In `internal/store/store.go`, next to the app-domain helpers:

```go
// SetAppEnv saves one env var for app, overwriting any existing value. The
// caller (api) validates the key and gates on app existence; the table's FK is
// documentation, not enforcement.
func (s *Store) SetAppEnv(app, key, value string) error {
	_, err := s.db.Exec(`INSERT INTO app_env(app, key, value) VALUES(?,?,?)
		ON CONFLICT(app, key) DO UPDATE SET value=excluded.value`, app, key, value)
	return err
}

// DeleteAppEnv removes one env var; deleting an absent key is a no-op.
func (s *Store) DeleteAppEnv(app, key string) error {
	_, err := s.db.Exec(`DELETE FROM app_env WHERE app=? AND key=?`, app, key)
	return err
}

// AppEnv returns app's env vars; an empty (non-nil) map when it has none.
func (s *Store) AppEnv(app string) (map[string]string, error) {
	rows, err := s.db.Query(`SELECT key, value FROM app_env WHERE app=?`, app)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	env := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		env[k] = v
	}
	return env, rows.Err()
}
```

In `DeleteApp` (store.go:117), add alongside the existing child deletes:

```go
	if _, err := tx.Exec(`DELETE FROM app_env WHERE app=?`, name); err != nil {
		return err
	}
```

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/store/ -v`
Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/store/schema.sql internal/store/store.go internal/store/store_test.go
git commit -m "feat(store): per-app env vars table and CRUD

Part of #<N>

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 2: Deploy — inject stored env at both Run sites

**Files:**
- Modify: `internal/runtime/fake.go` (capture env)
- Modify: `internal/deploy/deploy.go` (helper + two call sites, deploy.go:172 and deploy.go:642 area)
- Test: `internal/deploy/deploy_test.go`

**Interfaces:**
- Consumes: `(*store.Store).AppEnv` from Task 1.
- Produces: no new exports; `runtime.FakeRuntime` gains `RunEnv map[string]string` (last Run call's env) for tests.

- [ ] **Step 1: Capture env in the fake.** In `internal/runtime/fake.go`, add the field and record it:

```go
	RunEnv          map[string]string // captures the env of the last Run call
```

```go
func (f *FakeRuntime) Run(_ context.Context, _ string, _ int, env map[string]string) (RunResult, error) {
	f.RunEnv = env
	return f.RunResultVal, f.RunErr
}
```

- [ ] **Step 2: Write the failing tests** (append to `internal/deploy/deploy_test.go`; `newStore(t)` is the file's existing helper — it seeds app `blog`; check the seeded port and use it in the PORT assertion, the code below assumes 8080):

```go
func TestDeployInjectsAppEnv(t *testing.T) {
	s, _ := newStore(t)
	if err := s.SetAppEnv("blog", "SESSION_SECRET", "s3cret"); err != nil {
		t.Fatalf("SetAppEnv: %v", err)
	}
	// A stored PORT must lose to the app's real port — it is reserved.
	if err := s.SetAppEnv("blog", "PORT", "9999"); err != nil {
		t.Fatalf("SetAppEnv PORT: %v", err)
	}
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
	}
	d := New(s, rt, newFakeCaddy(), "piper.localhost")
	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if got := rt.RunEnv["SESSION_SECRET"]; got != "s3cret" {
		t.Errorf("SESSION_SECRET = %q, want s3cret", got)
	}
	if got := rt.RunEnv["PORT"]; got != "8080" {
		t.Errorf("PORT = %q, want 8080 — stored env must not shadow the app port", got)
	}
}

func TestStartInjectsAppEnv(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
	}
	d := New(s, rt, newFakeCaddy(), "piper.localhost")
	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if err := d.Stop(context.Background(), "blog"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := s.SetAppEnv("blog", "ADDED_LATER", "yes"); err != nil {
		t.Fatalf("SetAppEnv: %v", err)
	}
	if err := d.Start(context.Background(), "blog"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := rt.RunEnv["ADDED_LATER"]; got != "yes" {
		t.Errorf("ADDED_LATER = %q — Start must pick up env saved after the deploy (apply-on-restart semantics)", got)
	}
}
```

(If `Stop`/`Start` signatures differ from `(ctx, name)`, mirror the file's existing stop/start tests.)

- [ ] **Step 3: Run to verify failure**

Run: `go test ./internal/deploy/ -run 'InjectsAppEnv' -v`
Expected: FAIL — `SESSION_SECRET = ""` (env not injected yet).

- [ ] **Step 4: Implement.** In `internal/deploy/deploy.go`, add near the two call sites:

```go
// containerEnv builds an app container's environment: the app's stored vars
// with the reserved PORT overwritten on top, so a stored var can never shadow
// the port the health check and route depend on. A store failure fails the
// deploy — running a half-configured app is worse than not running it.
func (d *Deployer) containerEnv(appName string, port int) (map[string]string, error) {
	env, err := d.store.AppEnv(appName)
	if err != nil {
		return nil, fmt.Errorf("app env: %w", err)
	}
	env["PORT"] = fmt.Sprint(port)
	return env, nil
}
```

At the fresh-deploy site (deploy.go:172, inside `buildRunHealthy`), replace:

```go
	run, err := d.runtime.Run(ctx, tag, app.Port, map[string]string{"PORT": fmt.Sprint(app.Port)})
```

with:

```go
	env, err := d.containerEnv(app.Name, app.Port)
	if err != nil {
		_, _ = io.WriteString(&log, "\nerror: "+err.Error()+"\n")
		recordFailed(build.ImageID, "", 0, log.String())
		return build, runtime.RunResult{}, log.String(), err
	}
	run, err := d.runtime.Run(ctx, tag, app.Port, env)
```

At the restart site (deploy.go:642, inside `Start`), replace:

```go
	run, err := d.runtime.Run(ctx, dep.ImageID, app.Port, map[string]string{"PORT": fmt.Sprint(app.Port)})
```

with:

```go
	env, err := d.containerEnv(app.Name, app.Port)
	if err != nil {
		return err
	}
	run, err := d.runtime.Run(ctx, dep.ImageID, app.Port, env)
```

- [ ] **Step 5: Run to verify pass**

Run: `go test ./internal/deploy/ ./internal/runtime/ -v`
Expected: all PASS (including every pre-existing deploy test — the default env is now `{PORT: <port>}` via the same helper, unchanged behavior for apps with no stored vars).

- [ ] **Step 6: Commit**

```bash
git add internal/runtime/fake.go internal/deploy/deploy.go internal/deploy/deploy_test.go
git commit -m "feat(deploy): inject stored app env at deploy and restart

Part of #<N>

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 3: API — GET/POST/DELETE /v1/apps/{name}/env

**Files:**
- Modify: `internal/api/api.go`
- Test: `internal/api/api_test.go`

**Interfaces:**
- Consumes: Task 1's store methods.
- Produces: `GET /v1/apps/{name}/env` → `200 {"env":{...}}`;
  `POST /v1/apps/{name}/env` body `{"key","value"}` → `204`, `400` bad key/`PORT`, `404` unknown app;
  `DELETE /v1/apps/{name}/env/{key}` → `204`, idempotent. Task 4's client calls these.

- [ ] **Step 1: Write the failing tests** (append to `internal/api/api_test.go`, using the file's `newTestStore` and the `New(s, &fakeDeployer{store: s}, "piper.localhost", "", nil, nil, nil, nil, nil)` construction):

```go
func TestAppEnvCRUD(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.CreateApp("blog", 8080); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	h := New(s, &fakeDeployer{store: s}, "piper.localhost", "", nil, nil, nil, nil, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/apps/blog/env",
		strings.NewReader(`{"key":"SESSION_SECRET","value":"s3cret"}`)))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("post code = %d, body %s", rec.Code, rec.Body)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/apps/blog/env", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("get code = %d", rec.Code)
	}
	var out struct {
		Env map[string]string `json:"env"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Env["SESSION_SECRET"] != "s3cret" {
		t.Errorf("env = %v", out.Env)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/v1/apps/blog/env/SESSION_SECRET", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete code = %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/apps/blog/env", nil))
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Env == nil || len(out.Env) != 0 {
		t.Errorf("env after delete = %v, want empty non-null object", out.Env)
	}
}

func TestAppEnvRejects(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.CreateApp("blog", 8080); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	h := New(s, &fakeDeployer{store: s}, "piper.localhost", "", nil, nil, nil, nil, nil)

	for _, tc := range []struct {
		name, path, body string
		want             int
	}{
		{"unknown app", "/v1/apps/ghost/env", `{"key":"A","value":"b"}`, http.StatusNotFound},
		{"bad key", "/v1/apps/blog/env", `{"key":"9BAD-KEY","value":"b"}`, http.StatusBadRequest},
		{"reserved PORT", "/v1/apps/blog/env", `{"key":"port","value":"1"}`, http.StatusBadRequest},
		{"empty key", "/v1/apps/blog/env", `{"value":"b"}`, http.StatusBadRequest},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body)))
		if rec.Code != tc.want {
			t.Errorf("%s: code = %d, want %d", tc.name, rec.Code, tc.want)
		}
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/api/ -run 'TestAppEnv' -v`
Expected: FAIL — 404s from unregistered routes.

- [ ] **Step 3: Implement.** In `internal/api/api.go`, add near the other package-level vars:

```go
// envKeyRE is the accepted env var name shape; PORT is additionally reserved.
var envKeyRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
```

and after the domains handlers (before `return mux`):

```go
	mux.HandleFunc("GET /v1/apps/{name}/env", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if !knownApp(w, r, name) {
			return
		}
		env, err := s.AppEnv(name)
		if err != nil {
			serverError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"env": env})
	})
	mux.HandleFunc("POST /v1/apps/{name}/env", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if !knownApp(w, r, name) {
			return
		}
		var in struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Key == "" {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		if !envKeyRE.MatchString(in.Key) {
			http.Error(w, "invalid env key", http.StatusBadRequest)
			return
		}
		// The deploy path owns PORT; a stored one would be silently overwritten,
		// so reject it here where the user can see why.
		if strings.EqualFold(in.Key, "PORT") {
			http.Error(w, "PORT is reserved", http.StatusBadRequest)
			return
		}
		if err := s.SetAppEnv(name, in.Key, in.Value); err != nil {
			serverError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("DELETE /v1/apps/{name}/env/{key}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if !knownApp(w, r, name) {
			return
		}
		if err := s.DeleteAppEnv(name, r.PathValue("key")); err != nil {
			serverError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
```

(`knownApp`, `writeJSON`, `serverError` already exist in this file; add `regexp` to imports if absent.)

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/api/ -v`
Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/api/api.go internal/api/api_test.go
git commit -m "feat(api): app env endpoints under /v1/apps/{name}/env

Part of #<N>

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 4: Client + `piper env` CLI

**Files:**
- Modify: `internal/client/client.go`
- Create: `cmd/piper/env.go`
- Modify: `cmd/piper/main.go` (dispatch + top-level usage text)
- Test: `cmd/piper/env_test.go`

**Interfaces:**
- Consumes: Task 3's endpoints.
- Produces: `(*client.Client).AppEnv(app string) (map[string]string, error)`,
  `SetAppEnv(app, key, value string) error`, `DeleteAppEnv(app, key string) error`;
  CLI `piper env <app> <set KEY=VALUE ... | ls [--show] | rm KEY>`.

- [ ] **Step 1: Write the failing CLI tests** (create `cmd/piper/env_test.go`, mirroring `domains_test.go`'s httptest + `run()` pattern):

```go
package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRunEnvSetPostsEachVar(t *testing.T) {
	var got []map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/apps/dashboard/env" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode: %v", err)
		}
		got = append(got, body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	t.Setenv("PIPER_ADDR", srv.URL)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"env", "dashboard", "set", "A=1", "B=two=three"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	if len(got) != 2 || got[0]["key"] != "A" || got[0]["value"] != "1" ||
		got[1]["key"] != "B" || got[1]["value"] != "two=three" {
		t.Errorf("posted = %v — values may contain '='; split on the first only", got)
	}
	if !strings.Contains(stdout.String(), "next deploy or restart") {
		t.Errorf("stdout = %q, want the apply-semantics notice", stdout.String())
	}
}

func TestRunEnvLsMasksUnlessShow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"env": map[string]string{"SECRET": "hunter2"}})
	}))
	defer srv.Close()
	t.Setenv("PIPER_ADDR", srv.URL)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"env", "dashboard", "ls"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	if strings.Contains(stdout.String(), "hunter2") || !strings.Contains(stdout.String(), "SECRET=") {
		t.Errorf("masked ls = %q, must not print the value", stdout.String())
	}

	stdout.Reset()
	if code := run([]string{"env", "dashboard", "ls", "--show"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "SECRET=hunter2") {
		t.Errorf("--show ls = %q, want the real value", stdout.String())
	}
}

func TestRunEnvRmDeletes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/v1/apps/dashboard/env/SECRET" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	t.Setenv("PIPER_ADDR", srv.URL)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"env", "dashboard", "rm", "SECRET"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./cmd/piper/ -run 'TestRunEnv' -v`
Expected: FAIL — `env` is an unknown command (usage on stderr, exit 2).

- [ ] **Step 3: Implement the client methods.** In `internal/client/client.go`, next to the domain methods:

```go
// AppEnv returns app's saved environment variables.
func (c *Client) AppEnv(app string) (map[string]string, error) {
	resp, err := c.do(http.MethodGet, "/v1/apps/"+app+"/env", "", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, responseError("env", resp)
	}
	var out struct {
		Env map[string]string `json:"env"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Env, nil
}

// SetAppEnv saves one env var; it applies on the app's next deploy or restart.
func (c *Client) SetAppEnv(app, key, value string) error {
	body, err := json.Marshal(map[string]string{"key": key, "value": value})
	if err != nil {
		return err
	}
	resp, err := c.do(http.MethodPost, "/v1/apps/"+app+"/env", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return responseError("env set", resp)
	}
	return nil
}

// DeleteAppEnv removes one env var from app.
func (c *Client) DeleteAppEnv(app, key string) error {
	resp, err := c.do(http.MethodDelete, "/v1/apps/"+app+"/env/"+key, "", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return responseError("env rm", resp)
	}
	return nil
}
```

- [ ] **Step 4: Implement the command.** Create `cmd/piper/env.go`:

```go
package main

import (
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
)

const envUsage = "usage: piper env <app> <set KEY=VALUE [KEY2=VALUE2 ...] | ls [--show] | rm KEY>"

// cmdEnv drives per-app environment variables: a thin client over
// /v1/apps/<app>/env. Vercel semantics — changes are saved immediately and
// applied on the app's next deploy or restart, never by bouncing the
// running container.
func cmdEnv(remote string, args []string, stdout, stderr io.Writer) int {
	if len(args) < 2 {
		fmt.Fprintln(stderr, envUsage)
		return 2
	}
	app, sub, rest := args[0], args[1], args[2:]
	c, ok := dialClient(remote, stderr)
	if !ok {
		return 1
	}
	switch sub {
	case "set":
		if len(rest) == 0 {
			fmt.Fprintln(stderr, envUsage)
			return 2
		}
		for _, kv := range rest {
			key, value, found := strings.Cut(kv, "=")
			if !found || key == "" {
				fmt.Fprintf(stderr, "error: %q is not KEY=VALUE\n", kv)
				return 2
			}
			if err := c.SetAppEnv(app, key, value); err != nil {
				fmt.Fprintln(stderr, "error:", err)
				return 1
			}
		}
		fmt.Fprintf(stdout, "saved %d var(s) — applied on the next deploy or restart of %s\n", len(rest), app)
		return 0
	case "ls":
		fs := flag.NewFlagSet("env ls", flag.ContinueOnError)
		fs.SetOutput(stderr)
		show := fs.Bool("show", false, "print values instead of masking them")
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		if fs.NArg() != 0 {
			fmt.Fprintln(stderr, "usage: piper env <app> ls [--show]")
			return 2
		}
		env, err := c.AppEnv(app)
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		keys := make([]string, 0, len(env))
		for k := range env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v := "******"
			if *show {
				v = env[k]
			}
			fmt.Fprintf(stdout, "%s=%s\n", k, v)
		}
		return 0
	case "rm":
		if len(rest) != 1 {
			fmt.Fprintln(stderr, envUsage)
			return 2
		}
		if err := c.DeleteAppEnv(app, rest[0]); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		fmt.Fprintf(stdout, "removed %s — applied on the next deploy or restart of %s\n", rest[0], app)
		return 0
	default:
		fmt.Fprintln(stderr, envUsage)
		return 2
	}
}
```

In `cmd/piper/main.go`, add to `run()`'s command switch (next to `case "domains":`):

```go
	case "env":
		return cmdEnv(*remote, args[1:], stdout, stderr)
```

and add one line to the top-level usage/help text where the other commands are listed (grep for the string printed on unknown command — match its formatting):

```
  env <app> ...       manage the app's environment variables
```

- [ ] **Step 5: Run to verify pass**

Run: `go test ./cmd/piper/ ./internal/client/ -v`
Expected: all PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/client/client.go cmd/piper/env.go cmd/piper/env_test.go cmd/piper/main.go
git commit -m "feat(cli): piper env set/ls/rm

Part of #<N>

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 5: Verify, PR, merge

**Files:** none new.

- [ ] **Step 1: Full gate**

Run: `make verify`
Expected: exit 0 (judge by exit status, not output — it halts at the first failing gate).

- [ ] **Step 2: Push and open the PR**

```bash
git push -u origin ozykhan/app-env-vars
gh pr create --base main --title "feat: per-app environment variables" --body "Per-app env vars with Vercel semantics: saved on the box (app_env table), injected at both runtime.Run sites under the reserved PORT, managed via GET/POST/DELETE /v1/apps/{name}/env and piper env set/ls/rm. Applied on next deploy or restart; previews inherit; runtime-only (no build args). Spec: docs/superpowers/specs/2026-07-29-app-env-vars-design.md.

Closes #<N>

🤖 Generated with [Claude Code](https://claude.com/claude-code)"
```

- [ ] **Step 3: CI green, then squash-merge**

```bash
gh pr checks <PR> --watch
gh pr merge <PR> --squash --delete-branch
```

---

### Task 6: Rollout — unblock the dashboard login

Not repo work; the motivating case. After merge:

- [ ] **Step 1: Build and install on the box** (established procedure): `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w -X github.com/piperbox/piper/internal/version.value=$(git describe --tags --always)" -o <scratch>/piperd ./cmd/piperd`, stage via `ssh hetzner-box-root 'cat > /tmp/piperd-env'` (scp is not allowlisted), checksum both ends, back up `/usr/local/bin/piperd`, `install -m 0755`, `systemctl restart piperd`. Build and install the matching `piper` CLI on the Mac.
- [ ] **Step 2: Set the dashboard's variable** — ask the user for the exact KEY=VALUE the login needs (only they know it), then `piper --remote b65c423c-ozykhan.public.getpiper.dev env dashboard set <KEY>=<VALUE>`.
- [ ] **Step 3: Apply and verify** — `piper --remote b65c423c-ozykhan.public.getpiper.dev deploy dashboard` (or stop/start), then the user confirms login works at https://piperbox.dev.
