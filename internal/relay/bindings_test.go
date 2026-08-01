package relay

import "testing"

// enrolledAgent creates an account and one agent under it, returning the
// account id and the agent's base domain (which is also its agents.name).
func enrolledAgent(t *testing.T, st *Store, githubID, login string) (string, string) {
	t.Helper()
	acc, err := st.UpsertAccount(githubID, login)
	if err != nil {
		t.Fatal(err)
	}
	en, err := st.EnrollForAccount(acc.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	return acc.ID, en.BaseDomain
}

func TestBindRepoAndLookupByRepo(t *testing.T) {
	st := openTestStore(t)
	accID, agent := enrolledAgent(t, st, "1001", "alice")

	if err := st.BindRepo(agent, "blog", "Alice/Blog", "main"); err != nil {
		t.Fatalf("BindRepo: %v", err)
	}

	// Repo matching is case-insensitive: GitHub preserves the owner's casing,
	// but the same repository must resolve however it is spelled.
	got, err := st.BindingsForRepo(accID, "alice/blog")
	if err != nil {
		t.Fatalf("BindingsForRepo: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d bindings, want 1", len(got))
	}
	if got[0].AgentName != agent || got[0].App != "blog" || got[0].Branch != "main" {
		t.Fatalf("binding = %+v", got[0])
	}
}

func TestBindRepoReplacesPerApp(t *testing.T) {
	st := openTestStore(t)
	accID, agent := enrolledAgent(t, st, "1001", "alice")

	if err := st.BindRepo(agent, "blog", "alice/old", "main"); err != nil {
		t.Fatal(err)
	}
	if err := st.BindRepo(agent, "blog", "alice/new", "trunk"); err != nil {
		t.Fatal(err)
	}

	if got, _ := st.BindingsForRepo(accID, "alice/old"); len(got) != 0 {
		t.Fatalf("old repo still bound: %+v", got)
	}
	got, _ := st.BindingsForRepo(accID, "alice/new")
	if len(got) != 1 || got[0].Branch != "trunk" {
		t.Fatalf("new binding = %+v", got)
	}
}

func TestBindingsForRepoIsAccountScoped(t *testing.T) {
	st := openTestStore(t)
	_, aliceAgent := enrolledAgent(t, st, "1001", "alice")
	bobID, _ := enrolledAgent(t, st, "2002", "bob")

	if err := st.BindRepo(aliceAgent, "blog", "alice/blog", "main"); err != nil {
		t.Fatal(err)
	}

	got, err := st.BindingsForRepo(bobID, "alice/blog")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("bob saw alice's binding: %+v", got)
	}
}

func TestAgentBoundToRepo(t *testing.T) {
	st := openTestStore(t)
	_, aliceAgent := enrolledAgent(t, st, "1001", "alice")
	_, bobAgent := enrolledAgent(t, st, "2002", "bob")

	if err := st.BindRepo(aliceAgent, "blog", "alice/blog", "main"); err != nil {
		t.Fatal(err)
	}

	ok, err := st.AgentBoundToRepo(aliceAgent, "alice/blog")
	if err != nil || !ok {
		t.Fatalf("alice's agent bound = %v, err = %v; want true", ok, err)
	}
	ok, err = st.AgentBoundToRepo(bobAgent, "alice/blog")
	if err != nil || ok {
		t.Fatalf("bob's agent bound = %v, err = %v; want false", ok, err)
	}
}

func TestUnbindRepo(t *testing.T) {
	st := openTestStore(t)
	accID, agent := enrolledAgent(t, st, "1001", "alice")
	if err := st.BindRepo(agent, "blog", "alice/blog", "main"); err != nil {
		t.Fatal(err)
	}
	if err := st.UnbindRepo(agent, "blog"); err != nil {
		t.Fatalf("UnbindRepo: %v", err)
	}
	if got, _ := st.BindingsForRepo(accID, "alice/blog"); len(got) != 0 {
		t.Fatalf("binding survived unbind: %+v", got)
	}
}
