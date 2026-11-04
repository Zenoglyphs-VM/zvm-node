# Independent browser execution and the named CI gate

`market-browser-execution` depends only on `ci-scope`. It performs the original
browser job's pinned checkouts, builds, tests and source-drift check with the
same commands, environment, 15-minute job limit and 12-minute matrix limit.
It consumes no output artifact from another job.

The original `market-browser` check remains a final result gate. It depends on
source selection, Go, full-client recovery, compiler isolation and browser
execution. Its `always()` condition lets it examine failed or skipped jobs.
For `FULL_REQUIRED`, every dependency must actually succeed. Missing, failed,
cancelled, skipped, unknown or malformed results fail the gate. The gate also
checks that its current workflow has the reviewed literal dependency graph.

Explicit guide-only selection requires every runtime job to be skipped.
Declared NFT-test selection requires Go and compiler isolation to succeed and
the unchanged integrations to be skipped. These outcomes preserve source
selection; they do not provide new execution evidence for skipped jobs. Unknown
or inconsistent selection outputs fail the gate. The conservative classifier
continues to require full CI for workflow or other unreviewed source changes.

There is one additional small job and up to four substantial jobs can overlap,
instead of three. No extra native test, build, binary download, shared workspace,
artifact handoff or retry is introduced. GitHub runner availability can affect
the actual schedule. Measure overlap and the complete workflow rather than
inferring a speedup from the graph alone.

Qualification must identify all six job IDs, bind runtime evidence to
`market-browser-execution`, separately verify the named gate, and report the
actual capture order. Native logs may be captured after browser execution has
finished. Source review and a green gate alone do not establish the retained
native, compiler, vendor, full-client, financial or wallet runtime oracles.
Operator, platform, independent NoM finality and production acceptance remain
separate requirements.
