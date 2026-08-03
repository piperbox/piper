package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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

func TestRunEnvLsRendersAge(t *testing.T) {
	updatedAt := time.Now().Add(-(2*time.Minute + 30*time.Second))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"env":        map[string]string{"SECRET": "hunter2"},
			"updated_at": map[string]string{"SECRET": updatedAt.Format(time.RFC3339Nano)},
		})
	}))
	defer srv.Close()
	t.Setenv("PIPER_ADDR", srv.URL)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"env", "dashboard", "ls"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "SECRET=") {
		t.Errorf("stdout = %q, want SECRET= present", stdout.String())
	}
	// Old clients/servers printed KEY=value with no age; the new output appends
	// the age in parentheses.
	if !strings.Contains(stdout.String(), "(") || !strings.Contains(stdout.String(), ")") {
		t.Errorf("stdout = %q, want age rendered in parentheses", stdout.String())
	}
	// The value must still be masked by default.
	if strings.Contains(stdout.String(), "hunter2") {
		t.Errorf("stdout = %q, value must be masked", stdout.String())
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

func TestRunEnvSetMalformedArgPostsNothing(t *testing.T) {
	posts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts++
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	t.Setenv("PIPER_ADDR", srv.URL)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"env", "dashboard", "set", "A=1", "bogus"}, &stdout, &stderr); code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if posts != 0 {
		t.Errorf("posts = %d — a malformed arg must be caught before anything is saved", posts)
	}
}
