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
  -tags=fullclient_recovery -timeout=7m \
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
transactions commit. Each fault must observe the production reorg path, requested
common execution ancestor 6, and the retained actual eth RPC/direct engine head 8.
The pinned [catalyst forkchoice implementation](https://github.com/ethereum-optimism/op-geth/blob/e8800cffe53d459cde8a07c8e8f1de9d86e79e07/eth/catalyst/api.go#L304-L318)
does not lower the current head when the requested ancestor is already canonical.
Replacement payload selection later changes the execution branch. The fixture
records both actual head observations before refusing any mismatch; it never
forces an engine rewind. A read-only committed momentum-7 observation binds
block 6 and its hash in a declared fixture input. The child requires matching RPC
and direct-client header hashes, plus the exact production reorg from 9 to 7.
The service and CLI exit together without cleanup.
The replacement source frontier remains at momentum 9 throughout rewind
recovery. An interrupted SQL rewind can leave momentum 9 committed; presenting
frontier 8 makes the production deriver correctly wait for a source that is
behind. The three original forward-only process cases retain their 8-to-9
observation gates. The next production child replays the canonical branch and
must match the same original cold oracle across all six categories. The parent only selects fixture
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

The five original cold comparisons retain their exact SQL projection, excluding
only existing observation-time, local-epoch and build-duration bookkeeping. The
two new rewind comparisons additionally distinguish order business state from
its recorded proof coordinates. Production `observe` assigns `checked_block`,
`checked_hash` and `closed_block` to the header where a check was performed;
sequential and batched execution can validly record different canonical headers.

Before projecting exactly those three fields, the parent validates all four
original order rows from the recovered/reference snapshots: matching canonical
RPC/direct header hashes; the exact original order hash/input; a block-hash-
selected contract check matching code, fill and status; closed full-fill proof at
its recorded check block; and open checks at the current canonical tip. Original
row vectors remain in bounded job output. Every other field of every order and
every other canonical table remains an exact comparison. No parent SQL write,
Book initialization, cache observation update or recovery worker runs in this
read-only proof check. Direct chain access remains limited to the read-only
oracle and declared header observations; one isolated client does not establish
independent NoM canonicality or finality. The service never exposes a custom Engine/eth adapter, changes payload
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

### Accepted local order observation head

The production CLI supplies its initialized local derivation number and hash to
the Market order book. A retained execution-only suffix can survive an
interrupted rewind; workers must not treat its latest header as accepted order
state. The optional bound requests the exact accepted height, requires its hash
to agree, and refuses uninitialized, unsupported, unavailable or moving anchors.
Existing RPC/header, rewind-epoch, contract and conflicting-observation guards
remain in force. Standalone books without a local derivation callback retain
their existing reader behavior. This bound establishes local consistency only.

Seven focused synthetic subcases plus their root run in normal and selected
race CI. They validate an accepted head behind a retained execution tip, the
existing standalone default, missing/mismatched/unsupported anchors and a moving
accepted head, while proving these read-only observations change no stored
order bytes. The existing real-client index-rewind case remains the separate
end-to-end gate; its observed worker interleaving is not inferred from these
synthetic controls. No recovery comparison or original acceptance is relaxed.
