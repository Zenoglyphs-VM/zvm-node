# Signed production market browser qualification

The `market-browser` CI job exercises the actual built NFT and Karum pages,
their own locked Viem clients, the real node API and exchange-backed book,
the official relayer program and the pinned op-geth execution engine. The
sixteen serialized profiles cover each application's single and bulk listing
flow, followed separately by a purchase, one-order cancellation, exchange nonce
invalidation, or collection permission removal through its actual controls.

## Exact inputs

| Subject | Source |
| --- | --- |
| NFT production application | `1eda1f3334f59f8138cf980cbba1fb225247f17b` |
| Karum production application | `2d64eac12733505e9d7bfe948057b37e788c2807` |
| Official relayer program | `5a72a223dd60d1ae4e974d420cadd85e6f9ee929` |
| Shared Core | `29e6d9b30be94de3772c850c7a1e9811585a8833` |
| Application Viem | `2.56.8`, separately installed from each unchanged lock |
| Execution dependency | `op-geth v1.101702.2`, from the node's Go lock |

The existing browser pin file binds the complete frontend revisions. The
workflow checks the relayer and Core revisions, builds that unmodified program
once, and reuses its executable for all sixteen profiles. Each profile records
the executable digest and actual Go version. Each browser records its actual
Node, Chrome and SDK versions. No browser download fallback is allowed.

## Executed flow

1. A fresh loader-accepted rollup executes the shipped collection, factory,
   exchange and fee deployment. Its book starts empty. The real node API
   supplies info, market, name/verification reads and the `/rpc` proxy.
   Only optional USD pricing is disabled.
2. The actual item listing controls or account bulk-selection controls use
   the page's wall clock, salt and own locked SDK to request EIP-712 signatures
   from the public scalar-one fixture seller. Two single prompts/POSTs or one
   bulk prompt/POST admit two orders through real hash-bound exchange checks.
   Exact order, signature, proof and kind bytes are retained.
3. Accepted inputs bind only future source calldata. The already derived
   source heights one through six stay byte-identical. Three actual ZNN
   deposit transactions drain through the unchanged mandatory gas budget across
   source heights seven through nine, then fund the public scalar-two buyer. The executed
   state, including their consumed account nonces, determines its next nonce.
4. The actual item's Buy now button invokes the application's stateful gas
   estimate and base-fee calculation. The pinned op-geth gas estimator runs
   against executed state with the real value and calldata. The EIP-1193
   fixture provider signs the page's exact EIP-1559 request with the public
   buyer key and submits its bytes through the real node `/rpc` handler.
5. The declared execution RPC adapter forwards raw submission to the actual
   relayer HTTP endpoint. The official program checks admission, batches the
   original signed transaction, signs a NoM account block with Core's public
   devnet relayer account, and publishes it to the loopback NoM source.
   The fixture verifies that account signature, source context and exact
   decoded signed transaction bytes. No native buy is substituted.
6. The application must display its actual pending step while the canonical
   receipt is absent. Only future source height ten receives the published
   account block; heights one through nine remain byte-identical. Production
   derivation, op-geth building/validation, canonical indexing and book workers
   execute it. The application must consume the real successful receipt before
   showing success. Gas usage, sender, target and consumed nonce are checked.
7. A replacement source branch excludes that transaction. Its canonical receipt
   disappears, its nonce returns to the executed funding-deposit value, both
   items belong to the seller and both listings reopen without an orphan sale.
   Restoring the original source restores the exact transaction/receipt block
   hash, consumed nonce, ownership, one sale and one remaining listing.
   Both original accepted order inputs survive these changes byte-for-byte.

Item fillability is measured at the current item's primary price-card action.
Karum's independently purchasable sibling-card buttons cannot satisfy that
assertion. Backend owner/sale/order oracles remain required for both tokens
at every stable phase, independently of what the page renders.

## Ownership, resources and interpretation

