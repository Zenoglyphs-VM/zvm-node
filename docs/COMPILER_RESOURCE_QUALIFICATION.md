# Compiler resource and isolation qualification

`solcd` runs compiler work separately from the node. This source adds finite
per-service compiler/input/output/stderr/concurrency/deadline and retained-cache
budgets. Queued callers receive typed refusal instead of unbounded work. The
HTTP service reserves capacity before decoding a body, exposes bounded status,
redacts execution diagnostics, and distinguishes malformed input, deadline and
capacity refusal. Remote output overflow is refused rather than returned as a
short successful compiler response.

Defaults are two concurrent compiles, 16 MiB input, 64 MiB stdout, 1 MiB stderr,
two minutes per compile, 1 GiB retained cache and 16 cache files. Explicit CLI
flags choose finite limits before service start. One service instance owns its
cache; existing files count toward the budgets, and new versions cannot evict
them to make room. Cross-process/operator cache ownership remains separate.

The proposed `compiler-isolation` CI lane uses an immutable Linux amd64 Go image
and builds the actual race-instrumented `zvm-node` binary once plus its test
harness, reusing the same build cache and image. It explicitly builds the
standard `cmd/test2json` converter from that pinned Go distribution; it does
not assume a preinstalled tool binary at a fixed path. Build temporaries
use the declared build scratch, and named stages expose a failed boundary
without printing environment contents. It checks the embedded source
revision and clean-source metadata. Build networking and writable scratch are
distinct from the subsequent runtime profile. No local Docker engine, image,
Solidity compiler or Go SDK download is required by this source contribution.

The runtime profile requires kernel-observed 512 MiB memory, zero swap, two
CPUs, 64 tasks, a 64 MiB tmpfs, UID/GID 10001, a read-only root, no effective
capabilities, no-new-privileges and no non-loopback interface. Only the two
declared binaries and the explicit JSON converter are mounted. Host-only
synthetic node-secret and state
canaries, source, build cache and Docker socket are not mounted. The actual
compiler fixture also checks its inherited privilege state, absence of the
parent-only sentinel and absence of the host canary paths. No real key, node
database, wallet, NoM RPC or live chain is used.

Six actual service lifetimes cover compiler isolation, stdout/stderr overflow,
deadline/backpressure/slot reuse, non-evicting logical cache exhaustion and
physical tmpfs exhaustion. The last case fills only a fresh, explicitly owned
isolated fixture file until ENOSPC, preserves the retained compiler, removes
only that fixture, then checks a fresh operation. Kernel OOM events must remain
zero. Production service processes must stop and join; missing counters or
unestablished isolation fail visibly.

The compiler programs in this lane are controlled adversarial test fixtures,
not independently qualified vendor Solidity binaries. Component suites retain
release-hash/pin, path, archive, concurrency and cache tests. The container test
has an explicit prerequisite skip outside its dedicated job; the dedicated
profile requires its actual executable and kernel limits.

This file describes the proposed source/profile and is not evidence of an
executed CI run. Exact-source first-job output must be retained and qualified
before claiming results. Deployed compose/TLS/firewall policy, real compiler
registry/publisher authority, full production workloads and supported
platforms, independently accepted cache ownership, per-invocation descendant
containment and operator/maintainer acceptance remain open. This does not
complete N01 or the original 42-item roadmap or establish production readiness.
