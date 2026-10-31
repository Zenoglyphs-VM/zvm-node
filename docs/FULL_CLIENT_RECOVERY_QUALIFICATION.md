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

### Closed-cache provenance recovery

Closed rows with stale or missing observations are recovered incrementally after
ordinary open-order work. At most 100 rows, 16 MiB of original input and five
seconds are admitted per pass. Three partial indexes bound selection of stale
epochs and structural gaps without rescanning a healthy closed history at each
poll. Index creation is a transactional migration; large-history migration cost
and operator adoption remain separate unqualified gates.

Each recovery reads checkOrder and the independent filled(orderHash) getter in
one block-hash-selected multicall. Early cancellation/signature/time refusal
words cannot invent historical fills. A permanent historical observation is
retained only when its canonical header, code and complete fill state agree.
Otherwise only a new accepted-header observation may be established; a missing
first-closure time is never fabricated. Original inputs/hashes remain unchanged.
Writers check the rewind epoch and compare the captured row before committing;
all network reads precede the SQL writer. Refusal holds unproved state.

The direct order/all-orders routes expose whether observations and displayed
fill counts are verified. Other history consumers and complete historical
first-closure reconstruction require separate review. The declared native
legacy case changes only two derived cache rows after its first child joins;
production performs all recovery after restart. It makes no claim that an
observed crash produced those exact seed bytes. All original full-client cases,
canonical business fields, proof checks and 42 acceptance requirements remain.

### Hash-bound NFT URI observations and derived history

URI reads select the exact indexed block hash. Missing host capability, malformed
ABI data and failed reads produce unknown cache fields; they never fall back to
latest state. Live and backfill log records carry the source block hash. A SQL
hash fence precedes payload/cache writes, and those writes share the caller's
index transaction. Collection name/symbol display caches are a separate legacy
policy; the new journal binds contractURI and its resolved document, not those
two contract properties.

Versioned content-addressed payloads avoid duplicate documents. A payload is at
most 1 MiB, with at most 16 MiB of new distinct payload bytes admitted per block;
accounting survives rollback. Existing cooperative URI time/gas/size limits
remain. The 128-key restore pages do not hold the full history in Go memory.
These are algorithmic admission limits, not measured RSS or disk guarantees.
The existing architecture still performs URI RPC reads inside the plugin's SQL
transaction. Prepared-input reads outside the writer, full backfill resource
qualification and large-history migration remain separate open work.

Rewind restores cache fields only from journal observations whose number/hash
matches the surviving indexed blocks. Canonical journal rows and unreferenced
derived payloads rewind too, retaining the original all-table cold-replay oracle.
The first migration archives exact nullable legacy URI/document/display fields
before invalidating unproved caches; it does not assign them a new block or
hash. That archive is explicitly unproved. Its migration/retention cost needs
independent operator qualification before adoption. No actual operator database
has been migrated by authoring or testing this candidate.

A URI proof records a selected-hash RPC answer, and a payload hash records the
locally materialized bytes. Neither supplies independent NoM canonicality,
finality, media availability, an authenticated historical snapshot, first URI
or closure time, signed/native authority or owner/platform/release acceptance.
Missing historical inputs remain unknown. All six original canonical state
categories and all 42 original roadmap requirements remain mandatory.

The focused URI decoder, hash-call, history/rollback and actual-indexer controls
are selected in both the normal suite and the race lane. All previous race
selectors remain; indexer/nft is added to that lane. The RPC test call counter
is atomic. No browser, full-client, dependency, cache or output/time limit is
removed or relaxed. These source selections do not constitute executed tests.


The next backfill source fence separates the existing source read from its
transactional application. The read is bounded to its captured source height.
Before any handler or its RPC work, the writer rechecks a height/hash/empty
head identity and every distinct source block used by the ordered log input.
A same-height replacement, rewind, advance or missing/changed middle block
refuses the whole write. A concurrently completed job key skips stale replay.
This also covers existing-token ownership updates that do not make URI reads.
The source fence keeps only the preceding block identity while validating;
it adds no permanent epoch, per-history map or canonical-table exclusion.

