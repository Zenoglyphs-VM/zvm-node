# Market HTTP behavior across an index rewind

The A08 checks in `indexer/market_http_reorg_test.go` exercise the actual NFT
plugin, SQLite index, order-book epoch invalidation, shared rewind transaction,
`market.New().Register` routes and HTTP JSON encoder. They reuse the existing
loopback geth block/receipt fixture and recovery failure adapters.

These are the same listing, item and order endpoints consumed by both NFT and
Karum clients. The cases cover a replaced owner, token-level approvals,
operator grants/revocations and unrelated owners. They check projections before
the orphan, immediately after rewind, after a declared current check and after
indexing the replacement block. The original stored order identity and payload
must survive. Restore/rebuild/commit refusal cases require all table state and
the corresponding HTTP responses to stay unchanged.

Open check observations are deliberately seeded controls. A calibrated CodeOK
row does not establish a signature or exchange approval; it lets the test verify
that NFT projections independently apply indexed ownership and permissions.
Immediately after rewind, the order endpoint and default listing must refuse
the unchecked observation. Tests do not call `Book.Submit`, run the poller or
claim that a real RPC `checkOrder` revalidation occurred.

The normal CI suite emits Go JSON events once for the whole source tree. A
second selected run applies the race detector only to the new HTTP cases.
Build/vet and all previous regression cases remain required. Local authoring
uses the retained formatter; Go compilation and test execution occur in the
automatic fork workflow.

This qualifies a native Go HTTP/rewind boundary over declared chain history.
It does not qualify a browser, wallet provider, real NoM/op-reth process,
automatic production fork selection, live exchange settlement or deployment.
Those remain separate from the earlier literal Go/TS/Solidity adapter cases and
from full original A08 acceptance. Both interfaces still need actual page and
provider integration evidence and an agreed supported-product boundary.