Each profile owns and joins one Chrome process and one actual relayer child.
The Go command uses `-p=1`; subcases do not run in parallel. The two production
bundles and the relayer executable are built once per job. Each fixture retains
the bounded 16 MiB engine-cache settings. The browser child has a 90-second
owned context, 16 KiB stdin, 1 MiB stdout and 128 KiB stderr caps. The relayer
child has bounded output and an explicit joined shutdown. The unmodified
program returns exit 1 with `zvm-relayer relayer: context canceled` after its
owned SIGTERM cancellation; the test records that actual exit and requires the
exact terminal marker, no WARN/ERROR output, and a joined process. An early exit
or a deadline is refused. All endpoints and
signer transports are loopback; external browser requests, page exceptions and
failed HTTP responses refuse qualification.

Passing requires all sixteen profiles, every named browser check, the independent
Go state/input/receipt oracles and successful child joins at the new exact node
revision. An older run or an unchanged file tree does not qualify that revision.
The separate backend admission/recovery tests retain the original native-lane
scenario and their own source and authority boundaries.

The Engine and eth RPC adapter, synthetic NoM canonicality and declared plasma
allocation remain fixture authority. This lane does not qualify the unmodified
full execution daemon, independently authenticated NoM consensus/finality,
physical wallets, public-chain settlement or production operation. The selected owner-action matrix below extends cancellation, nonce and approval
coverage. The separate full-width timestamp codec/client lane is specified in
[MARKET_TIME_WIRE.md](MARKET_TIME_WIRE.md). Remaining batch/refund/currency/royalty/
ERC-1271 browser paths and final supported-product agreement remain open.


## Actual owner invalidation controls

The original four buyer cases retain their source horizon and gas policy.
Twelve owner cases additionally exercise single-order cancellation, exchange
order-nonce invalidation and collection permission removal, for single and bulk
inputs in each application. Each fresh owner profile funds both public fixture
actors with three actual deposits each, then drains the unchanged production
mandatory queue through source height twelve. The actor's account nonce must
match the canonical genesis nonce plus all executed deposits/native calls from
that actor; the balance is read from executed state. The extra horizon is not
a relaxed gas budget or a fabricated balance/nonce. Source heights one through
twelve remain byte-identical; height thirteen receives the real relayer block.

Each owner action uses one real signed EIP-1559 transaction and one verified
signed NoM publication through the unchanged official relayer. Cancellations
must void the selected order while retaining its unaffected sibling, including
a sibling using the same bulk root signature. Exchange nonce invalidation must
void both orders while preserving the distinction between the exchange's order
nonce and the actor's Ethereum account nonce. Permission removal must leave
owners and signatures intact while the real collection approval and exchange
check prevent both listings from being offered. Direct hash-bound views check
approval, exchange nonce, cancellation mappings and exact checkOrder code and
remaining amounts independently of worker readiness and cached HTTP results.
There must be no sale, owner transfer, SQL order/cache control, manual index
step, replacement input or substitute native transaction.

Every owner transaction is removed by a canonical replacement and restored
again. The actual receipt, account nonce, direct contract state, book/HTTP
views and original input bytes must agree at every stable phase. Permission
removal is also observed in the actual account component: it reads the changed
approval after the receipt, restores permission after the orphan, and removes
it after restoration without navigating or reloading the component. Its
bounded four-at-a-time reads, five-second visible-page refresh, disposed timer and stale-result fences
are separate from the preserved RPC, worker and browser ownership controls.

Both applications expose permission removal only for the matching connected
account and node network. A guard is checked before estimation and again after
asynchronous preparation before asking the wallet to send. Three small
standard-Node controls in each frontend independently exercise immediate
context refusal, a change during an awaited estimate, and unguarded legacy
receipt waiting. Their synthetic capabilities are not browser or settlement
proof; the sixteen actual browser/relayer profiles qualify that separate path.
Existing SDK locks and Go/Solidity golden inputs remain unchanged. The newly
pinned order modules add exact numeric/string timestamp transport. Their
separate real-SDK/Go/Solidity time-wire lane does not turn earlier complete
financial adapter campaigns into fresh executions. Remaining batch/refund/
currency/royalty/ERC-1271 browser paths, independent NoM/full-daemon
qualification and final supported-product agreement remain open.