The declared native regression source selects this boundary in the normal and
race lanes, using the production reader/writer and NFT handler, real loopback
RPC, all-table snapshots, and a successful cold-replay comparison. It includes
declared source-store faults under a stable terminal head, a completed-key
race, and the empty-index versus block-zero distinction. Authoring or offline
formatting does not establish that those native controls have executed.

URI RPCs still run inside the existing plugin write transaction, and the
backfill still loads its full selected log range. Moving reads outside the
writer must preserve the complete backfill's atomicity and cannot silently
drop unprepared history, mark a prefix complete or weaken the original table
oracle. Streaming/staging, resource and actual operator migration qualification
remain open; this source fence alone establishes none of them.


### Prepared URI writer boundary (candidate; native CI pending)

The indexer discovers each NFT URI and collection-name request before its main
write transaction. Discovery runs the actual NFT handler against separate
in-memory SQLite tables, loading only the source collection/token keys needed
by each event. Earlier mints remain available to later batch refreshes. The
original lexicographic 5000-token batch selection and complete backfill remain
the application boundary.

The main source snapshot closes before preparation issues any URI or plugin
collection-name call. Unrelated core token/code reads retain their existing
path. Responses are retained
in sequence, with explicit unknowns and content-addressed text deduplication.
The live writer checks its captured source head; the backfill writer retains
the exact head and complete source-block checks. Actual handlers consume only
matching prepared responses. Missing, changed, corrupt or unused input refuses
the complete transaction; no unprepared suffix is silently completed. An
already-indexed block or completed job makes no repeated provider calls.

Preparation has a read-back 64 MiB SQLite page limit. Selected backfill rows
also have 64 MiB logical field accounting and a 131072-row bound. These are
logical admission controls, not hard RSS guarantees. One source row is scanned
before its size admission. Exhaustion refuses the whole attempt and preserves
the incomplete job; support for histories exceeding these bounds remains open.
Local media resolution remains in the existing metadata admission path. Direct
plugin callers retain their old helper API and must provide prepared input to
use the indexer's network boundary.

The new candidate prepares actual-handler comparison, missing/corrupt/unused
response, cancellation, capacity, rollback-budget and batch-selection tests.
A real loopback provider probes the one-connection canonical DB from inside its
callback, for live indexing and backfill; a source-movement schedule must refuse
all target writes. The existing full-table recovery oracles are preserved. The
race selector includes the new PreparedURI tests as well as every prior selector.
These candidate tests have not yet executed. The isolated Python SQLite model
does not qualify Go/modernc, process RSS, full-history performance, an operator
database, independent chain finality, supported platforms or release readiness.


### Sparse URI range candidates (source candidate)

Prepared discovery and the live NFT batch handler share an exact decimal-text
prefilter. For proper subranges, canonical unsigned token IDs outside the
requested numeric interval stay inside SQLite. The previous lexicographic order, 5000-refresh boundary and
big.Int validation remain. Legacy spellings and non-text storage take the old
parser path; integer/REAL casts are not used. No schema/index migration is added.
Both range queries use the caller context, including the live query that
previously ignored cancellation.

The candidate compares actual direct and prepared handlers with the previous
full-scan selection for sparse, decimal-width, uint256, legacy, zero and reversed
ranges, plus cancellation in both phases. The sparse fixture requires three
driver candidates from 8192 canonical collection rows. These tests require
execution; authoring and offline formatting supply no native result. The existing
5002-row/shadow-mint boundary and all-table recovery controls remain selected.
This reduces returned canonical candidates; SQLite still scans the collection
index, and legacy candidates can still be numerous. Hard CPU/RSS bounds,
large-history throughput and real operator migration are not established.


