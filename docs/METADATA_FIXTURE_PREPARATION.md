# Bounded metadata adoption fixture preparation

The isolated adoption-cost test still uses 4,096, 16,384 and 65,536 token rows,
the same nine typed columns, four collections, malformed text and BLOB token
IDs, a single original transaction and the 64 MiB logical page bound. Schema,
production writes, recovery code and all read-only, page-full, rollback,
idempotent-adoption, file-backup and process-interruption oracles are unchanged.

Only test-fixture preparation changes. The in-memory adoption fixture executes
INSERT batches of sixteen rows with at most 144 bind arguments and reuses the
invariant owner string. The shared ID helper constructs its exceptional list
only for the first eight row positions. Disk-backup and process fixtures also
call this helper; their source files and input values remain unchanged, and all
of their original runtime oracles still require fresh qualification.
The UPSERT clause and input values are preserved. Fixture-level Exec calls fall
from N to N/16 (256, 1,024 and 4,096); this does not measure SQLite VM operations,
driver-internal retries, elapsed speedup, allocation or production capacity.
The small fixed parameter count stays below SQLite's documented historical
default of 999 and current default of 32,766; per-connection limits can differ.
See the official [parameter limit](https://www.sqlite.org/limits.html#max_variable_number).

The test also requires complete-table SHA-256 values authenticated in both
normal and selected-race streams at each of the two prior source revisions.
The CFF workflow failed in its separate browser job; its native job remains
separately qualified. The 7eb3aeb first workflow passed every original declared
native and marketplace oracle. Those runs provide reference row images, not
runtime qualification or a controlled performance benchmark for a new revision.

Reference workflows: https://github.com/edgepillar/zvm-node/actions/runs/41377799981 and https://github.com/edgepillar/zvm-node/actions/runs/41387815184.

| Token rows | Complete fixture-table SHA-256 |
| --- | --- |
| 4096 | `7390e0488f0cf609c03239b446ba8f29f0f416b640c5a48b35ecd490002b1e63` |
| 16384 | `0258337ff291525c6dc0b0336a12902f99f93c2212f736e9ce6b0da223f75be1` |
| 65536 | `c651e024c58d61bc282745a823216b4efbd23eaf89137acde4222529a82b94e9` |

Every original full-table byte comparison after a refusal, rollback and index
adoption remains. Each successful batch must affect sixteen rows, and the new
separate `METADATA-INDEX-FIXTURE-BATCH` marker reports the actual fixture call
counter and verified parent table image. The original adoption-cost marker is
unchanged. Existing normal/race cases still require the original SQLite driver
and all range, backup, reopen and SIGKILL checks.

Hosted CI must qualify the exact candidate before any elapsed improvement is
claimed. Local checks are source/format controls only; no local native Go suite
or new dependency download is needed. Before any later local native selection,
inspect FIFO, symlink, socket and other nonregular fixtures and keep them outside
managed foreground scratch. Full-suite execution remains in the authorized
hosted CI lane. Full N04, larger histories, signed NoM/finality, wallets,
hardware/platform and independent owner acceptance remain open. Production is
`NO_GO`.