## Batch fills and native refunds

The same opt-in production-browser lane also runs full and partial two-item
checkouts for both applications and both single/bulk listing signatures. This
adds eight profiles to the existing sixteen owner/buyer profiles. The four SDK
full-width-time profiles remain separate from browser execution.

A full batch sends one exact provider `buyMany` transaction. The partial profile
holds the buyer's already prepared EIP-1193 request before signing or publication.
An independent production seller page cancels the first listing through its own
real signed transaction and the unmodified relayer. Once the seller page consumes
that executed receipt, the original buyer request is released with both original
inputs and the full 0.5 ZNN value. The exchange must buy only the second item and
return the unused 0.25 ZNN. No cancellation, fill, refund, order row or checker
reply is substituted by the controller.

The relayer signs two successive NoM account blocks in the partial case. The
source adapter verifies each signature, account height, previous hash, exact EVM
bytes and acknowledged source frontier. It binds cancellation at source height
13, then the buyer at 14, preserving prefixes through 12 and 13 respectively.
Height 15 is quiet. The adapter's NoM authority remains synthetic.

Independent hash-bound contract views, executed native balances and receipt fees
check filled/cancelled mappings, owners, remaining amounts, direct refund, zero
exchange credits and zero retained exchange native balance. This isolated EOA
profile admits no operator fee; L1 receipt fees are included when present. HTTP
orders, item listings and sales must agree. The application must display the
actual count and unused amount; Karum must keep the skipped cart entry. Internal
accounting receipt reads do not count as application receipt observations.

The replacement branch excludes both signed actions. Both receipts and account
nonce effects must disappear, the original balances and two listings must return,
and restoration must recover the exact original receipts, native accounting and
HTTP model. Original API signatures and proofs remain unchanged. Existing tests
continue to build each pinned frontend and the official relayer once per job and
serialize profiles. Passing these fixture profiles does not establish mixed
currencies, nonzero royalties/fees, ERC-1271, rejecting refund receivers, the full
market acceptance matrix or production readiness.


## Credit interface read regression

The retained 24 serialized page/provider/relayer profiles now begin on the real
seller account page. Each profile waits for the new exchange-credit component
to read an exact zero balance from the executed fixture RPC and verifies that
its withdrawal action is disabled. One additional named assertion per profile
brings the retained matrix to 520 named checks. All original signing, admission,
receipt, full/partial native refund, cancellation, nonce, approval and canonical
replacement/restoration assertions remain required. The original transaction
and NoM publication counts stay unchanged.

This extension exercises a zero-credit page read. Positive credit withdrawal
with accepting/rejecting recipients uses the separate integration lane below.
The read-only legacy
navigation fixture keeps its metadata-only RPC limits and is not used to
establish EVM credit behavior. Full A08 acceptance remains incomplete.

## Positive credit and contract-account recovery

`TestExecutedMarketCreditBrowserRecovery` adds one serialized profile per
interface while retaining the 24 existing profiles and every original check.
The job compiles one self-contained `CreditFixtureWallet` using the same
byte-count-, digest- and version-pinned Solidity 0.8.30 WASM compiler as the
retained financial lane. The existing compiler helper is reused unchanged.
This adds no npm dependency graph, OpenZeppelin archive, browser installation or
local Go/compiler execution. The compiler download is streamed once with its
exact 9,591,828-byte cap and SHA-256; output is bounded to 64 KiB in fresh job
scratch. Each frontend and the official relayer still build once per job. The
15-minute job and 12-minute serialized test bounds remain required.

