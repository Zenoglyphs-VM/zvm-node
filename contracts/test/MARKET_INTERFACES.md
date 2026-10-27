# Marketplace adapter qualification

The suite executes the real `zvm-nft` and `zvm-karum` order modules selected by
`market-inputs.lock.json`. The exact published revisions, file lengths and
SHA-256 digests are mandatory. Acquisition has a 128 KiB per-file cap and a
384 KiB aggregate cap; it fetches five text files, not full frontend checkouts.

Each frontend keeps its own exact SDK dependency closure. Both lock viem
2.56.8, but their resolved transitive graphs differ. The small runtime projects
retain those versions, integrity values and nested resolutions directly from
the corresponding frontend lock. The test verifies both graphs and installed
versions. It does not substitute the contract harness's viem 2.57.1 runtime.
The npm cache is shared for transport; the two installed module trees remain
independent.

The UI modules run in VM module contexts with their actual SDKs. The provider
uses only the existing public scalar-one offline signing fixture. Contract
wallet responses are synthetic; the deployed ERC-1271 fixture validates the
actual requested digest. Calls encoded by the real adapters execute against
fresh Solidity 0.8.30 output in the existing EthereumJS world. Compilation is
shared with the original contract suite within the same process.

The checks cover:

- Go golden order hashes, EIP-712 digests, odd bulk roots and proofs compared
  with both actual UI modules and actual Solidity execution.
- Native and ERC20 purchases, exact seller/royalty/fee amounts, aggregate
  shortfall rollback, cancelled sweep entries and exact excess refunds.
- Single/bulk signatures, ERC-1271 digest binding, individual and nonce-wide
  cancellation, exact/any-token bids and wrapped-native funding.
- On-chain approval, backed credits after rejected native payouts, and credit
  reads/withdrawal through the adapter's exchange ABI.
- Ownership and approval changes followed by actual EVM checkpoint/revert,
  re-reading the signed input after every state change.

The read-only golden-domain fixture relocates freshly constructed exchange
runtime to the fixed address and chain from the Go vectors. Its EIP-712 cache
must recompute for the new address. That case proves hashes/digests; it does
not prove constructor storage, sales or deployment provenance at that address.
All settlement tests use normally deployed fresh contracts.

The API/provider bridge is synthetic and the offline VM does not charge real
transaction gas. Checkpoint/revert controls do not qualify the Node indexer's
canonical reorg handling, a browser, a wallet provider, public-chain execution
or production deployment. Both interfaces still need those integration gates
and maintainer agreement on the supported product boundary.

The original A08 criterion remains intact: simple/full UI ownership is explicit;
Go/TS/Solidity order vectors agree; bulk fills/refunds, royalties, ERC-1271,
cancellations and reorged ownership/approvals pass across both supported
interfaces. These adapter tests are one part of that criterion.

CI installs the original EVM test lock and both small SDK runtime locks, obtains
the bounded compiler and interface inputs, then runs `npm test` in this
directory on Node 22 and 24. Existing artifact comparison and contract controls
remain mandatory. For local qualification, supply retained verified inputs in
`ZVM_MARKET_INPUT_ROOT` with the manifest layout, alongside the existing
compiler/dependency environment variables; local tests do not download them.
