package relay

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// enrollOnce POSTs /v1/enroll with the given JSON body and returns the decoded
// 200 response.
func enrollOnce(t *testing.T, h http.Handler, cred string, body map[string]any) map[string]any {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/enroll", &buf)
	req.Header.Set("Authorization", "Bearer "+cred)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("enroll: status %d, body %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestEnrollAPIUpsertsByBoxID(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10, 5)
	acc, _ := st.UpsertAccount("sub-1", "erin")
	cred, err := st.MintAccountCredential(acc.ID)
	if err != nil {
		t.Fatal(err)
	}
	h := NewAPI(st, nil)

	first := enrollOnce(t, h, cred, map[string]any{"box_id": "box-aaaa"})
	second := enrollOnce(t, h, cred, map[string]any{"box_id": "box-aaaa"})

	if first["base_domain"] != second["base_domain"] {
		t.Fatalf("base domain changed: %v -> %v", first["base_domain"], second["base_domain"])
	}
	if first["enrollment_token"] == second["enrollment_token"] {
		t.Fatal("token did not rotate")
	}
}

func TestEnrollAPIWithoutBoxIDMintsFreshDomains(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10, 5)
	acc, _ := st.UpsertAccount("sub-1", "erin")
	cred, err := st.MintAccountCredential(acc.ID)
	if err != nil {
		t.Fatal(err)
	}
	h := NewAPI(st, nil)

	first := enrollOnce(t, h, cred, nil)
	second := enrollOnce(t, h, cred, nil)
	if first["base_domain"] == second["base_domain"] {
		t.Fatal("bodyless enroll must keep insert-per-call semantics")
	}
}
