# SQLite metadata-index process boundaries

This fixture qualifies two declared Linux process boundaries in an isolated
65,536-token SQLite component. It is not an operator database migration,
whole-client recovery, hardware power-loss test or a production backup recipe.

## Declared controls

The parent seeds real observed-history, payload and legacy tables, heterogeneous
token spellings and an unrelated sentinel. It retains one logical source and
creates independent SQLite copies without hard links. Each child opens only its
declared private fixture and executes the production NFT schema in a transaction.

The parent requires an exact nonce-bound marker after index creation, either
before COMMIT or immediately after a successful COMMIT. It then sends SIGKILL
to that owned process and reaps it. An early exit, timeout, another signal,
unexpected output or race diagnostics refuse qualification. This does not
interrupt CREATE INDEX internally or prove any unobserved crash boundary.

WAL mode, FULL synchronization, a 64-page cache with spilling, disabled automatic
checkpointing and in-memory temporary work are checked. Each database is limited
to 64 MiB of logical SQLite pages; the observed retained WAL must also fit 64 MiB.
These are fixture limits and observations, not an OS disk quota or fixed RSS.

On recovery, the uncommitted index must be absent and the committed index present.
All canonical table bytes and row counts must match the independent source, and
integrity checking must pass. Fresh adoption, idempotence and an additional close
and reopen must preserve that state. The source remains unmodified. The same
test binary executes each owned child, retaining its normal or race instrumentation.
Other platforms explicitly skip this Linux-only subprocess control.

## CI selection

The Go job still runs normal and selected race tests for changes in these declared
NFT test files. Runtime, dependency, browser and full-client inputs are compared
to the pinned qualified anchor. Only these test files and this guide may be
excluded from that input comparison. The reviewed workflow must match exactly.
Workflow/controller changes, unknown files or types, missing references, invalid
selection or selector failure run the full jobs conservatively. A guide-only
change can skip both Go and integration jobs when their inputs remain unchanged.

Skipped jobs establish selection only. They do not establish fresh runtime,
browser, full-client, artifact, release or production qualification at a new SHA.
Historical evidence keeps its original source/run binding.

## Evidence state

This initial source describes required controls. Its native execution has not yet
been recorded in this guide. Preserve the existing production, original-negative,
corrected-simulator, full-client, browser and historical qualification separately.
All original roadmap acceptance, independent NoM authority, supported platforms,
operator/maintainer decisions and production NO_GO remain in force.