The declared source setup deploys an owner-controlled receiver, admits two
public-test-key orders through the actual HTTP API, and executes a native
0.25 ZNN sale with a 200-basis-point exchange fee. The rejecting fee recipient
leaves exactly 0.005 ZNN of backed exchange credit. Source setup is explicitly
separate from the browser withdrawal and is never replaced by direct SQL,
contract-storage insertion or a fabricated RPC answer. The actual stateful gas
estimator observes receiver rejection before wallet dispatch. An executed owner
call enables receipt; a separate explicit wallet rejection must leave no
submitted transaction or uncertain recovery record.

The real production account page then requests `withdrawCredits()` from the
credited contract account. Its declared test provider signs an ordinary owner
EOA transaction calling the compiled wallet's `execute(exchange, call)` method.
The original journal must exist before the provider request and its returned
outer hash must be durable before receipt waiting. The unchanged official
relayer publishes those exact signed EVM bytes in a verified NoM account block.
Reload while pending must restore the same hash and prevent a second request.
Actual canonical execution, recipient/exchange/owner balances and receipt fees
are checked independently. Source replacement removes only the withdrawal,
restoring the backed credit and pending record; another reload must retain the
same hash. Returning to the original branch must recover the same receipt,
balances and immutable journal without re-signing. Runtime bytes, owner and
receiver mode are checked against the source-bound compiled artifact.

This known contract-account adapter does not qualify generic Safe/ERC-4337
user-operation mapping or physical wallets. The initial two profiles cover
stateful receiver rejection before dispatch. The additional mined-revert
profiles below retain the same withdrawal and recovery controls. NoM
canonicality/plasma and the custom Engine/eth adapter remain fixture
authority. The source sale uses native currency, zero royalties and the declared
nonzero fee; it is not mixed-currency or ERC-1271 browser acceptance. Full A08,
independent full-daemon/NoM authority and production acceptance remain open.

## Mined withdrawal failure after successful preparation

One additional profile per interface schedules the test receiver to reject ZNN
from L2 block 16. This is an owner-authorized call in the declared source setup,
before any withdrawal request. The actual direct and outer-call gas estimates
run at L2 block 15 and succeed. The same signed outer transaction is then
included at block 16, where its payout reverts. No already-derived source byte,
contract storage, receipt status, caller or checker response is rewritten to
produce the failure.

The canonical failed receipt must retain gas headroom, consume the owner nonce
and charge its actual receipt fee while leaving 0.005 ZNN backed credit and
zero recipient balance. The real page must show a reverted record with the same
original hash and available credit. A new preparation against the rejecting
receiver must fail before wallet dispatch and leave the existing journal exact.
The stateful estimator retains the actual `PayoutFailed()` revert data, so a
different RPC failure cannot stand in for receiver rejection.

Removing the failed transaction must restore its owner nonce/fee effects and
make the original journal pending; reload must prevent a duplicate request.
Returning to the original branch must restore the same failed receipt, fee,
backed credit and immutable journal without re-signing. The original positive
profiles retain 13 checks each; the two mined-revert profiles require 14 each.
Together with the original 24 profiles/520 checks, this is 28 browser profiles
and 574 named checks. Four actual SDK/time clients and the separate navigation
checks remain required. One relayer binary and one compiled receiver target
serve the entire serialized lane within the existing bounds.

This extension qualifies one declared block-dependent receiver policy with a
known owner-EOA account adapter. Unknown outer-hash recovery, other wallet/user-
operation mappings, physical wallets, independent NoM authority and the rest of
the original A08 acceptance remain open.


## Caller-id wallet credit recovery

The serialized lane retains all 24 signed production-interface profiles and the
four successful/mined-revert credit profiles. Legacy fixture wallets now answer
scoped capability discovery with an unsupported response, before continuing their
unchanged `eth_sendTransaction` path. The workflow and browser pins select the
new frontend revisions that passed 179 tests, typecheck and production builds.

