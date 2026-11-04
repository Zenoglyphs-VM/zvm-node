# Connected production Node and OP client qualification

This bounded CI-only cohort executes the actual `cmd/zvm-node` production binary
with its index and loopback health API enabled, against a complete official
OP-reth client. Price fetching and compilation are explicitly disabled. The
program receives declared synthetic NoM RPC inputs; it does not contact a real
NoM node, authenticate a chain or operate a funded wallet.

The immutable Core dependency is
`acf39556a3f465a6885f23fc3d0cc8065891d76c`. Its connected-client helpers provide
the unchanged independent non-EVM state model, OP process lifecycle, genesis,
account, receipt and sandbox predicates. This new driver does not construct or
import the positive blocks: only the production Node does that. The observer
reads the Node database with `mode=ro` and `query_only(1)` and makes no repairs.

The original four required default profiles are a supported Isthmus variant and the
three shipped Karst profiles (`l2`, `l2-69`, `l2-dp`). Each must derive eight
connected blocks containing the exact source-planned L1 information and
synthetic ZNN mint deposits. Every block must match the independent complete
state root, fourteen account nonce/balance/code/storage-root observations, both
receipts, the persisted Node record and both indexed transactions. The original
source genesis accounts, fork inputs, gas limits and runtime bytes remain.

At block four, both the production Node and OP client must stop and join, then
resume the same persistent head, complete state, read-only Node record, index
and health response. Each changed-root payload must be refused without moving
the canonical head; unauthenticated Engine access must be refused. Reconstructing
a stored canonical header for this negative input is separate from the Node's
positive execution. The pinned SDK's encoder behavior is visible in its
[immutable source](https://github.com/ethereum-optimism/op-geth/blob/e8800cffe53d459cde8a07c8e8f1de9d86e79e07/beacon/engine/types.go).

The OCI lock preserves Linux/amd64 OP-reth v2.6.1, its platform/configuration/
layer identities, and the observed executable SHA-256 and length. This is not
independent binary-build provenance. The production Node's complete executable
digest and embedded clean source revision are also checked. Runtime uses a
non-root owner, no external network, a read-only root, two CPUs, 4 GiB memory,
256 PIDs and bounded diagnostics/deadlines/data. All actual exits and complete
stdout digests must remain in the first-job evidence. No image or binary is
downloaded or executed locally by the contribution controller.

The full seven-job source workflow remains required. The added job is a new
dependency of the existing final named gate. Its controls retain every original
7,203 result combination and test 50,421 combinations with the additional job.
Passing this cohort cannot qualify a failed marketplace job or establish its
historical cause. Full signed-user/economic/default-scale/concurrent/fault
coverage, real NoM/finality, other platforms, release/genesis authority and
maintainer/operator/governance/custody acceptance remain separate gates.


The same driver, production Node executable and single container also run four
explicit `source-constructors-v1` genesis profiles. The production Node remains
the only positive payload builder/importer. Another 32 connected blocks, 64
exact planned transactions, 32 complete roots, 32 read-only Node records,
32 indexed blocks and 64 indexed transactions must agree, including four joint
Node/client restarts and eight additional joined production CLI lifetimes.
The original models, NoM fixture, lifecycle, SQL observer, image, limits and
all seven workflow jobs remain required.

The constructor checkpoint source is copied byte-for-byte from the qualified
Core revision. Its genesis hash/root pairs must match that same Core recipe.
At genesis, the persisted height-four joint restart and height eight, explicit
canonical block hashes bind 48 account observations, twelve EntryPoint status
reads and 36 domain/deposit/nonce getters. Eight requests at deterministic
unknown canonical hashes must return errors while the accepted head stays
unchanged. Sampled combined data/free-space records retain the unchanged
512 MiB cap, 10 GiB floor and scan-instability handling. They do not assert an
OS-enforced aggregate quota or a transient peak bound. No historical/local
data, real chain configuration or operator database is deleted or migrated.

These bounded synthetic NoM/mint profiles do not establish independent NoM
authority, signed user-operation or full economic/wallet flows, accepted epoch
or release signatures, other platforms, governance/custody acceptance or a
production release. Every original acceptance requirement remains open to its
own authoritative evidence.
