package relay

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

// Under SQLite every write transaction took a global lock, so a cap check
// and its insert could never interleave. On Postgres that guarantee comes
// from SELECT … FOR UPDATE on the owning row; these tests exist to fail the
// moment it is removed.

func TestEnrollForAccountCapHoldsUnderConcurrency(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10, 5)
	acc, err := st.UpsertAccount("gh-race", "race")
	if err != nil {
		t.Fatal(err)
	}
	const callers = 8
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := st.EnrollForAccount(acc.ID, "")
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	ok := 0
	for err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrQuotaExceeded):
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if ok != 3 {
		t.Fatalf("%d concurrent enrolls succeeded against a cap of 3", ok)
	}
}

// RegisterHostname had no transaction at all on SQLite, so this guarantee is
// new, not preserved: two boxes on one account registering at once must not
// overshoot the app cap.
func TestRegisterHostnameCapHoldsUnderConcurrency(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 2, 5)
	acc, err := st.UpsertAccount("gh-apps", "apps")
	if err != nil {
		t.Fatal(err)
	}
	en, err := st.EnrollForAccount(acc.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	const callers = 8
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := st.RegisterHostname(en.BaseDomain, fmt.Sprintf("app%d", i), 0)
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	ok := 0
	for err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrQuotaExceeded):
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if ok != 2 {
		t.Fatalf("%d concurrent registrations succeeded against a cap of 2", ok)
	}
}
