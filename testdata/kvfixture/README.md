# `kvfixture` — Seeded Three-Node Raft Key-Value Store

`kvfixture` is a small, self-contained three-node Raft key-value store used by the `synthesis` demonstration to exhibit linearizability checking, earned `PASS` budgets, and tamper-evident test locking.

## Variants

Both variants are built from the same source tree via `Dockerfile` build arg `KV_VARIANT`:

- **`KV_VARIANT=buggy` (`internal/raft/lease.go`, default build):**
  The Raft leader grants itself a 5,000 ms local read lease (`leaseDuration = 5000 * time.Millisecond`), which exceeds the follower election timeout (`600–900 ms`). Under an asymmetric peer-network partition (`net.partition(role:leader)@3000..7500`), the majority side elects a new leader and commits newer writes while the isolated old leader continues serving stale reads from memory until its 5,000 ms lease expires.
- **`KV_VARIANT=kvfixed` (`internal/raft/lease_fixed.go`, `-tags kvfixed`):**
  Bounds the leader's local read lease (`LeaseDuration = 300 * time.Millisecond`) strictly below the follower election timeout (`600–900 ms`), leaving margin for clock drift (`300 ms + ~100 ms < 600 ms`). A leader isolated from a majority stops serving local reads after 300 ms — before any peer can complete an election — and falls back to the quorum read-index path, failing closed (`applied: false`) when it cannot reach a majority.

## Topology

`docker-compose.yaml` defines three services (`kv-n1`, `kv-n2`, `kv-n3`) connected to two Docker networks:

1. `kvclient` — publishes `127.0.0.1:18081..18083` for host-side health checks and client operations (`cmd/loadgen`).
2. `kvpeer` (`internal: true`) — carries Raft `AppendEntries` and `RequestVote` RPCs over network-scoped aliases (`n1-peer`, `n2-peer`, `n3-peer`).