Two additional real-browser profiles exercise Wallet Call API response loss, one
per interface. The declared fixture wallet executes from a compiled owner-EOA
account adapter; it records exact caller ids before dispatch and retains their
association with one actual signed outer transaction across page reloads. The
application loses the accepted send response, then recovers only by the saved id.
The wallet returns actual node receipt data for that exact associated hash. It
never searches by nonce, replaces a signature or submits a duplicate transaction.

A real stateful PayoutFailed estimate and an explicit wallet rejection are
retained as offchain wallet assertions with their original request ids. Neither
signs or publishes a transaction. The original accepted transaction is submitted
through the unchanged official relayer with actual NoM signature verification.
Canonical execution checks credit backing, recipient/exchange balances, owner
nonce and receipt fees, gas headroom, exact call bytes and the compiled runtime.
Reload, canonical removal and restoration preserve the same caller id and all
known hashes. A wallet that temporarily returns an incomplete receipt group leaves
the original request unresolved; canonical restoration resolves it without a new
send. The legacy withdrawal journal is never modified by the new protocol.

The new driver records 16 named checks per interface. These assertions qualify
only after their exact-source hosted executions succeed. One relayer binary and
one existing pinned compiler/wallet target serve all 30 serialized browser
profiles within the unchanged 15-minute job and 12-minute test bounds. There is
no added dependency graph, archive or browser installation.

Wallet id association and inner outcome remain wallet assertions, separately
checked against canonical outer receipts and actual exchange credit. This
adapter is not generic Safe/ERC-4337 or physical-wallet qualification. NoM
canonicality/plasma remain synthetic, and the custom Engine/eth fixture remains
explicit. Independent NoM/full-daemon authority, already-lost legacy outer
hashes, other wallet mappings and full marketplace acceptance remain open.


### Direct wallet rejection without retained status

The two existing Wallet Call API profiles also exercise a direct numeric `4001`
send rejection whose identifier has no queryable wallet batch status. The
production page must retain the exact request with its bounded method/code
annotation, display it as the wallet's rejection assertion, survive a reload and
refresh without a new dispatch, and permit a new request only after another
explicit action. The original rejected row remains unchanged through the later
accepted request, response loss, canonical removal and restoration.

The receiver-estimate refusal remains a separate cached offchain status. Only the
third request signs and submits one actual outer transaction per profile. This
adds one named check per existing profile, with no additional browser profile,
relayer compilation, wallet-contract compilation or transaction. The target is
30 production browser profiles, 608 named checks and 34 actual provider
transactions with verified NoM publications. These are expectations until the
automatic workflow succeeds at the newly pinned Node and frontend revisions;
earlier runs retain their original source identities.

Provider rejection is an offchain assertion under EIP-1193 and EIP-5792, not
independent execution proof. The companion model controls cover conflicting
pending/receipt evidence, ambiguous errors and persistence failure. Generic
Safe/ERC-4337 accounts, physical wallets, independent NoM/full-daemon acceptance
and the full original marketplace acceptance remain separate gates.


### Exact quote preparation on a production page

The existing Karum single-order purchase profile holds two actual approval-read
transport requests without changing their parameters or replies. During the first
read the declared provider emits a standard accountsChanged event. The real
application must refuse the old listing before any typed-data request or order
POST. Restoring the public test account and making a fresh explicit action then
tests a one-hour listing: while its approval read is held, the actual price input
changes from 0.25 to 0.75 and its duration from one hour to one day. The real
typed-data request and admitted order must still contain the original 0.25 ZNN
and exactly 3600 seconds, independently of the later form values.

The remaining purchase, nonce/cancellation/approval, batch/refund, credit and
wallet-call cases are retained at the new UI pins. This adds three named checks
to one existing profile and no browser profile or transaction. The expected
campaign is 30 production browser profiles, 611 named checks and 34 actual
provider transactions with verified NoM publications. The existing one-time
frontend/relayer/wallet builds and job/test bounds are unchanged. These are
expectations until automatic CI qualifies the exact new source revisions.

