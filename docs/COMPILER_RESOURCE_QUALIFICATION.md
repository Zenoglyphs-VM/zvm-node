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

## Linux compiler process groups

Each Linux invocation starts its compiler in a fresh process group. Caller
cancellation, deadline and output overflow stop that group, including ordinary
same-group children whose standard descriptors are already closed. Normal
compiler exit also stops remaining same-group children before output/slot return.
The existing input/output/stderr limits, empty environment, temporary working
directory, concurrency limit and one-second inherited-pipe bound remain.

The runner first observes leader exit with `waitid(WEXITED | WNOWAIT)`. It keeps
that direct child unreaped until the one group-stop operation completes, then
calls `Cmd.Wait` to reap the leader and join its pipe copies. A non-reaping
ownership check precedes group signaling; zero and broadcast group IDs are
refused. Group-stop/observation errors never become successful cleanup. This
ordering avoids sending a late group signal after `Cmd.Wait` releases the
leader identity for reuse. The observation goroutine is joined on every
successful launch. Other platforms retain their existing direct-child behavior.

Native tests cover normal exit with inherited and closed child descriptors,
caller cancellation, deadlines, both output limits, independent concurrent
compiler groups and cancelled/failed launches. Children have a separate finite
self deadline so a failed regression cannot leave a long-lived fixture. The
actual isolated service profile also requires two deadline child stops while
retaining every original status, backpressure, slot/cache/isolation and six
service-lifetime oracle. It reuses the existing production binary and image.

This is ordinary same-process-group cleanup, not complete hostile descendant
containment. A child can leave the group; adoption/reaping of grandchildren,
per-invocation kernel containment, parent crashes, vendor compilers, other
platforms and production operator acceptance remain separate open requirements.
A terminated zombie observation is not claimed as a joined adopted process.
The source still requires fresh exact-head CI; this guide is not execution proof.

References: [Go 1.25 command lifecycle](https://github.com/golang/go/blob/go1.25.0/src/os/exec/exec.go),
[Linux non-reaping wait](https://man7.org/linux/man-pages/man2/waitpid.2.html)
and [Linux process-group signaling](https://man7.org/linux/man-pages/man2/kill.2.html).

Process-stop observations distinguish task absence (`ENOENT` or `ESRCH`) from
permission, I/O, interruption or cancellation errors. Other read errors are
retried only within the original 300-ms observation window and never count as
stopped-process proof. A positive same-group `Z`/`X` state or kernel-task absence
is still required. Eight error-classification cases run inside the existing
start-failure control; no new test profile, service lifetime or deadline is added.
The first group-cleanup workflow remains failed: its race caller-cancellation
observation did not record an errno, so its exact cause remains NOT_ESTABLISHED.
A corrected successful observation does not reclassify that historical run.
