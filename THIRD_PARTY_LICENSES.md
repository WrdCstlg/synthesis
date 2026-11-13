# Third-Party Licenses & Dependency Audit

This document inventories every direct, transitive, toolchain, and runtime-container dependency declared in the `synthesis` public demonstration repository (`CGO_ENABLED=0` Go binaries and Docker Compose fixture), along with compatibility with the repository's `GNU Affero General Public License v3.0 only` (`AGPL-3.0-only`, see [`LICENSE`](LICENSE)) and [`NOTICE`](NOTICE).

## 1. Go Modules & Third-Party Libraries

| Component / Module | Version | Scope | Upstream Reference | License | AGPL-3.0-only Compatibility |
|---|---|---|---|---|---|
| `synthesis.dev/synthesis` | workspace | First-party demonstration CLI, lock verifier, Wing & Gong checker, JCS recorder | `go.mod` | `AGPL-3.0-only` | First-party (`LICENSE` + `NOTICE`) |
| `synthesis.dev/kvfixture` | workspace | First-party three-node Raft toy fixture (`cmd/kv`, `cmd/loadgen`) | `testdata/kvfixture/go.mod` | `AGPL-3.0-only` | First-party (`LICENSE` + `NOTICE`); zero third-party Go deps |
| Go standard library & runtime | Go `1.22+` | Root & fixture toolchain/runtime | `https://go.dev/LICENSE` | `BSD-3-Clause` | Compatible permissive license (AGPLv3 §7 / §13) |
| `gopkg.in/yaml.v3` | `v3.0.1` | Direct dependency in root `go.mod` (YAML config/oracle decoding) | `https://pkg.go.dev/gopkg.in/yaml.v3@v3.0.1` | `MIT` and `Apache-2.0` (derived from `libyaml` MIT + Go translation Apache-2.0) | Compatible permissive licenses (AGPLv3 §7 / §13) |
| `gopkg.in/check.v1` | `v0.0.0-20161208181325-20d25e280405` | Transitive module-graph entry in `go.sum` only (test suite dependency of upstream `yaml.v3`; never imported by `synthesis.dev/synthesis` or compiled into release binaries) | `https://pkg.go.dev/gopkg.in/check.v1` | `BSD-2-Clause` | Compatible permissive license; not linked into built binaries |

Verified via `go list -m all` (`synthesis.dev/synthesis` + `gopkg.in/yaml.v3 v3.0.1`) and `testdata/kvfixture/go.mod` (`synthesis.dev/kvfixture` only). Neither module imports GPL-incompatible, proprietary, or undocumented third-party packages.

## 2. Container Base Images (`testdata/kvfixture/Dockerfile`, Built Locally at Runtime)

No prebuilt container images or binary archives are committed or distributed in git (`scripts/leak-audit.sh` rejects binaries outside `docs/`). When `./demo.sh` or `./demo.ps1` runs Docker Compose locally:

- **`golang:1.22-alpine` (`build` stage):** Compiles static `CGO_ENABLED=0` binaries `/out/kv` and `/out/loadgen` and is discarded before final images are created.
- **`alpine:3.20` (`kv` and `tools` runtime stages):** Minimal base container OS holding only the static `CGO_ENABLED=0` Go binaries `/kv` and `/loadgen` (`sh`/core BusyBox utilities for container diagnostics). Static Go binaries do not dynamically link against host system libraries.
