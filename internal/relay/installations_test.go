package relay

import (
	"errors"
	"fmt"
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
	if err := st.LinkInstallation("55", "1001", "user", "alice"); err != nil {
		t.Fatal(err)
	}
	if err := st.LinkInstallation("66", "1001", "org", "getpiper"); err != nil {
		t.Fatal(err)
	}

	got, err := st.InstallationsForAccount(acc.ID)
	if err != nil {
		t.Fatalf("InstallationsForAccount: %v", err)
	}
	want := []Installation{
		{ID: "66", TargetType: "org", TargetLogin: "getpiper"},
		{ID: "55", TargetType: "user", TargetLogin: "alice"},
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("installations = %+v, want %+v", got, want)
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
