# zvm-node

The ZVM's derivation node. It reads momentums from a Zenon node, turns each one
into exactly one EVM block, drives op-reth through the Engine API, indexes the
result into SQLite and serves it over a read API.

```sh
git clone --recurse-submodules https://github.com/Zenoglyphs-VM/zvm-node
go build ./...
go test ./...
docker build -t zvm-node .

zvm-node -config l2/rollup.json \
         -l1 http://127.0.0.1:45997 \
         -engine http://127.0.0.1:8551 -eth http://127.0.0.1:8545 \
         -db /data/zvm.db -listen :8080
```

| | |
|---|---|
| `GET /healthz` | 200 while keeping up, 503 once derivation has stalled |
| `GET /api/info` | chain parameters, fork status, derivation head |
| `/api/...` | blocks, transactions, tokens, momentums, contracts, prices, NFTs |
| `POST /rpc`, `POST /l1rpc` | proxied to op-reth and to NoM |

Derivation retries forever rather than exiting, so watch `/healthz`, not the
process. On a public hostname, set `-rpc-deny debug_,txpool_,admin_`.

Subcommands: `bench`, `apps`, `media`, `solcd`, `dex`, `verify-tokens`
(`-h` on each). Their endpoints default to `NOM_NODE`, `ZVM_RPC`, `ZVM_NODE`
and `ZVM_RELAYER`; `.env.example` has a local zvm-devnet's. No node is built
in. A whole local devnet is in
[zvm-fleet](https://github.com/Zenoglyphs-VM/zvm-fleet)'s `devnet/`.

Consensus lives in [zvm-core](https://github.com/Zenoglyphs-VM/zvm-core), a
submodule here.

MIT licence; GPL-3.0 files are listed in [NOTICE.md](NOTICE.md). The full
guide, with how it works and why: [docs/GUIDE.md](docs/GUIDE.md).
