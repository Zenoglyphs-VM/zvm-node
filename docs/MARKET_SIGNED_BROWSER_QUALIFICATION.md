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
| NFT production application | `4f0c9cc1c663baecc9aee2c13aba2d9f51622056` |
| Karum production application | `e30edefb2d0a0dd2626182c658c78411603e0480` |
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
coverage. Remaining batch/refund/currency/royalty/ERC-1271 browser paths,
full-width uint64 timestamps and final supported-product agreement remain open.


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
Existing SDK locks, complete order modules and Go/Solidity vector inputs remain
unchanged. Full-width timestamps, all remaining batch/refund/currency/royalty/
ERC-1271 browser paths, independent NoM/full-daemon qualification and final
supported-product agreement remain open.