### Signed EOA orders with the default full client (source candidate)

The additional candidate uses the existing public scalar-one test key to sign
two EIP-712 sell orders. Initial Book.Submit admission runs against the real
default-persistence client's current hash and the committed local derivation
head, after the priming CLI joins and before any fault. It performs actual
contract checks and retains the exact original inputs. No on-chain approval
supplies those orders. The production CLI later executes native forced buys,
indexing and book recovery; the parent never repairs or resubmits after a fault.

A cold reference is compared with a graceful client reopen and a declared
derivation SQL exit 86 followed by client reopen. The original unsigned oracle
remains strict; the separate signed-input branch requires exact serialized
inputs, genuine EOA recovery and all previous balance, ownership, order, chunk,
deferred and receipt checks. Every canonical table retains the original oracle.
The existing default-persistence test and its time/memory limits stay intact.
The signed controls reuse the same compiled test binary, run serially and have
their own four-minute timeout within the unchanged twenty-minute job limit.

These source controls have not executed. The new source must be compiled and
qualified independently. NoM observations are still synthetic and unsigned;
this supplies no NoM signature, canonicality/finality, HTTP/wallet integration,
signed-path engine-process crash, kernel kill or hardware power-loss proof.
The explicitly nonempty synthetic MessagePasser genesis and the separate shipped
empty-genesis refusal remain. Operator/platform/release acceptance and all 42
original requirements remain open where not independently established.


### Colocated signed-order process exit (source candidate)

The added case owns the actual full service and production CLI in the same
exact child. After a graceful priming child joins, initial EOA order admission
opens the persisted service in the parent through the existing Book.Submit
path, before any fault, and closes it again. The next owned child exits 86 at
the real derivation SQL boundary with engine head 7 and without service cleanup.
A new owned child recovers through the production startup/workers. The parent
removes only its declared exit hook, never repairs or resubmits an order after
the fault, and opens the service again only after recovery joins, for the
existing read-only signature/business/all-table oracle against cold replay.

The existing graceful and CLI-only signed cases remain. This source adds one
terminal case, three joined CLI children and five service opens: three in owned
children and two in the parent admission/read-only phases. The signed command
retains its four-minute timeout, reuses the same compiled binary, and preserves
the original unsigned command and twenty-minute job limit. Source authoring and
offline formatting do not establish execution. NoM inputs remain unsigned and
synthetic; kernel kill, hardware power loss, independent NoM authority/finality,
supported platforms, operator migration and release acceptance remain open.


### Signed index and book write boundaries (source candidate)

Two additional source cases use genuine EOA-signed initial Book.Submit inputs
with the real default full service and production CLI. The existing signed
derivation/graceful cases and original unsigned command remain. The new command
builds a fresh cold reference, then owns separate exact-child exits at the real
NFT index-plugin boundary and the book scanned-cursor write before commit.
The original owned-boundary checker requires a responding engine at head 7;
the index case additionally proves that the real NFT plugin completed inside
the uncommitted transaction. Exit 86 skips service cleanup. Initial admission
is before the fault; the parent only removes its declared exit hook, never
repairs or resubmits an order, and opens the service after recovery joins solely
for the original-signature/business/all-canonical-table comparison.

The same compiled binary is reused with a separate three-minute selector under
the unchanged twenty-minute job cap. The original seven-minute and four-minute
commands retain their own selectors and limits. The new command requires three
terminal pass events (root and two cases), eight joined CLI children, eleven
service opens (five parent and six child), three initial admissions and two
canonical comparisons if it executes. Source preparation alone does not prove
these events. Signed rewind schedules, independent NoM authority/finality,
standalone daemons, platform/operator/release acceptance, kernel kill and
hardware power loss remain open. All 42 original acceptance fields remain
authoritative; production remains NO_GO.


### Signed default-client rewinds (source candidate)

