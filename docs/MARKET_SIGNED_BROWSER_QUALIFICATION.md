# Production browser signatures and executed market recovery

The opt-in `TestExecutedMarketBrowserAdmissionAndRecovery` lane runs the pinned
NFT and Karum production bundles through their actual item and account-page
listing controls. Both applications create two single listings and a separate
two-item bulk listing. The real order module and its locked Viem transport send
EIP-712 requests to an injected EIP-1193 provider backed by a public test signer.
The provider uses that application's own locked Viem installation; no new SDK or
browser dependency is added.

Each case starts with an empty book. Actual browser HTTP requests reach the
production node API, `Market.Register`, `Book.Submit` and block-hash
Multicall3/exchange checks in the existing pinned op-geth execution fixture.
The real node info, optional-name/verification and RPC handlers run together.
Only optional USD pricing is explicitly disabled. Market and RPC responses are
never fabricated or intercepted by the browser test.

The source fixture accepts the two actual admitted browser inputs only at its
joined, pre-fill horizon. It compares every already-derived source block's
complete serialized bytes before replacing unobserved future fill calldata.
No accepted order is re-signed or rewritten. A fresh source clock accommodates
the production pages' current listing-time allowance; their wall clocks, salts,
prices, signatures and proofs are not replaced with precomputed test orders.
The local derived bulk-root cache field is excluded from wire equality because
it is absent from the browser request and Solidity input ABI. Address text is compared as its exact twenty-byte value across JSON letter-case normalization; order quantities, signature bytes and proof entries remain exact.

Production derivation, canonical indexing and book loops execute the submitted
signatures' fills, recover after a quiet replacement branch and restore the
original branch. The real browser renders both items at each phase. Original
input bytes, checked canonical hashes, ownership, listings and sale history must
agree through HTTP and executed state. Each of four application/signature cases
has fourteen named browser checks; these are distinct from Go terminal events.
Completed successful checker calls, unexpected RPC errors and joined-context
cancellations remain separately reported.

Both bundles are built once per browser job. Package tests and cases are
serialized so only one Chrome instance and one small execution fixture are
active at a time. Every child, browser and production worker is joined; each
case has a ninety-second deadline and bounded input/output. The ordinary Go
suite explicitly skips this browser prerequisite; CI runs it in the dedicated
job using the preinstalled browser.

This is real browser signing/admission and executed local consistency under
declared fixture authority. The public signer is not a physical wallet. NoM
observations are unsigned, Engine/eth endpoints are custom loopback adapters
and fills still use native forced-transaction transport. Ethereum-wallet
transaction broadcasting, an unmodified daemon, independent-source verification
and public-chain settlement remain unqualified. Cancellation/approval-revocation
integration, complete uint64 timestamp transport and final supported-product
agreement remain separate original acceptance requirements.
