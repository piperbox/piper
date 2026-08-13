package relay

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"testing"
)

func TestLinkInstallationBindsToSenderAccount(t *testing.T) {
	st := openTestStore(t)
	acc, err := st.UpsertAccount("1001", "alice")
	if err != nil {
		t.Fatal(err)
	}

	if err := st.LinkInstallation("55", "1001", "user", "alice"); err != nil {
		t.Fatalf("LinkInstallation: %v", err)
	}

	got, err := st.AccountForInstallation("55")
	if err != nil {
		t.Fatalf("AccountForInstallation: %v", err)
	}
	if got != acc.ID {
		t.Fatalf("account = %q, want %q", got, acc.ID)
	}

	insts, err := st.InstallationsForAccount(acc.ID)
	if err != nil {
		t.Fatalf("InstallationsForAccount: %v", err)
	}
	if len(insts) != 1 || insts[0].ID != "55" {
		t.Fatalf("installations = %+v, want single id 55", insts)
	}
}

func TestLinkInstallationIsIdempotent(t *testing.T) {
	st := openTestStore(t)
	if _, err := st.UpsertAccount("1001", "alice"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := st.LinkInstallation("55", "1001", "user", "alice"); err != nil {
			t.Fatalf("LinkInstallation #%d: %v", i, err)
		}
	}
}

func TestLinkInstallationUnknownSender(t *testing.T) {
	st := openTestStore(t)
	err := st.LinkInstallation("55", "9999", "user", "nobody")
	if !errors.Is(err, ErrUnknownAccount) {
		t.Fatalf("err = %v, want ErrUnknownAccount", err)
	}
}

func TestAccountForInstallationUnknown(t *testing.T) {
	st := openTestStore(t)
	_, err := st.AccountForInstallation("404")
	if !errors.Is(err, ErrNoInstallation) {
		t.Fatalf("err = %v, want ErrNoInstallation", err)
	}
}

// TestLinkInstallationIfAbsentConcurrentRacers exercises the interleaving the
// serial ingress test cannot: concurrent recovery links for the SAME
// installation id, each naming a DIFFERENT account. Insert-if-absent must let
// exactly one racer insert, and the winner's row must never be replaced — a
// read-then-upsert lets several racers pass the check and the last upsert
// steals the ownership.
func TestLinkInstallationIfAbsentConcurrentRacers(t *testing.T) {
	const racers = 8
	st := openTestStore(t)
	githubIDs := make([]string, racers)
	accountIDs := make([]string, racers)
	for i := 0; i < racers; i++ {
		githubIDs[i] = strconv.Itoa(1000 + i)
		acc, err := st.UpsertAccount(githubIDs[i], fmt.Sprintf("racer-%d", i))
		if err != nil {
			t.Fatal(err)
		}
		accountIDs[i] = acc.ID
	}

	race := func(githubID string) bool {
		ok, err := st.LinkInstallationIfAbsent("55", githubID, "user", "racer")
		if err != nil {
			t.Errorf("LinkInstallationIfAbsent: %v", err)
			return false
		}
		return ok
	}

	inserted := make([]bool, racers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			inserted[i] = race(githubIDs[i])
		}(i)
	}
	close(start)
	wg.Wait()

	winner := -1
	for i, ok := range inserted {
		if !ok {
			continue
		}
		if winner >= 0 {
			t.Fatalf("racers %d and %d both report inserted", winner, i)
		}
		winner = i
	}
	if winner < 0 {
		t.Fatal("no racer reports inserted; the installation stayed unlinked")
	}
	owner, err := st.AccountForInstallation("55")
	if err != nil {
		t.Fatal(err)
	}
	if owner != accountIDs[winner] {
		t.Fatalf("owner = %q, want the winning racer's account %q", owner, accountIDs[winner])
	}

	// The link is settled: a second wave of racers must all report
	// not-inserted, and the owner must not move.
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if race(githubIDs[(i+3)%racers]) {
				t.Errorf("racer %d re-inserted an already-linked installation", i)
			}
		}(i)
	}
	wg.Wait()
	owner, err = st.AccountForInstallation("55")
	if err != nil {
		t.Fatal(err)
	}
	if owner != accountIDs[winner] {
		t.Fatalf("owner moved after the race: %q, want %q", owner, accountIDs[winner])
	}
}

func TestUnlinkInstallation(t *testing.T) {
	st := openTestStore(t)
	if _, err := st.UpsertAccount("1001", "alice"); err != nil {
		t.Fatal(err)
	}
	if err := st.LinkInstallation("55", "1001", "user", "alice"); err != nil {
		t.Fatal(err)
	}
	if err := st.UnlinkInstallation("55"); err != nil {
		t.Fatalf("UnlinkInstallation: %v", err)
	}
	if _, err := st.AccountForInstallation("55"); !errors.Is(err, ErrNoInstallation) {
		t.Fatalf("err = %v, want ErrNoInstallation", err)
	}
}

