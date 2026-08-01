package relayclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestEnrollPostsBoxIDAndOrg(t *testing.T) {
	var got struct {
		BoxID string `json:"box_id"`
		Org   string `json:"org"`
	}
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/enroll" {
			http.NotFound(w, r)
			return
		}
		auth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&got)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"enrollment_token": "enr-1", "base_domain": "ab12-erin.public.getpiper.co",
			"tunnel_endpoint": "relay.getpiper.co:7000",
			"webhook_secret":  "whsec-1", "github_app": true,
		})
	}))
	defer srv.Close()

	en, err := New(srv.URL).Enroll(context.Background(), "cred-xyz", "box-aaaa", "acme")
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	if auth != "Bearer cred-xyz" {
		t.Fatalf("auth = %q", auth)
	}
	if got.BoxID != "box-aaaa" || got.Org != "acme" {
		t.Fatalf("body = %+v, want box-aaaa/acme", got)
	}
	if en.BaseDomain != "ab12-erin.public.getpiper.co" || en.EnrollmentToken != "enr-1" {
		t.Fatalf("enrollment = %+v", en)
	}
}
