package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRequireTokenRejectsAndAccepts(t *testing.T) {
	s := newTestStore(t)
	tok, err := s.CreateToken("cli", "admin")
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	var reached bool
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true })
	h := RequireToken(s, next)

	cases := []struct {
		name      string
		header    string
		wantCode  int
		wantReach bool
	}{
		{"no header", "", http.StatusUnauthorized, false},
		{"bad token", "Bearer nope", http.StatusUnauthorized, false},
		{"not bearer", "Basic xyz", http.StatusUnauthorized, false},
		{"valid", "Bearer " + tok, http.StatusOK, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reached = false
			req := httptest.NewRequest(http.MethodGet, "/v1/apps", nil)
			if c.header != "" {
				req.Header.Set("Authorization", c.header)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != c.wantCode || reached != c.wantReach {
				t.Fatalf("code=%d reached=%v, want code=%d reached=%v",
					rec.Code, reached, c.wantCode, c.wantReach)
			}
		})
	}
}

func TestRequireTokenStoreErrorIs500Not401(t *testing.T) {
	s := newTestStore(t)
	tok, err := s.CreateToken("cli", "admin")
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	// A failing store (here: closed DB) is a box problem, not a credential
	// problem — it must not be reported to the client as "invalid token".
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	h := RequireToken(s, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	req := httptest.NewRequest(http.MethodGet, "/v1/apps", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code = %d, want 500", rec.Code)
	}
}

func TestDeploymentEndpointsRequireToken(t *testing.T) {
	s := newTestStore(t)
	h := RequireToken(s, New(s, &fakeDeployer{}, "piper.localhost", "", nil, nil, nil, nil, nil, AgentInfo{}))
	for _, ep := range []struct{ method, path string }{
		{http.MethodGet, "/v1/apps/blog/deployments"},
		{http.MethodGet, "/v1/apps/blog/deployments/dep1/logs"},
		// Destructive endpoints: assert 401 explicitly, not just via the
		// mux-wide RequireToken wrap.
		{http.MethodPost, "/v1/apps/blog/stop"},
		{http.MethodDelete, "/v1/apps/blog"},
	} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(ep.method, ep.path, nil))
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without token = %d, want 401", ep.method, ep.path, rr.Code)
		}
	}
}
