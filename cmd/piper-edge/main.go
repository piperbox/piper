// Command piper-edge is the public entrypoint in front of N piper-relay
// processes: it routes :443 by SNI and :80 by Host to the relay that owns the
// agent's tunnel, and :7000 to the least-loaded relay, learning ownership
// from the relays' Postgres. It holds no certificate and terminates nothing.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/piperbox/piper/internal/relay"
	"github.com/piperbox/piper/internal/version"
)

// versionRequested reports whether args ask for the build version (cf.
// piper-relay); it also imports internal/version so the release ldflags
// stamp lands in this binary too.
func versionRequested(args []string) bool {
	return len(args) > 0 && (args[0] == "version" || args[0] == "--version")
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// configFromEnv reads the PIPER_EDGE_* listener settings. The prefix is
// deliberately not PIPER_RELAY_: a relay env file handed to an edge must
// fail, not half-work.
func configFromEnv() (relay.EdgeConfig, error) {
	apex := os.Getenv("PIPER_EDGE_APEX")
	if apex == "" {
		return relay.EdgeConfig{}, errors.New("PIPER_EDGE_APEX is required (the relays' apex, e.g. public.getpiper.dev)")
	}
	return relay.EdgeConfig{
		Apex:       apex,
		TLSAddr:    env("PIPER_EDGE_TLS_ADDR", ":443"),
		HTTPAddr:   env("PIPER_EDGE_HTTP_ADDR", ":80"),
		TunnelAddr: env("PIPER_EDGE_TUNNEL_ADDR", ":7000"),
		ProxyProto: os.Getenv("PIPER_EDGE_PROXY_PROTOCOL") == "1",
	}, nil
}

func main() {
	if versionRequested(os.Args[1:]) {
		fmt.Println(version.String())
		return
	}
	dsn := os.Getenv("PIPER_EDGE_DB_URL")
	if dsn == "" {
		log.Fatal("PIPER_EDGE_DB_URL is required (postgres://user:password@host/dbname — the relays' database)")
	}
	cfg, err := configFromEnv()
	if err != nil {
		log.Fatal(err)
	}
	if cfg.ProxyProto {
		log.Print("piper-edge: PIPER_EDGE_PROXY_PROTOCOL=1 — :443/:80/:7000 require a PROXY v2 header (trusted balancer in front)")
	}
	st, err := relay.Open(dsn)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer st.Close()

	// Infra-only ops surface, same shape and defaults as the relay's.
	opsAddr := env("PIPER_EDGE_OPS_ADDR", "127.0.0.1:9090")
	metricsOn := os.Getenv("PIPER_EDGE_METRICS") == "1"
	logsOn := os.Getenv("PIPER_EDGE_LOGS") == "1"
	var metrics *relay.Metrics
	var ring *relay.LogRing
	if metricsOn {
		metrics = relay.NewEdgeMetrics()
	}
	if logsOn {
		ring = relay.NewLogRing(1000)
		log.SetOutput(io.MultiWriter(os.Stderr, ring))
	}
	if metricsOn || logsOn {
		opsHandler := relay.NewOpsHandler(metrics, ring)
		go func() {
			log.Printf("piper-edge: ops endpoint %s (metrics=%v logs=%v)", opsAddr, metricsOn, logsOn)
			srv := &http.Server{Addr: opsAddr, Handler: opsHandler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
			if err := srv.ListenAndServe(); err != nil {
				log.Fatalf("ops endpoint: %v", err)
			}
		}()
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	log.Printf("piper-edge: TLS %s, HTTP %s, tunnel %s, apex %s", cfg.TLSAddr, cfg.HTTPAddr, cfg.TunnelAddr, cfg.Apex)
	if err := relay.ServeEdge(ctx, cfg, st, metrics); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatal(err)
	}
	log.Print("piper-edge: stopped")
}
