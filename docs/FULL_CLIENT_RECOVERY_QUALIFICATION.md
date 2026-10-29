# Full-client default-persistence recovery lane

The explicit `fullclient_recovery` lane uses the pinned op-geth service, its
catalyst Engine API, authenticated JWT HTTP listener and normal eth HTTP API.
It connects the actual production `nodecmd.Run`, Market and Markets extensions,
default 3-second polling and 30-second book recheck to those endpoints. The NoM
observation port remains synthetic; verification, public HTTP, external prices,
mainnet RPC and media are disabled.

Run the serialized lane separately from the ordinary and browser suites:

```sh
GOMAXPROCS=2 GOMEMLIMIT=5GiB go test -json -count=1 -p=2 -failfast \
  -tags=fullclient_recovery -timeout=5m \
  -run '^TestFullClientDefaultPersistenceRecovery$' ./orderbook
```

The service starts from `ethconfig.Defaults`: database cache 2048 MiB, clean trie
cache 614 MiB, dirty trie cache 1024 MiB, snapshot cache 409 MiB, buffered state
and its automatically selected path scheme. The suite refuses drift in these
pinned values. FullSync replaces peer synchronization; the data directory,
loopback ports, public test P2P identity and network isolation are fixture
overrides. No client cache is reduced. The Go memory setting is a soft GC target,
not a hard OS memory cap. Only one full service is alive at a time. Neither the
command-line geth binary nor the Karst/op-reth daemon is qualified by this lane.

The shipped genesis has empty MessagePasser storage. In this pinned client, the
payload encoder omits that empty withdrawals root, while the real newPayloadV4
endpoint requires it. A required negative control submits the unmodified
getPayloadV4 result, checks the exact INVALID refusal and unchanged canonical
head, and preserves this as an unresolved compatibility gate. It never repairs
the RPC payload and does not claim shipped-genesis client acceptance.

The positive storage cases declare a different synthetic genesis with one
nonzero MessagePasser storage slot and update and reload its genesis hash. This
keeps the real RPC encoding path usable without a production genesis change.
Compare a cold replay with two separate cases: graceful CLI/client close and
disk reopen; and an exact owned CLI SQL exit 86, followed by graceful client
close and disk reopen. Original child markers, race-exit refusal and joined
process checks are retained. The client directory identity is checked across
reopen. Actual production workers reconcile state; the parent reads the
canonical SQL tables, original unsigned orders, balances, token ownership,
chunks, deferred queues and executed roots using the existing oracle.

The canonical comparison excludes only existing observation-time, local epoch
and build-duration bookkeeping. Direct chain access is limited to that read-only
oracle. The service never exposes a custom Engine/eth adapter, changes payload
fields or invokes parent derivation/index/book repair.

These are declared-horizon, synthetic unsigned NoM observations and controlled
CLI process exits. The full client itself closes gracefully in the parent.
Kernel kill, daemon-process crash, hardware power loss, arbitrary scheduling,
signed NoM authority, real canonicality/finality, public chains, custodial
wallets, Karst/platform acceptance, maintainer activation and the complete N04
acceptance remain unqualified. Hosted results must be tied to the exact source;
source preparation and formatting alone do not establish runtime success.
