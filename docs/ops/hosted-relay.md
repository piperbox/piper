# The hosted relay

How piperbox runs `public.getpiper.dev`: a single-node k3s cluster on a
Hetzner host, reconciled by Flux from the private `piperbox/relay-ops` repo.
Generic relay install and upgrade steps live in
[self-host/relay.md](../self-host/relay.md); this page is only what differs
for our own deployment.

## Kubernetes

`piper-edge` is a Deployment behind a TCP Service that holds the public IP
(or a cloud NLB speaking PROXY protocol, with `PIPER_EDGE_PROXY_PROTOCOL=1`;
otherwise `externalTrafficPolicy: Local`). `piper-relay` is a Deployment
with `PIPER_RELAY_PROXY_PROTOCOL=1` and a NetworkPolicy admitting only edge
pods and other relays (the `:8080` control hop). The knobs each needs:

```yaml
# relay Deployment — the parts that are not just env from this runbook
strategy:
  rollingUpdate: { maxSurge: 1, maxUnavailable: 0 }   # replacement in the pool before the old one drains
template:
  spec:
    terminationGracePeriodSeconds: 60                  # drain 20s + leave 5s + webhooks 35s
    securityContext:
      runAsUser: 65532                                 # image is root by default (#554)
      runAsNonRoot: true
      fsGroup: 65532                                   # Secret files readable by 65532
      sysctls: [{ name: net.ipv4.ip_unprivileged_port_start, value: "0" }]  # :443/:80 as uid 65532
    containers:
      - name: relay
        image: ghcr.io/piperbox/piper-relay:<version>
        env:
          - { name: PIPER_RELAY_ADVERTISE_HOST, valueFrom: { fieldRef: { fieldPath: status.podIP } } }
          - { name: PIPER_RELAY_OPS_ADDR, value: ":9090" }  # kubelet + Prometheus; default is loopback
          - { name: PIPER_RELAY_PROXY_PROTOCOL, value: "1" }
        readinessProbe: { httpGet: { path: /readyz, port: 9090 }, periodSeconds: 2 }
        livenessProbe:  { httpGet: { path: /livez,  port: 9090 }, periodSeconds: 10 }
        securityContext: { readOnlyRootFilesystem: true }
        volumeMounts:
          - { name: app-key, mountPath: /etc/piper-relay, readOnly: true }
    volumes:
      - name: app-key
        secret: { secretName: piper-relay-github-app, defaultMode: 0400 }  # the 0644 default is refused as world-readable
```

- **`/readyz`** is 503 until the relay's first heartbeat row lands (an edge
  cannot route to it before that) and from SIGTERM onward; **`/livez`** is
  200 whenever the process answers. Neither depends on Postgres: a relay
  with its database down still serves every tunnel it holds, and the edge's
  15 s instance TTL is what retires one that stopped heartbeating. Both
  probes live on the ops listener, which binds when `PIPER_RELAY_OPS_ADDR`
  is set or metrics/logs are on.
- **Edge:** `terminationGracePeriodSeconds: 30` and a
  `lifecycle.preStop.exec.command: ["sleep", "5"]` so the Service has
  removed the endpoint before the edge stops accepting; on SIGTERM it flips
  `/readyz`, refuses new connections and carries existing ones for up to
  20 s (#534). Its `PIPER_EDGE_OPS_ADDR=:9090` serves the same probes. Both
  images run as root by default (#554: the compose layout's cert and key
  mounts are root-owned); the same `runAsUser: 65532` plus the sysctl (safe
  since 1.22) makes the edge non-root too.
- **Zone:** `PIPER_RELAY_ZONE` should carry the node's
  `topology.kubernetes.io/zone`; the downward API cannot read node labels,
  so run one relay Deployment per zone with the value hard-coded and a node
  selector, or have an init step copy the label in. On ECS the task metadata
  endpoint (`${ECS_CONTAINER_METADATA_URI_V4}/task`, field
  `AvailabilityZone`) reports the zone; an entrypoint exports it before
  `exec`ing `piper-relay`.
- **Postgres:** a direct connection or a session-mode pooler. Both binaries
  hold a `LISTEN` connection, which a transaction-mode PgBouncer silently
  breaks. Concurrent first start against an empty database is safe (#555).
- **`api.<apex>`** may be terminated by an L7 ingress with a cert-manager
  certificate and routed to the relays' `:8080` Service — that port is plain
  HTTP written to be fronted with TLS — with DNS pointing `api.<apex>` at the
  ingress and the wildcard at the edge. The ingress must never take the
  wildcard: per-hostname routing to the owning pod is the dynamic map the
  edge exists to hold, and box-held certificates need L4 passthrough anyway.

**What still drops.** A relay restart closes one of each agent's two
sessions, and the edge routes through the other while the lost slot redials
onto the replacement (#530); nothing is unrouted. Restarting the edge on a
single host still drops every tunnel through it — a replacement cannot bind
the ports until the old one exits; behind a Service the edge drains (#534)
and only `:7000` forwards are cut, at the 20 s deadline. Both relays drain at once if they are
recreated together, and a plain `docker compose up -d` does that: it
recreates every replica of a scaled service within the same second, and
`COMPOSE_PARALLEL_LIMIT=1` does not change it (it bounds concurrent engine
calls, not replica order). Replace the replicas one at a time instead, as
in "Single host with compose" below (#535); on Kubernetes or ECS the
orchestrator's rolling update already provides it.

## Rolling out with Flux

The hosted relay is the stanzas above, applied by Flux from a private repo
(`piperbox/relay-ops`, one Kustomization per directory, secrets created on the
node and never committed). Nothing on that cluster is `kubectl apply`ed by
hand: Flux reverts drift within its ten-minute interval, so every change is a
commit. Two image policies (`ghcr.io/piperbox/piper-relay` and `piper-edge`)
drive upgrades:

- **Patch releases roll themselves.** The policies follow a semver range
  pinned to the current minor (`>=0.23.1 <0.24.0`); a new tag inside it makes
  Flux commit the bump into the two Deployments and apply it — relays
  surge-1/unavailable-0, then the edge's `Recreate` (one tunnel drop, agents
  redial). Expect the new version live within about ten minutes of the tag.
  Verify with `flux get images all`, `kubectl -n piper rollout status
  deployment/relay`, and two sessions per agent in `relay_instances`.
- **Minor releases are deliberate**, because by the release convention a
  relay `schema.sql` change forces a minor bump and pre-1.x has no
  migrations. Run the `ALTER TABLE` / `DROP TABLE` first, on the cluster's
  Postgres (`kubectl -n piper exec postgres-0 -- psql -U piper_relay
  piper_relay`), then widen both policy ranges in git; that commit is the
  rollout. The corollary for anyone cutting a release: **a relay schema change
  must never ship as a patch** — the hosted relay would roll it onto a
  database with the old shape.
- **Rollback** is reverting the bump commit; a schema change additionally
  needs the nightly dump (`/var/backups/piper-relay-<date>.sql.gz` on the
  node, taken from `postgres-0`).

A clean `piperd` stop or restart (SIGTERM, which is what upgrades and
`systemctl restart` send) closes both sessions at once; both relays
unregister them immediately and the restarted agent reconnects right away.
After an unclean stop (crash, power loss, network blackhole) both relays
keep the stale sessions until the yamux keepalive reaps them (about 30 s),
and every redial in that window is refused as a duplicate. Slot 0 retries
every 5 s rather than waiting the one-minute duplicate backoff, so the
box's apps are unreachable for the keepalive reap window plus at most five
seconds — about 40 s, not a minute. #538 tracks a relay-side ping that would
cut the reap window itself to about 10 s.