The provider and accounts are the declared public fixture. EVM execution, API
admission, official-relayer transport and signatures remain actual; NoM
canonicality/plasma and the custom Engine adapter retain fixture authority.
Pure source-function quote controls separately cover both interfaces, QSR
configuration, full-precision relisting, offers, bulk selection and context
changes. Actual mixed-currency, nonzero-royalty and ERC-1271 browser acceptance
and the full original roadmap remain separate gates. Earlier financial
campaigns retain their original order-module/UI/source revisions.


### Nonzero native royalties and protocol fees

The existing twelve buy, full-batch and partial-batch profiles use a 375-bps
collection royalty and a 200-bps exchange fee. Seller, buyer, royalty receiver
and fee receiver are four distinct public fixture addresses. The constructor
and royalty setter execute in the declared source prefix; balances are never
inserted into EVM state, SQL or market responses.

At each canonical phase, independent hash-selected contract and balance reads
check exact gross spend, seller proceeds, both payouts, receipt-derived gas
costs and zero deferred credit. The actual SDK separately reads configuration,
fill events and balances, including the partial-batch refund and source
removal/restoration. These add three named assertions per purchase profile
while retaining all original profiles, transaction counts, build inputs and
CI bounds. Exact-head hosted execution is required before claiming this scope.
Mixed-currency and ERC-1271 production-interface execution remain separate
open requirements; this native-currency lane does not establish them.


## Additional QSR and mixed cart profiles

The existing thirty native profiles and their 647 named checks stay in the lane. Two separately named single-signature profiles add an NFT item-page QSR purchase and a Karum cart containing one ZNN and one QSR listing. The simple NFT sweep retains its native-only selection.

QSR uses the existing Core genesis predeploy and actual bridge derivation. A finite allowance is executed in the declared source prefix; these cases do not qualify the missing-allowance approval/purchase path. Native and QSR balances, rounded royalty/fee shares, native receipt gas and the remaining QSR allowance are read independently through canonical removal and restoration. QSR prices include a non-divisible base-unit amount. No balance storage insertion or product API/RPC response replacement is used.

The candidate retains the existing workflow limits, dependency graphs, compiler/wallet target and relayer build. Exact-source automatic CI output must establish the current totals. Historical runs keep their original revisions. Mixed partial refunds, token bulk signatures, royalty cap boundaries, ERC-1271 production-page paths, general wallets and independent NoM/full-daemon acceptance remain separate open gates.

### Mixed-currency partial refund profile

`karum-single-mixed-partial-batch` retains a prepared original buyer request
containing one 0.25 ZNN order and one 20,000,001-base-unit QSR order. A separate
production seller page cancels the native order before that buyer request is
signed and published. The original buyer request then executes only the QSR
fill. The exact native balance delta must prove the entire 0.25 ZNN returned,
with zero exchange credit and only the independently observed buyer gas cost.
The seller separately pays its cancellation gas. Actual fill logs and both
currencies' balances independently verify the 375-bps royalty, 200-bps fee,
remaining finite QSR allowance and unchanged order bytes through canonical
removal and restoration of both original transactions and NoM publications.

This adds one separately named profile and preserves every previous native and
token profile. It uses the existing QSR predeploy and the declared executed
bridge deposit and finite allowance prefix. Missing-allowance approval, generic
wallets, ERC-1271, independent NoM authority and full-daemon execution remain
separate requirements. Source assertions and parser checks alone do not establish
hosted execution success; historical CI remains bound to its original revision.

### Missing-allowance approval and original purchase profiles

`nft-single-qsr-missing-allowance` and
`karum-single-mixed-missing-allowance` start with exactly zero QSR allowance.
The actual item/cart controls prepare a finite approval for 20,000,001 base
units and the original purchase. The first approval executes through the same
official relayer and source derivation as the purchase. Only after the actual
page consumes its canonical approval receipt may its second wallet request
reach the declared public fixture signer; that request is briefly held before
signing/publication so independent allowance, Approval-log and nonce assertions
can run. No product API/RPC reply or gas estimate is replaced.

