package main

import (
	"context"
	"io"
	"path/filepath"
	"testing"

	"github.com/piperbox/piper/internal/runtime"
	"github.com/piperbox/piper/internal/store"
)

// stubRoutes records the hosts routing work touches, which is how these tests
// observe which hostname a deploy would actually serve on.
type stubRoutes struct{ removed []string }

func (s *stubRoutes) UpsertRoute(string, int) error    { return nil }
func (s *stubRoutes) UpsertRouteTLS(string, int) error { return nil }
func (s *stubRoutes) RemoveRoute(host string) error {
	s.removed = append(s.removed, host)
	return nil
}

// stubRuntime stands in for Docker; no test here builds or runs anything.
type stubRuntime struct{}

func (stubRuntime) Build(context.Context, string, string, io.Writer) (runtime.BuildResult, error) {
	return runtime.BuildResult{}, nil
}
func (stubRuntime) Run(context.Context, string, int, map[string]string) (runtime.RunResult, error) {
	return runtime.RunResult{}, nil
}
func (stubRuntime) WaitHealthy(context.Context, int) error              { return nil }
func (stubRuntime) Stop(context.Context, string) error                  { return nil }
func (stubRuntime) Logs(context.Context, string) (io.ReadCloser, error) { return nil, nil }
func (stubRuntime) PruneAppImages(context.Context, string, int) error   { return nil }

type recordingRegistrar struct {
	host       string
	registered []string
}

func (r *recordingRegistrar) Register(app string, pr int) (string, error) {
	r.registered = append(r.registered, app)
	return r.host, nil
}
func (r *recordingRegistrar) Deregister(string) error { return nil }

func webhookTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "piper.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := st.CreateApp("blog", 8080); err != nil {
		t.Fatal(err)
	}
	return st
}

// A git-push deploy must serve on the relay-assigned hostname, exactly as an
// API-driven deploy does. Routing <app>.<baseDom> instead puts the app two
// labels under the relay apex — outside its wildcard certificate and unknown
// to its router — so it is unreachable however healthy the container is.
func TestWebhookDeployerUsesTheRelayHostname(t *testing.T) {
	st := webhookTestStore(t)
	routes := &stubRoutes{}
	reg := &recordingRegistrar{host: "abc123-alice.relay.example"}

	d := newWebhookDeployer(st, stubRuntime{}, routes, "piper.localhost", reg, false)
	if err := d.Delete(context.Background(), "blog"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if len(reg.registered) == 0 {
		t.Fatal("webhook deployer never consulted the hostname registrar")
	}
	for _, h := range routes.removed {
		if h == "blog.piper.localhost" {
			t.Fatalf("routed the local host %q instead of the relay hostname", h)
		}
	}
}

// With no relay configured the registrar is nil and the box keeps its LAN
// convention. Guards against "fixing" the above by always requiring a relay.
func TestWebhookDeployerWithoutRelayKeepsLocalHost(t *testing.T) {
	st := webhookTestStore(t)
	routes := &stubRoutes{}

	d := newWebhookDeployer(st, stubRuntime{}, routes, "piper.localhost", nil, false)
	if err := d.Delete(context.Background(), "blog"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	var found bool
	for _, h := range routes.removed {
		if h == "blog.piper.localhost" {
			found = true
		}
	}
	if !found {
		t.Fatalf("LAN box did not route <app>.<base>; removed = %v", routes.removed)
	}
}