func TestInstallationsForAccountReturnsAllNewestFirst(t *testing.T) {
	st := openTestStore(t)
	acc, err := st.UpsertAccount("1001", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.LinkInstallation("66", "1001", "org", "getpiper"); err != nil {
		t.Fatal(err)
	}
	if err := st.LinkInstallation("55", "1001", "user", "alice"); err != nil {
		t.Fatal(err)
	}

	got, err := st.InstallationsForAccount(acc.ID)
	if err != nil {
		t.Fatalf("InstallationsForAccount: %v", err)
	}
	want := []Installation{
		{ID: "55", TargetType: "user", TargetLogin: "alice"},
		{ID: "66", TargetType: "org", TargetLogin: "getpiper"},
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("installations = %+v, want %+v", got, want)
	}
}

// stampInstallation overwrites an installation's created_at directly. The link
// helpers stamp time.Now(), so the only way to pin a specific pair of
// timestamps is to write them behind those helpers.
func stampInstallation(t *testing.T, st *Store, installationID, created string) {
	t.Helper()
	res, err := st.db.Exec(
		`UPDATE github_installations SET created_at=? WHERE installation_id=?`,
		created, installationID)
	if err != nil {
		t.Fatalf("stamp %s: %v", installationID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		t.Fatalf("stamp %s rows affected: %v", installationID, err)
	}
	if n != 1 {
		t.Fatalf("stamp %s: matched %d rows, want 1", installationID, n)
	}
}

// TestInstallationsForAccountNewestFirstAcrossTrimmedFractions pins the
// documented newest-first order for the timestamp pair RFC3339Nano compares
// backwards. The format trims trailing fractional zeros, so the earlier ".1Z"
// is a shorter string than the later ".15Z" and sorts after it ('Z' > '5'); a
// DESC sort on the text column therefore puts the older installation first.
// The existing ", rowid DESC" does not save it — that only breaks exact ties
// between equal created_at values, and these two differ.
func TestInstallationsForAccountNewestFirstAcrossTrimmedFractions(t *testing.T) {
	st := openTestStore(t)
	acc, err := st.UpsertAccount("1001", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.LinkInstallation("66", "1001", "org", "piperbox"); err != nil {
		t.Fatal(err)
	}
	if err := st.LinkInstallation("55", "1001", "user", "alice"); err != nil {
		t.Fatal(err)
	}
	stampInstallation(t, st, "66", "2026-01-01T00:00:00.1Z")
	stampInstallation(t, st, "55", "2026-01-01T00:00:00.15Z")

	got, err := st.InstallationsForAccount(acc.ID)
	if err != nil {
		t.Fatalf("InstallationsForAccount: %v", err)
	}
	want := []Installation{
		{ID: "55", TargetType: "user", TargetLogin: "alice"},
		{ID: "66", TargetType: "org", TargetLogin: "piperbox"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("installations = %+v, want %+v (newest first)", got, want)
	}
}

func TestInstallationsForAccountEmpty(t *testing.T) {
	st := openTestStore(t)
	acc, err := st.UpsertAccount("1001", "alice")
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.InstallationsForAccount(acc.ID)
	if err != nil {
		t.Fatalf("InstallationsForAccount: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("installations = %+v, want empty", got)
	}
}

// TestInstallationsVisibleToNewestFirstAcrossTrimmedFractions is the sibling of
// TestInstallationsForAccountNewestFirstAcrossTrimmedFractions for the
// visibility query, which carries the same defect on the same column. It is
// asserted separately rather than folded into that test because the two
// queries differ: this one unions the account's own installations with those
// of every org it belongs to, so the row it must order correctly can arrive
// through either predicate branch. Both branches are populated here — one
// installation owned by alice, one owned by an org she is a member of — so the
// ordering is exercised across the union, not within one side of it.
func TestInstallationsVisibleToNewestFirstAcrossTrimmedFractions(t *testing.T) {
	st := openTestStore(t)
	acc, err := st.UpsertAccount("1001", "alice")
	if err != nil {
		t.Fatal(err)
	}
	org, err := st.CreateOrg(acc.ID, "acme")
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	if err := st.LinkInstallationForAccount("66", org.ID, "org", "acme"); err != nil {
		t.Fatal(err)
	}
	if err := st.LinkInstallationForAccount("55", acc.ID, "user", "alice"); err != nil {
		t.Fatal(err)
	}
	stampInstallation(t, st, "66", "2026-01-01T00:00:00.1Z")
	stampInstallation(t, st, "55", "2026-01-01T00:00:00.15Z")

	got, err := st.InstallationsVisibleTo(acc.ID)
	if err != nil {
		t.Fatalf("InstallationsVisibleTo: %v", err)
	}
	want := []Installation{
		{ID: "55", TargetType: "user", TargetLogin: "alice"},
		{ID: "66", TargetType: "org", TargetLogin: "acme"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("installations = %+v, want %+v (newest first)", got, want)
	}
}
