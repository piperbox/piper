package e2e

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/piperbox/piper/internal/client"
	"github.com/piperbox/piper/internal/store"
)

// TestEndToEndDeploy builds piperd, runs it against real Docker + Caddy, deploys
// the sample app, and fetches it through Caddy's :80 by Host header.
// Skips unless RUN_E2E=1 and Docker is available (Caddy is embedded in piperd).
func TestEndToEndDeploy(t *testing.T) {
	if os.Getenv("RUN_E2E") != "1" {
		t.Skip("set RUN_E2E=1 to run (needs Docker + free :80/:8088/:2019)")
	}
	repoRoot, _ := filepath.Abs("../..")
	startLANPiperd(t)

	// Tokenless: the local control API needs no auth (#221) — the golden path
	// on the box is `piper up` with zero login.
	c := client.New("http://127.0.0.1:8088", "")
	if err := c.CreateApp("blog", 8080); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	dep, err := c.Deploy("blog", filepath.Join(repoRoot, "test/e2e/sampleapp"))
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	// Deploy now returns at 202, before the build runs: follow the deployment
	// to completion (the real exercise of the async polling contract) so the
	// curl-retry window below only has to absorb route propagation, not the
	// whole Docker build.
	followCtx, cancelFollow := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancelFollow()
	final, err := c.FollowDeploy(followCtx, "blog", dep.ID, io.Discard)
	if err != nil {
		t.Fatalf("FollowDeploy: %v", err)
	}
	if final.Status != "running" {
		t.Fatalf("deploy status = %q, want running", final.Status)
	}

	// Fetch through Caddy on :80 with Host: blog.piper.localhost. Retry until
	// we see the sample app's exact response — any 200 isn't enough, since a
	// stray server squatting on :80 can answer 200 with an empty body (#126).
	const wantBody = "hello piper\n"
	req, _ := http.NewRequest("GET", "http://127.0.0.1:80/", nil)
	req.Host = "blog.piper.localhost"
	var body string
	var lastStatus int
	var lastErr error
	ok := false
	for i := 0; i < 20; i++ {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			lastErr = err
			time.Sleep(500 * time.Millisecond)
			continue
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		lastStatus = resp.StatusCode
		body = string(b)
		if lastStatus == http.StatusOK && body == wantBody {
			ok = true
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !ok {
		if lastStatus == 0 {
			t.Fatalf("never connected through Caddy: %v", lastErr)
		}
		t.Fatalf("connected through Caddy but never got the app's response: last status %d, body %q (want 200 with %q)", lastStatus, body, wantBody)
	}
	fmt.Printf("e2e response: %q\n", body)

	// Stop: the hostname must stop serving the app; the app stays listed as
	// "stopped" with its history intact.
	if err := c.StopApp("blog"); err != nil {
		t.Fatalf("StopApp: %v", err)
	}
	assertRouteGone(t, req)
	apps, err := c.ListApps()
	if err != nil {
		t.Fatalf("ListApps after stop: %v", err)
	}
	if len(apps) != 1 || apps[0].Status != "stopped" {
		t.Fatalf("apps after stop = %+v, want blog stopped", apps)
	}

	// Delete: app and state gone.
	if err := c.DeleteApp("blog"); err != nil {
		t.Fatalf("DeleteApp: %v", err)
	}
	apps, err = c.ListApps()
	if err != nil {
		t.Fatalf("ListApps after delete: %v", err)
	}
	if len(apps) != 0 {
		t.Fatalf("apps after delete = %+v, want none", apps)
	}
	_, err = c.App("blog")
	var statusErr *client.StatusError
	if !errors.As(err, &statusErr) || statusErr.Code != http.StatusNotFound {
		t.Fatalf("App after delete err = %v, want HTTP 404 StatusError", err)
	}
	assertRouteGone(t, req)
}

func assertRouteGone(t *testing.T, req *http.Request) {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("fetch route after teardown: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read route response after teardown: %v", err)
	}
	if resp.StatusCode != http.StatusOK || len(body) != 0 {
		t.Fatalf("route after teardown = status %d, body %q; want 200 with empty body", resp.StatusCode, body)
	}
}

func waitPort(t *testing.T, addr string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if conn, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
			conn.Close()
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("port %s never opened", addr)
}

// startLANPiperd builds piperd and boots it LAN-only against a fresh temp data
// dir, killing it on test cleanup. Returns once the control API accepts.
func startLANPiperd(t *testing.T) {
	t.Helper()
	repoRoot, _ := filepath.Abs("../..")
	bin := filepath.Join(t.TempDir(), "piperd")
	build := exec.Command("go", "build", "-o", bin, "./cmd/piperd")
	build.Dir = repoRoot
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build piperd: %v\n%s", err, out)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, bin)
	cmd.Env = append(os.Environ(),
		"PIPER_DATA_DIR="+t.TempDir(),
		"PIPER_API_ADDR=127.0.0.1:8088",
	)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start piperd: %v", err)
	}
	killOnCleanup(t, cmd)
	waitPort(t, "127.0.0.1:8088", 15*time.Second)
}

