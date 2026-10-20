# zvm-node guide

The ZVM's derivation node: it reads momentums from a Zenon node, turns each one
into exactly one EVM block, drives an execution client (**op-reth**) through the
Engine API, indexes the result into SQLite and serves it over a read API.

There is no sequencer here and nothing to elect. The order is whatever NoM
committed, so this node's only job is to rebuild the chain that order implies —
which means two nodes on the same NoM history produce the same chain, byte for
byte, without talking to each other.

```sh
go build ./...
go test ./...

zvm-node -config l2/rollup.json \
         -l1 http://127.0.0.1:45997 \
         -engine http://127.0.0.1:8551 -eth http://127.0.0.1:8545 \
         -db /data/zvm.db -listen :8080
```

`-h` lists the rest. `-no-index` derives and nothing else, for a node that only
needs to follow the chain.

## What it serves

| | |
|---|---|
| `GET /healthz` | 200 while keeping up, **503** once derivation has stalled |
| `GET /api/info` | chain parameters, fork status, derivation head, base fee |
| `/api/blocks`, `/api/txs`, `/api/address/…`, `/api/tokens/…` | the index |
| `/api/momentums/…`, `/api/nom/{hash}` | the NoM side of every block |
| `/api/contracts/…`, `POST /api/verify`, `/api` | source verification, in Etherscan's shape so `forge verify-contract` works |
| `/api/ops` | what this node costs to run: disk, memory, derivation rate |
| `/api/prices`, `/api/dex/pairs/…` | the ZVM's own Uniswap pools, their candles and trades, and what any of it is in dollars |
| `POST /rpc`, `POST /l1rpc` | proxied to the execution client and to NoM |

It also carries two things that only make sense next to a node:

| | |
|---|---|
| `zvm-node bench <setup\|run\|suite\|report\|storage>` | deploy standard contracts and drive load through both lanes; `storage` runs *inside* the node's container, which is the only place that can see its volumes |
| `zvm-node apps setup` / `apps trade` | deploy the exchange, collection factory and farm, and drive activity through them |
| `zvm-node media` | list, withhold or restore the NFT media this node stores |
| `zvm-node solcd` | run solc and nothing else, so a node doing contract verification never runs a compiler itself |
| `zvm-node dex` | seed Uniswap V2 pools that pair ZNN with the benchmark tokens |
| `zvm-node verify-tokens` | verify the deployed WQSR and WZNN, so a copycat token can never earn their checkmark |

