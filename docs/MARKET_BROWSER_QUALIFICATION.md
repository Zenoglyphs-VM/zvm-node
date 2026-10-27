# Market browser qualification

The opt-in `market-browser` CI job builds the two pinned production applications
with their own lockfiles and original shared Core gitlink. It uses preinstalled
Chrome and the `puppeteer-core` dependency already locked by Karum. There is no
browser download fallback or new frontend dependency.

`TestMarketBrowserHTTP` runs the actual Go Market.Register routes, NFT plugin,
SQLite index and shared order-book rewind transaction. Both real item pages
read the same running fixture. The original nine checks per application cover item/listing
rendering, EIP-6963 discovery, EIP-1193 connect, account and network events,
disconnect, an orphan ownership transfer, unchecked rewind, a declared current
observation and a replacement approval revocation. Original stored order intent
must remain byte-for-byte unchanged.

Canonical collection-to-item navigation adds one check per application. Three
additional NFT checks cover legacy item/collection bookmarks and generated
explore/create links. Owner/account navigation and the legacy address alias add
one more NFT check: 14 checks for NFT and 11 for Karum. The network action
selector distinguishes the real switch control from the header's theme toggle.
The initial failed run is retained separately, including its NFT undefined
address request and the initially ambiguous Karum switch selection.

The provider is an injected stand-in with no signing or sending capability.
Chain metadata, zero balances, optional royalty/name reads and stored exchange
check observations are declared transport controls. The fixture does not run
Book.Submit, an exchange RPC checker, a real wallet, a transaction, NoM or
op-reth. Explicit IndexBlock/Rewind calls are not automatic canonical selection.
The backend is the running Go implementation, not browser-intercepted API JSON.
Page requests are confined to the owned loopback origin. No arbitrary node URL
or faucet is accepted. The standalone Karum trade verifier is not invoked.
The optional token-name and verified-name endpoints return explicit empty
metadata controls. Account, collection, item and order data come from the real
Go market. The external node explorer is linked, not rendered by this test.

The ordinary Go suite reports this test as skipped when its explicit browser
prerequisites are absent. In the browser job, missing prerequisites are errors;
the browser and both page paths must execute. Failures remain failures. Exact
application revisions and the actual browser/runtime versions are emitted in
the bounded qualification record. No screenshots, user data, credentials or
wallet material are published.

This adds evidence for page/provider and Go HTTP rewind integration. It does
not complete the original A08 acceptance: full signing/settlement and real RPC
recheck coupling, automatic canonical recovery, supported-product agreement and
full-width timestamp transport retain their separate requirements. Earlier
Go/TypeScript/Solidity vectors and contract results keep their own source pins.


A subsequent failed source revision retained an NFT owner-link failure, an
absent optional token-name fixture route and three 400 responses from Karum
collection side endpoints during navigation. Current pins include the NFT
account-route repair and Karum's captured-address/request-generation fence.
Both failed browser revisions keep their own results and are not relabelled.

The additional Karum check changes a valid collection route through real
browser history to an invalid address, requires an explicit settled error and
removal of the old item card, then returns to the valid item. It requires no
malformed API request, wallet signing or transaction. There are 25 declared
page/provider checks across both applications.

The 25-check revision retained passing main checks but a failed whole run due
to an item-page sibling read using an undefined address after navigation. The
next pin includes captured item keys, guarded sibling/royalty reads and early
block-listener registration with synchronous unmount cleanup. The complete
browser driver, its 25 checks and its zero-HTTP-failure requirement are
unchanged; a positive check count alone is not acceptance.