// killOnCleanup SIGKILLs cmd during test cleanup and reaps it with Wait, so
// the port it held is released before the next test's piperd/relay binds it.
// Wait's error on a killed process is expected and ignored. t.Cleanup (unlike
// a defer in the test body) also runs when a helper fails the test with
// t.Fatal.
func killOnCleanup(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	t.Cleanup(func() {
		cmd.Process.Kill()
		_ = cmd.Wait()
	})
}

// dockerfileFor returns a one-file app image serving body on :8080, the same
// netcat single-liner as sampleapp. body must end in "\n" and contain no
// single quotes — it is passed to the shell printf as a quoted %s argument,
// so "%" and backslashes in it are data, not format directives.
func dockerfileFor(body string) string {
	return "FROM alpine:3.20\nRUN apk add --no-cache netcat-openbsd\nEXPOSE 8080\n" +
		fmt.Sprintf("CMD while true; do printf 'HTTP/1.1 200 OK\\r\\nContent-Length: %d\\r\\nConnection: close\\r\\n\\r\\n%%s\\n' '%s' | nc -l -p 8080; done\n",
			len(body), strings.TrimSuffix(body, "\n"))
}

// TestDeployFailureAndRedeploy proves the deploy orchestrator's core promise
// end-to-end: a failed build ends "failed" and never touches the running
// version, and the next good deploy swaps the served body.
func TestDeployFailureAndRedeploy(t *testing.T) {
	if os.Getenv("RUN_E2E") != "1" {
		t.Skip("set RUN_E2E=1 to run (needs Docker + free :80/:8088/:2019)")
	}
	repoRoot, _ := filepath.Abs("../..")
	startLANPiperd(t)

	c := client.New("http://127.0.0.1:8088", "")
	if err := c.CreateApp("blog", 8080); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}

	deployAndFollow := func(srcDir, wantStatus string) {
		t.Helper()
		dep, err := c.Deploy("blog", srcDir)
		if err != nil {
			t.Fatalf("Deploy(%s): %v", srcDir, err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		final, err := c.FollowDeploy(ctx, "blog", dep.ID, io.Discard)
		if err != nil {
			t.Fatalf("FollowDeploy(%s): %v", srcDir, err)
		}
		if final.Status != wantStatus {
			t.Fatalf("deploy from %s: status = %q, want %q", srcDir, final.Status, wantStatus)
		}
	}
	writeApp := func(dockerfile string) string {
		t.Helper()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	assertServes := func(want string) {
		t.Helper()
		req, _ := http.NewRequest("GET", "http://127.0.0.1:80/", nil)
		req.Host = "blog.piper.localhost"
		deadline := time.Now().Add(20 * time.Second)
		var last string
		for time.Now().Before(deadline) {
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				last = err.Error()
			} else {
				b, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if resp.StatusCode == http.StatusOK && string(b) == want {
					return
				}
				last = fmt.Sprintf("status %d body %q", resp.StatusCode, b)
			}
			time.Sleep(500 * time.Millisecond)
		}
		t.Fatalf("never served %q: last %s", want, last)
	}

	// v1: the checked-in sample app.
	deployAndFollow(filepath.Join(repoRoot, "test/e2e/sampleapp"), "running")
	assertServes("hello piper\n")

	// Broken build → "failed", and v1 keeps serving; the app row stays running.
	deployAndFollow(writeApp("FROM alpine:3.20\nRUN false\n"), "failed")
	assertServes("hello piper\n")
	apps, err := c.ListApps()
	if err != nil {
		t.Fatalf("ListApps: %v", err)
	}
	if len(apps) != 1 || apps[0].Status != "running" {
		t.Fatalf("apps after failed deploy = %+v, want blog running", apps)
	}

	// v2: a good redeploy swaps the served body.
	deployAndFollow(writeApp(dockerfileFor("hello piper v2\n")), "running")
	assertServes("hello piper v2\n")

	// History: v1 retired to "stopped", the broken row "failed", v2 "running".
	// Retiring v1's container is async relative to v2's deploy reporting
	// "running" (docker stop can take real wall-clock time), so poll like
	// assertServes rather than reading the snapshot immediately.
	want := map[string]int{"stopped": 1, "failed": 1, "running": 1}
	deadline := time.Now().Add(30 * time.Second)
	var deps []store.Deployment
	var got map[string]int
	for time.Now().Before(deadline) {
		deps, err = c.Deployments("blog")
		if err != nil {
			t.Fatalf("Deployments: %v", err)
		}
		got = map[string]int{}
		for _, d := range deps {
			got[d.Status]++
		}
		if len(deps) == 3 && got["stopped"] == want["stopped"] && got["failed"] == want["failed"] && got["running"] == want["running"] {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("deployment history statuses = %v (%d rows), want one each of stopped/failed/running", got, len(deps))
}
