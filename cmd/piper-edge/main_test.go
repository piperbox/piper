package main

import "testing"

func TestVersionRequested(t *testing.T) {
	for _, args := range [][]string{{"version"}, {"--version"}} {
		if !versionRequested(args) {
			t.Errorf("versionRequested(%v) = false", args)
		}
	}
	if versionRequested(nil) || versionRequested([]string{"serve"}) {
		t.Error("versionRequested true for a non-version invocation")
	}
}

func TestEdgeConfigFromEnv(t *testing.T) {
	t.Setenv("PIPER_EDGE_APEX", "public.getpiper.dev")
	t.Setenv("PIPER_EDGE_TLS_ADDR", "")
	t.Setenv("PIPER_EDGE_HTTP_ADDR", "0.0.0.0:8080")
	t.Setenv("PIPER_EDGE_TUNNEL_ADDR", "")
	t.Setenv("PIPER_EDGE_PROXY_PROTOCOL", "1")
	cfg, err := configFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Apex != "public.getpiper.dev" || cfg.TLSAddr != ":443" || cfg.HTTPAddr != "0.0.0.0:8080" || cfg.TunnelAddr != ":7000" || !cfg.ProxyProto {
		t.Fatalf("cfg = %+v", cfg)
	}
	t.Setenv("PIPER_EDGE_APEX", "")
	if _, err := configFromEnv(); err == nil {
		t.Fatal("missing apex accepted")
	}
}
