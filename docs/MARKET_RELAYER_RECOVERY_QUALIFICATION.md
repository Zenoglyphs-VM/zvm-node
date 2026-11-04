# Relayer recovery in marketplace fixtures

This source candidate binds the existing marketplace consumer to
`edgepillar/zvm-relayer` at
`f5d55cce233a90fa962837671b0371004ea56a12`, the separately reviewed bounded
admission and complete-history recovery candidate. The Relayer is compiled
without source changes and with explicit VCS stamping. The Core gitlink remains
`29e6d9b30be94de3772c850c7a1e9811585a8833`.

The executable's embedded revision and clean-tree flag must match before its
first process starts. Every start also requires the same file identity, size and
SHA-256. One compilation supplies all original profiles and the added reopens.
These are candidate-source checks, not an upstream release or signature claim.

The owned NoM adapter serves only the declared public devnet relayer account.
Frontiers and exact hashes select actual retained signed publications. Height
pages keep the full frontier height in `count`, including partial pages; the
requested range bounds the selected list. Unconfirmed pagination keeps its own
count and filters confirmation observations. Gaps, reordered blocks, changed
hashes, foreign accounts and unbounded selections are refused.

The execution adapter honors `requireCanonical` for block-hash state reads.
An orphaned header may be read explicitly without that requirement, while a
canonical nonce query for the same orphan must fail without a latest fallback.
The nonce is read from actual state at the selected executed header.

All original 43 marketplace profiles, 996 named financial checks and 52 owned
provider transactions/test NoM publications remain required. After the first
approval or seller cancellation has an actual application receipt, the profile
joins the first real Relayer process and starts the same binary on the same
endpoint. The new process must finish complete history recovery before health
becomes ready. Its status must retain zero queued/posted transactions and zero
raw bytes for the already executed first nonce. The adapter must observe a
nonempty account-history page and canonical-hash nonce lookup, with no duplicate
submission or publication, before the next wallet action proceeds.

Five partial-batch (including mixed payment) and four missing-allowance profiles
declare one reopen each. This requires nine additional process lifetimes, or
52 total across the
original 43 profiles. No additional wallet transaction or NoM publication is
introduced. Exact runtime counts and first-source execution must be checked in
fresh hosted logs; this document is not evidence that the candidate ran.

The default admission mode stays `volatile`. This profile recovers an already
published and executed account prefix; it does not establish queued-ACK durable
wallet recovery, production wallets, independent NoM authority/finality,
hardware power loss, deployed operator policies or broader platform acceptance.
Optional `local-sync` retains its separate evidence and remaining wallet gate.
All original acceptance requirements remain active and production is **NO_GO**.

Separate queued-ACK `local-sync` variants are described in
[Queued wallet ACK recovery](MARKET_DURABLE_WALLET_RECOVERY.md).
They require their own fresh current-source execution and preserve all original
default-volatile profiles and financial predicates.
