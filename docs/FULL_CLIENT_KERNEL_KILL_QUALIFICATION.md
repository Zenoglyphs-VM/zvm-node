# Owned full-client kernel-kill recovery

This Linux-only test adds parent-requested SIGKILL at five declared write
boundaries to the existing full-client recovery lane. It uses the actual pinned
op-geth service, authenticated catalyst Engine API and production node entrypoint
with both shipped market extensions in one owned child process. The service keeps
the pinned default buffered path-state persistence and cache configuration.

The cases cover derivation, NFT index/plugin and book writes, then derivation and
index rewind writes. The original exit-86 cases and their state oracles remain.
A separate uninterrupted reference is shared by the five sequential cases.
Each case joins a graceful priming child, kills and joins its declared boundary
child, then joins a new production startup. Engine directories must retain their
physical identity. The engine is opened in the parent only after every child
joined, for the existing read-only canonical state comparison.

Before a kill, the child must independently observe the declared engine head
through both real eth RPC and direct chain access. Rewind cases also require the
production source-reorg path and the same common-ancestor header through both
views. The child publishes one complete boundary line bound to the original
declaration digest, phase, process ID and requested action, then waits. The
parent sends SIGKILL only to that exact child and requires its actual Unix signal
status. An early exit, wrong or duplicate declaration, deadline, truncated output,
race diagnostic or graceful service cleanup refuses qualification. Nine small
marker controls cover exact, absent/partial and mismatched declarations.

Only owned loopback endpoints and the existing unsigned synthetic NoM source are
used. The declared nonempty synthetic MessagePasser slot remains distinct from
shipped-genesis interoperability, whose original refusal control is unchanged.
Source horizons remain 7, 8 and 9. The parent adds/removes only declared fixture
hooks and never repairs canonical rows. Ordinary SQLite recovery can occur while
the parent removes a fixture hook before the new production startup; this is not
a proof that production startup alone performs every physical database repair.

The existing full-client binary is compiled once and reused by this additional
step. The new cases run in its normal instrumentation mode. No additional race
binary, production daemon, Karst, live chain, wallet, signing, broadcast, operator
database, hardware power loss or default-scale history is qualified here. The
fixture observes declared boundaries rather than interruptions inside arbitrary
syscalls or storage-device writes. Full N04 and all original roadmap acceptance
and independent operator/maintainer gates remain open; production stays NO_GO.

This initial candidate specifies required controls. Native execution and exact
source/run qualification must be recorded separately before reporting success.