The last two are the only ZVM commands outside [`zvm-core`](https://github.com/Zenoglyphs-VM/zvm-core)'s
`zvm` tool. They are here because both need the compiled Solidity in
`contracts/`, and a command that has to build half a contract tree verifies
nothing.

The whole-system benchmark is its own repository, [`zvm-fleet`](https://github.com/Zenoglyphs-VM/zvm-fleet) —
it drives every service at once and so depends on all of them.

`contracts/` is the Solidity: the bridge and fee contracts, the benchmark's
tokens and Uniswap V2 deployment, and the application contracts. It is one
Foundry/npm project because `verify/` matches submissions against it, and half
a source tree verifies nothing.

Derivation retries forever rather than exiting, which is deliberate — see
`operations/HALT.md` in zvm-docs. `/healthz` is what tells you it is stuck,
so put it in front of a load balancer rather than trusting the process to die.
`-rpc-deny debug_,txpool_,admin_` is worth setting on any node with a public
hostname: the execution client answers those namespaces on the same port as
`eth_*`, and a trace over a large block is a cheap way to keep it busy.

## The NFT market, and the seams it goes through

This node also serves the NFT market: `/api/nft/…`, the order book, and the
media store. That is not a convenience — it is forced. Who owns an item is
derived from the same `Transfer` logs the chain's own index reads, so an L1
reorg has to rewind both inside one transaction. A separate process indexing the
same logs would rewind on its own schedule and, between its commit and this
one's, answer "who owns this" for blocks that no longer exist.

What it is *not* is tangled into `indexer.go` and `api.go`. The market goes
through two seams, exactly as an out-of-tree extension would:

```go
// api.Extension — routes, a slice of /api/info, and an outermost wrapper.
type Extension interface {
	Register(mux *http.ServeMux)
	Info() map[string]any
	Wrap(http.Handler) http.Handler
}

// indexer.Plugin — extra tables, filled from the same logs in the same
// transaction, so a rewind is atomic across both.
type Plugin interface {
	Name() string
	Attach(h Host) error
	Schema() string
	Handle(ctx context.Context, dbtx *sql.Tx, r LogRow) error
	Jobs() []Job
	Deletes() []string
	Restores() []string
	AfterBlock(dbtx *sql.Tx, block uint64) error
	Rebuild(dbtx *sql.Tx, block uint64) error
	Stale(db *sql.DB) bool
}
```

`nodecmd.Run(args, extenders...)` is the whole node as a function. An
`Extender` puts its own flags on the node's flag set and is built once the
clients are up and before the index is opened, because what it returns decides
which tables that index has. This binary passes two: `nodecmd.Market` and
`nodecmd.Markets`.

The front end is [`zvm-nft`](https://github.com/Zenoglyphs-VM/zvm-nft), a separate static bundle that reads
from here. `-apps` points at a deployment written by `zvm-node apps setup`, and
is re-read while the node runs, so a market appears without a restart.

## Prices, and why the node asks

`markets/` is the second extension: the ZVM's own Uniswap pools as pairs,
candles and trades, with a dollar price laid over them. Dollars come from one
request a minute to DexScreener, which prices the bridged wZNN and wQSR in
Ethereum's pools; every other token is derived from this chain's own reserves,
through a pool at least `-min-liquidity-usd` deep — below that, anyone can set a
token's price for the cost of the pool.

The node makes that request, never the page. A browser asking a price API
directly would tell a third party who is looking at which token, and would be
refused outright as mixed content on a node served over plain http. `-prices=off`
turns it off entirely and the pools keep their prices in ZNN.

A token with no deep enough pool has **no** price, reported as absent rather
than as zero: a page that prints $0.00 for a token someone holds is stating
something false about their balance.

`-dex` points at a Uniswap V2 deployment file. It is the node's flag, not an
extension's — the read API reports it on `/api/info` and the markets extension
is handed the same path, because two flags naming one file is two answers to
which router is live.

The UI is likewise no longer compiled in. `-ui <dir>` serves any built static
bundle, with unknown paths falling through to `index.html` for client-side
routing — point it at [`zvm-explorer`](https://github.com/Zenoglyphs-VM/zvm-explorer)'s `dist`, or at
[`zvm-glyphswap`](https://github.com/Zenoglyphs-VM/zvm-glyphswap)'s.

## Layout

```
node/          the derivation loop, the Engine API driver, the stall detector
indexer/       blocks, transactions, transfers, tokens, holders → SQLite
indexer/nft/   the market's tables, as an indexer.Plugin
verify/        recompile a submitted source with solc, match it against the chain
api/           the read API above, and the extension seam
market/        /api/nft/…, as an api.Extension
markets/       /api/prices and /api/dex/…, as an api.Extension
prices/        the DexScreener poll, and deriving a price through this chain's pools
orderbook/     the book: storing D28 orders and re-checking them against the exchange
media/         content-addressed images and metadata, served immutable
apps/          `apps setup`: the exchange, the collection factory, the farm
nodecmd/       all of the above wired together, as a function anything can call
bench/         deploy standard contracts, drive load, measure, size the database
contracts/     the Solidity: bridge, fees, benchmark tokens, app contracts
```

Consensus lives in [`zvm-core`](https://github.com/Zenoglyphs-VM/zvm-core), which this module pins. Changing
how a momentum becomes a block is a change there, not here.
