# Market browser qualification

The opt-in `market-browser` CI job builds the two pinned production applications
with their own lockfiles and original shared Core gitlink. It uses preinstalled
Chrome and the `puppeteer-core` dependency already locked by Karum. There is no
browser download fallback or new frontend dependency.

`TestMarketBrowserHTTP` runs the actual Go Market.Register routes, NFT plugin,
SQLite index and shared order-book rewind transaction. Both real item pages
read the same running fixture. Nine checks per application cover item/listing
rendering, EIP-6963 discovery, EIP-1193 connect, account and network events,
disconnect, an orphan ownership transfer, unchecked rewind, a declared current
observation and a replacement approval revocation. Original stored order intent
must remain byte-for-byte unchanged.

The provider is an injected stand-in with no signing or sending capability.
Chain metadata, zero balances, optional royalty/name reads and stored exchange
check observations are declared transport controls. The fixture does not run
Book.Submit, an exchange RPC checker, a real wallet, a transaction, NoM or
op-reth. Explicit IndexBlock/Rewind calls are not automatic canonical selection.
The backend is the running Go implementation, not browser-intercepted API JSON.
Page requests are confined to the owned loopback origin. No arbitrary node URL
or faucet is accepted. The standalone Karum trade verifier is not invoked.

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
