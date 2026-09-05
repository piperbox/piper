# Zone-aware placement for the second tunnel session (#531)

Fifth child of the scale-out epic (#524); follow-up 5 in
[the placement prior art](2026-09-05-tunnel-placement-prior-art.md). Builds
on two sessions per agent (#530).

## Goal

When an agent holds two tunnel sessions, the edge should put them in two
different failure zones when it can, so a zone outage does not take both.
This is Cloudflare's two-prefix split at Piper's scale.

## What changes

### Schema and store

`relay_instances` gains one nullable column, edited in place in
`schema.sql` (pre-1.x, no migrations):

```sql
zone TEXT
```

`InstanceRow` gains `Zone string`; empty means unset and is written as NULL
(`sql.NullString` on both sides). The heartbeat insert carries it. The
`ON CONFLICT` branch does not update it: a zone is fixed for the life of a
process id. Both instance projections (`instanceCols` and `ownerSelect`)
read the column, so every path that hands the edge an `InstanceRow` carries
the zone.

### Relay

`Instance` gains an exported `Zone` field. `cmd/piper-relay` reads
`PIPER_RELAY_ZONE` (default empty) and sets the field after `NewInstance`;
`row()` copies it into the heartbeat. The startup log line prints
`zone=<z>` when set. A relay that never sets the variable heartbeats NULL
and is placed exactly as today.

### Edge placement

`pickTunnel` collects the zones of the agent's live owners, skipping empty
ones. The comparator becomes:

1. prefer a candidate whose zone is not in that set — an empty zone never
   clashes, so a zoneless candidate sorts with the preferred group;
2. fewest sessions;
3. earliest started, then id.

Zone is a preference, not a filter: the existing owner-exclusion pass and
its whole-pool fallback are unchanged, and the pool never empties because
of zones. `ownersOf` (the order public traffic prefers among an agent's
owners) and `pickAPI` do not change.

### Configuration

| Env | Meaning |
|---|---|
| `PIPER_RELAY_ZONE` | optional failure-zone label; unset ⇒ zone plays no part in placement |

The relay cannot discover its zone portably, so the operator copies it in
however the platform exposes it: Kubernetes, a node label (the downward API
cannot read node labels directly, so an init step or a per-zone Deployment
sets it); ECS, the task metadata endpoint's availability zone, copied by an
entrypoint. A single-host compose deployment leaves it unset.

Setting it on only some relays weakens the guarantee: a zoneless relay is
always eligible as "a different zone", so the second session can land in
the same physical zone without the edge knowing. Set it on all or none.

## Not in scope

- Zone-aware `api.<apex>` round-robin.
- Zone in the public-traffic owner order.
- Any hard requirement that two zones exist.
- Rebalancing existing sessions when a zoned relay joins.

## Tests (written first)

- **Store:** an upserted row with a zone reads back through `LiveInstances`
  and `OwnerOf`; an empty zone reads back empty.
- **Edge:** with the agent owned by a relay in zone `a`, `pickTunnel`
  prefers a busier relay in zone `b` over an idle one in zone `a`; a
  zoneless candidate is preferred over a same-zone one; when every
  candidate is in zone `a`, it falls back to fewest sessions; an agent
  with no owners is placed by the old rule.
- **Relay main:** `PIPER_RELAY_ZONE` lands on the instance and in the
  heartbeat row.

## Docs

- `docs/runbooks/relay-deploy.md`: `PIPER_RELAY_ZONE` under relay
  configuration, with the all-or-none caveat; one sentence each in the
  Kubernetes and ECS notes on where the value comes from.
- `deploy/compose/relay/docker-compose.yml`: a comment that it is left
  unset on a single host.
- Prior-art spec: mark follow-up 5 as landed.
- `PROGRESS.md`: one line.
