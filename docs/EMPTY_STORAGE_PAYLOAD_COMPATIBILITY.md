# Empty MessagePasser payload compatibility

The Node targets the Isthmus Engine API. Its pinned op-geth encoder omits
`withdrawalsRoot` when that header value is the empty withdrawals hash. The
unmodified client rejects that omission at `engine_newPayloadV4`. The original
raw-client refusal remains an explicit control.

The derivation boundary handles only a missing root with the current chain's
Isthmus context. It tries the one empty-root value on a copy and reconstructs the
complete block with the original advertised hash, beacon root, transactions and
the same empty versioned-hash and execution-request arguments used by the Engine
client. A full block-hash mismatch or invalid material is refused. An already
present root and pre-Isthmus data keep their original path. No RPC response,
genesis allocation, chain identifier or dependency file is rewritten.

The native controls must cover missing context, absent/nonempty withdrawals,
unknown hidden roots, changed commitments, malformed fields, original-input
preservation and concurrent shared input. Their separate race run uses no service
or network fixture. The additional full-client profile must use the same test
binary and production CLI with the original empty MessagePasser allocation:
cold replay, graceful reopen and a joined derivation SQL write exit followed by
client/CLI reopen must match the complete original read-only state oracle.
The actual genesis header and account storage roots and deployed code are checked
before each case. The fixture does not fill or replace catalyst replies.

Every existing seeded recovery, signed order, rewind, whole-client SIGKILL,
compiler/application and marketplace oracle remains required. The new profile
does not qualify empty-storage coverage at every existing fault boundary, general
raw Engine API compatibility, other clients, hardware power loss, independent
NoM authority, a real accepted release/genesis or operator deployment.

Primary contracts:

- [Pinned encoder and complete block-hash conversion](https://github.com/ethereum-optimism/op-geth/blob/e8800cffe53d459cde8a07c8e8f1de9d86e79e07/beacon/engine/types.go)
- [Isthmus header and Engine API rules](https://specs.optimism.io/protocol/isthmus/exec-engine.html)

The omitted-root path also requires both V4 blob gas header fields to be present. RLP can encode a missing optional integer like an explicit zero when later fields are present, so the block hash alone cannot prove field presence. The Engine API still validates the submitted payload and execution result. Both missing-field cases have separate required unit and race controls.