This separate source candidate admits two genuine EOA-signed orders through
the real Book.Submit checks at the committed common priming horizon, then
advances an owned full-service/CLI to the competing synthetic source branch at
momentum 9. Each case owns an exit 86 inside either the derivation rewind SQL
transaction or the index/book rewind transaction before commit. The existing
owned-boundary checker must observe the actual production reorg, responding
engine head 8 and independent RPC/direct hashes for common execution ancestor
6 (source momentum 7). Exiting skips service cleanup. Recovery makes the
replacement source frontier available at momentum 9 immediately, as required
by the existing production recovery path.

The parent performs initial admission only before the fault. It removes only
its declared exit hook afterward, never repairs canonical state or resubmits
an order, and reopens the service only after recovery children have joined for
the read-only original-signature/business/all-canonical-table oracle. The
existing observation comparator verifies hash-selected contract results and
independent RPC/direct headers before projecting only checked_block,
checked_hash and closed_block. Original input rows and every business field
remain exact; arbitrary table or signature differences are not projected.

One existing compiled binary serves a new separate four-minute selector under
the unchanged twenty-minute job cap. The original seven-minute unsigned,
four-minute signed recovery and three-minute signed write-boundary commands,
all selectors, dependency pins, cache/memory/parallelism settings and the
marketplace job are unchanged. If executed, the new command requires three
terminal passes (root and two cases), ten joined CLI children, thirteen default
service opens (five parent and eight child), three initial signed admissions
and two verified canonical-business comparisons. Combined full-client scopes
would contain 20 terminal events, 51 children and 63 service opens/observations.
These are required candidate outcomes, not executed evidence.

The parent revision has independently retained native/full-client success and
a failed first automatic browser workflow. Its navigation-deadline cause
remains NOT_ESTABLISHED; no prior green browser matrix qualifies this candidate.
Independent signed NoM authority/finality, HTTP/physical-wallet admission,
standalone daemons, native full-client races, larger histories, platform,
operator and release gates, kernel kill and hardware power loss remain open.
All 42 original acceptance fields remain authoritative; production is NO_GO.

## Metadata payload field validation

Prepared URI consumers must replay the discovered calls exactly once and in the
same order, without retries. A mismatch, omitted response or preparation error
invalidates the batch; callers must abort the canonical write.

The URI-history field matrix seeds canonical block/history references and matching
payload digests before testing one malformed field or envelope condition per case.
It covers version, trailing data, unknown fields, invalid UTF-8 in all four text
fields, URI/document/payload size, malformed document JSON and orphan display
fields. Each invalid case has an otherwise valid baseline. Exact size boundaries
and a legitimate Unicode replacement character remain accepted. Complete table
snapshots stay unchanged by validation. Raw UTF-8 is checked before JSON decoding
can replace invalid source bytes; malformed retained input cannot become a
different valid observation through that normalization.

These cases use local SQLite fixtures in the existing normal and selected race
commands. Their results do not establish operator migration, independent NoM
source authority/finality, supported platforms or complete N04 acceptance.

## Owned loopback RPC handler observations

The marketplace and credit fixtures observe only their own POST /rpc handler
lifetimes. The original request and ResponseWriter reach the original node API
unchanged. No body, header, URL query, address or other request identity is
retained. Tracking holds at most 32 active records and eight recent completions;
overflow remains explicit. Normal returns, abnormal exits and context cancellation
are observations, not HTTP status, body delivery or protocol request identity.
The bounds cover diagnostic records, not server concurrency or process RSS.

After a browser failure, the Node driver makes at most one capability-protected,
read-only fixture GET before closing the owned browser. It permits only the
declared loopback origin, refuses redirects, and limits the observation to 750 ms
and 32 KiB. Unavailable, invalid or incomplete diagnostics remain unavailable and
never replace the original failure. The Go parent separately records handler
progress after the browser process joins, before server cleanup. The two phases
help distinguish pre-cleanup state from cancellation during browser shutdown.
They cannot correlate an individual server observation with a CDP request ID.