Both original buyer transactions have consecutive actual nonces and separate
signature-verified NoM account blocks. Independent Go and SDK oracles account
for each receipt's gas, exact native and QSR payouts and the remaining finite
allowance. Canonical removal must remove both receipts/nonces, allowance and
balance effects; restoration must recover the original transaction bytes,
hashes, receipts and account blocks. All previous profiles remain. These cases
do not establish general wallet, ERC-1271, independent NoM/full-daemon or
production acceptance. Current-source hosted execution must qualify the new
profiles; historical CI retains its original revision.

### QSR bulk root and finite approval profiles

`nft-bulk-qsr-missing-allowance` and
`karum-bulk-qsr-missing-allowance` select two items in the actual account
page, select QSR, and list each for exactly 20,000,001 base units with one
OrderRoot prompt and one original order POST. Independent SDK and Go checks
bind both admitted currency/price leaves, their sibling proofs and the same
original root signature. The NFT account bulk form captures its selected
currency and reward-token configuration with the quoted price and duration.
A changed reward-token mapping prevents a late signature or POST.

The NFT item purchase approves exactly 20,000,001 units for its one fill;
the unaffected sibling remains open. The Karum cart approves exactly
40,000,002 units and fills both original leaves. Neither profile inserts an
allowance into state or uses an unlimited approval. Each requires the actual
approval receipt before the original purchase request, two consecutive buyer
nonces, two actual relayer publications and exact native gas/QSR payout
accounting. Replacement and restoration retain the original signed bytes,
receipts, finite allowance and all order/proof inputs.

All previous native, token and mixed-currency refund profiles remain. These
additional cases require fresh exact-head automatic CI; source preparation
does not establish hosted success. General wallets, ERC-1271, royalty cap
boundaries, independent NoM/full-daemon acceptance and the complete original
roadmap remain separate requirements. Workflow, engine and estimator limits
and dependency versions remain unchanged.

### Per-item sale proceeds estimates

Karum reads each selected token's royalty for the complete quoted price at one
canonical block, sharing that block's exchange fee, receiver and royalty-cap
reads. The estimate uses separate integer floors and the actual royalty cap;
a zero receiver disables the corresponding deduction. The display shows the
complete currency precision and a range when selected items have different
proceeds. Missing or failed reads remain unavailable. Changed inputs, selection,
endpoint or a closed dialog cannot receive a late older estimate.

The reader bounds active royalty calls at four and debounces form edits by
200 ms. Refreshes occur only while the dialog is open. All existing production
browser cases remain; every Karum listing additionally compares the actual
display with independent SDK calls at its selected canonical hash. These are
estimates of current settlement inputs, not a promise about future sale terms.
Above-cap or reverting external royalty contracts and ERC-1271 execution still
require their separate native acceptance evidence. No deadline, gas ceiling,
dependency graph or original transaction count is increased.

### Bounded immutable observation reuse

The independent proceeds observer reuses only the exchange fee, recipient,
royalty cap and block number at an identical block hash within the same fixture.
The cache holds at most two snapshots, joins concurrent reads and drops failed
observations. Eviction cannot reinsert an older pending result or let an old
failure remove a newer entry. Every selected token's royalty at its complete
price is still read independently. Every observation still rechecks its block
number's current canonical hash, including after a cache hit.

No latest header, balance, allowance, owner, nonce, receipt or royalty is cached.
No product RPC reply is replaced. All 37 browser profiles, 845 named checks and
46 original provider transactions remain. Ten dependency-free cache controls
cover failed reads, eviction and delayed completion. Workflow deadlines,
dependencies and engine/estimator bounds are unchanged. Exact fresh CI timings
and actual eliminated-call counts are required before claiming a speedup.
