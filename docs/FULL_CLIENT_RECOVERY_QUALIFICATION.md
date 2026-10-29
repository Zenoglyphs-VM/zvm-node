# Full-client default-persistence recovery lane

The explicit `fullclient_recovery` lane uses the pinned op-geth service, its
catalyst Engine API, authenticated JWT HTTP listener and normal eth HTTP API.
It connects the actual production `nodecmd.Run`, Market and Markets extensions,
default 3-second polling and 30-second book recheck to those endpoints. The NoM
observation port remains synthetic; verification, public HTTP, external prices,
mainnet RPC and media are disabled.

Run the serialized lane separately from the ordinary and browser suites:

```sh
GOMAXPROCS=2 GOMEMLIMIT=5GiB go test -json -count=1 -p=2 -failfast \
  -tags=fullclient_recovery -timeout=5m \
  -run '^TestFullClientDefaultPersistenceRecovery$' ./orderbook
```

The service starts from `ethconfig.Defaults`: database cache 2048 MiB, clean trie
cache 614 MiB, dirty trie cache 1024 MiB, snapshot cache 409 MiB, buffered state
and its automatically selected path scheme. The suite refuses drift in these
pinned values. FullSync replaces peer synchronization; the data directory,
loopback ports, public test P2P identity and network isolation are fixture
overrides. No client cache is reduced. The Go memory setting is a soft GC target,
not a hard OS memory cap. Only one full service is alive at a time. Neither the
command-line geth binary nor the Karst/op-reth daemon is qualified by this lane.

The shipped genesis has empty MessagePasser storage. In this pinned client, the
payload encoder omits that empty withdrawals root, while the real newPayloadV4
endpoint requires it. A required negative control submits the unmodified
getPayloadV4 result, checks the exact INVALID refusal and unchanged canonical
head, and preserves this as an unresolved compatibility gate. It never repairs
the RPC payload and does not claim shipped-genesis client acceptance.

The positive storage cases declare a different synthetic genesis with one
nonzero MessagePasser storage slot and update and reload its genesis hash. This
keeps the real RPC encoding path usable without a production genesis change.
Compare a cold replay with the original two restore cases and five owned-process boundaries:

graceful CLI/client close and disk reopen; the exact owned CLI SQL exit 86 with
its parent-owned service closed gracefully; and an owned process containing both
the actual full service and production CLI. In the last case the original SQL
boundary exits that entire child after actual eth RPC and direct engine header
reads confirm L2 head 7. Boundaries are derivation before SQL commit, index after
the actual NFT plugin completes but before the index transaction commits, and
book cursor insertion before its transaction commits. The index wrapper delegates
the shipped NFT plugin and preserves rollback notification behavior; it checks
transaction-visible block/log rows before exiting. The book uses a fixture-only
SQLite trigger on cursor 7 after the actual log application writes. Two additional
rewind cases first persist the competing declared NoM branch through momentum 9
with the actual client and production workers. Read-only readiness verifies the
source momentum hash, index/book cursors, original order states and latest checks.
Only the declared NoM source changes back to the canonical branch. Fixture triggers
exit inside derivation chunk rollback or index/book cursor rewind, before their
transactions commit. Each fault must observe the production reorg path and actual
eth RPC/direct engine head 6; the service and CLI exit together without cleanup.
The next production child replays the canonical branch and must match the same
original cold oracle across all six categories. The parent only selects fixture
source observations and manages the declared trigger; it never repairs state.
Exit 86 skips the full service's test cleanup, Close and Stop. A new owned child
opens the same client directory and starts the production CLI. The parent removes
only the fixture's exit trigger and never derives, rewinds, indexes or repairs
canonical data. The restarted production workers must converge to the cold replay.

Each process boundary validates three exact joined children, their bounded output and
normal/abrupt service lifecycle markers. The parent checks physical directory
identity between them. It opens the final full service only after the children
joined, solely for the existing read-only chain/SQL oracle. All six original
canonical categories remain: balances, ownership, original unsigned orders,
chunks, deferred queues and executed roots. The exit is shared by the in-process
client and CLI; a standalone geth/Karst daemon is not exercised.

The canonical comparison excludes only existing observation-time, local epoch
and build-duration bookkeeping. Direct chain access is limited to that read-only
oracle. The service never exposes a custom Engine/eth adapter, changes payload
fields or invokes parent derivation/index/book repair.

These are declared-horizon, synthetic unsigned NoM observations and controlled
CLI process exits. The first two storage cases close the full service gracefully; the additional
owned process case exits the service and CLI together without service cleanup.
Kernel kill, standalone-daemon crash, hardware power loss, arbitrary scheduling,
signed NoM authority, real canonicality/finality, public chains, custodial
wallets, Karst/platform acceptance, maintainer activation and the complete N04
acceptance remain unqualified. Hosted results must be tied to the exact source;
source preparation and formatting alone do not establish runtime success.

The browser job requires a successful full-client job before its expensive
frontend, compiler and profile work starts. A failed or skipped prerequisite
leaves browser qualification incomplete. All three jobs remain required for an
overall successful source workflow. The full-client test deadline is seven
minutes for the two additional default-timer competing-branch/recovery cases;
the job, compiler, browser and normal/race bounds remain unchanged. The dependency uses the documented [GitHub Actions needs condition](https://docs.github.com/en/actions/reference/workflows-and-actions/workflow-syntax#jobsjob_idneeds).