The original 20-second navigation deadline, DOM lifecycle, zero-active dedicated
Network requirement, quiet interval, signing, receipt and balance oracles remain.
No request is completed by the observer and no RPC reply or provider transaction
is introduced by the read-only probe. Pure local controls, native concurrency
controls and actual hosted browser observations are separate evidence classes.
These diagnostics do not establish a cause for any historical failure, independent
NoM authority/finality or complete N04/production acceptance.

## Indexed decimal token ranges

The exact unsigned domain `[0, 2^256-1]` reads the existing collection/token
ordering directly. A separate scalar probe requires the existing decimal index
in the same statement; an absent or unusable required index refuses preparation,
including an empty collection. No outer SQL limit precedes the Go parser. Raw
oversized numeric IDs can cross the driver in this case, then fail the original
upper-bound check. Matching actions, lookahead, legacy spelling and block columns
retain the original scan/parser selection. This route uses 200 SQL bytes and two
parameters for token IDs and adds no schema or index.

Other unsigned 256-bit BatchMetadataUpdate bounds use an expression index on collection,
canonical decimal length and original token text. One ordered range seek per
intersected decimal width is merged with an ordered legacy stream. The maximum
is 78 canonical branches plus one legacy branch and 313 parameters. No numeric
integer/REAL cast changes token identity. The original lexicographic order,
Go big.Int parser, 5000-match action limit and 5001-key lookahead remain.
Legacy spellings, embedded NUL and non-text storage reach the original parser;
the number of nonmatching legacy records is not bounded by this optimization.
Signed, reversed and oversized internal bounds retain the original predicate.

The query explicitly requires the declared index. A missing or unusable index
refuses live refresh or discovery before canonical changes; it does not fall
back to an unqualified scan. The original independent scan/parser oracle remains
in the tests. Native controls require indexed searches without a temporary sort
for narrow, cross-width and full 256-bit ranges; they also cover dense misleading
text prefixes, legacy merging, the action boundary, cancellation, block columns,
idempotent schema adoption, read-only refusal and transactional rollback.
EXPLAIN output is inspected only by qualification tests on their selected SQLite
runtime, never parsed by production code.

The local standard-library SQLite model is separate from native Go execution.
Its sparse dense-prefix fixture reduced VM steps from 465192 to 236 while
preserving the selection. Its full-domain fixture increased VM steps from
170017 to 237947 because of the bounded merge work. These are fixture VM counts,
not latency, fixed RSS or a universal speedup. The added index consumes storage
and write work. A large operator database migration, its allocation/locking cost,
backup/rollback rehearsal and supported-platform acceptance remain unqualified;
no actual operator database is migrated by this source qualification.

The [expression-index rules](https://www.sqlite.org/expridx.html),
[query-plan diagnostics](https://www.sqlite.org/eqp.html) and
[required-index semantics](https://www.sqlite.org/lang_indexedby.html) describe
the SQLite interfaces used here. All original recovery, navigation, signature,
refund, receipt, balance and independent NoM/owner/release gates remain required.

The separate local SQLite 3.54.0 model retains eight selections and both column
shapes. Its 16402-row whole-domain fixture changes from 237947 to 40069 VM steps,
while raw driver candidates change from 5004 to 5005 for the same 5001 matches.
This is a local VM-count observation, not Go-driver timing, fixed RSS or a
universal speedup. The first EXISTS proposal required a temporary sort and was
rejected. First-execution and repeated counters are retained separately; their
difference has no established cause in this model. Other SQL and arguments are
unchanged. Four missing-index and four unusable-index controls refuse, including
empty collections. Actual Go plan, direct/prepared replay and the complete
current-source CI still require execution. Parent index migration/storage costs
and all independent operator, NoM and production gates remain open. The scalar
probe follows the [subquery expression rules](https://www.sqlite.org/lang_expr.html).
