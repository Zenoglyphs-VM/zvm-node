# Upstream predeploy source reconstruction

`rebuild-upstream-predeploys.cjs` executes five exact upstream solc WASM modules
against literal source bundles. It covers these eight embedded genesis targets:

| Group | Compiler | Source units | Targets |
| --- | --- | ---: | --- |
| `multicall` | 0.8.12 | 1 | Multicall3 |
| `op` | 0.8.15 | 16 | L1Block, L2ToL1MessagePasser |
| `permit2` | 0.8.17 | 14 | Permit2 |
| `entrypoint` | 0.8.23 | 18 | EntryPoint v0.7, SenderCreator v0.7 |
| `factory` | 0.5.8 | 1 | Arachnid CREATE2 deployer, Safe singleton factory |

The lock records every Git repository, full commit, source path, Git blob,
UTF-8 byte count and SHA-256. It includes the exact compiler input/settings,
complete compiler contract inventory, creation bytes, runtime templates, ABI,
metadata, storage layout and immutable reference maps. Compiler modules are
selected from `argotorg/solc-bin` commit
`c04c6cdc827bbdcc7bd65689fa76b63a238b223f`. The modern entries use their real
`bin/` files; their `wasm/` Git entries contain symlink text.

For acquisition, use the immutable paths in
[`upstream-predeploy-build.lock.json`](upstream-predeploy-build.lock.json).
Compare Git source content with its recorded blob and SHA-256. EntryPoint uses
OpenZeppelin **5.0.0** from its original tagged `yarn.lock`, including three
literal package members. The lock records that archive's exact SHA-512 integrity,
SHA-256 and byte count. Acquire and verify the archive separately before selecting
those members. This tool verifies selected source bytes against the reviewed
lock; it does not fetch repositories, authenticate Git commit membership or
recheck an archive it has not been given. Independent release source inventory
acceptance remains a separate requirement.

Prepare one source bundle per group using this shape:

```json
{
  "schema": 1,
  "group": "multicall",
  "sources": {
    "Multicall3.sol": { "content": "<complete exact UTF-8 source>" }
  }
}
```

Supply every `sources[].unit` listed for that group, without extra units. The
compiler unit for Multicall3 is `Multicall3.sol`; its original Git path is
`src/Multicall3.sol`. Preserving that distinction is required for full metadata
and runtime equality. Source contents are never normalized or flattened.
Permit2 resolves only its declared `solmate/=lib/solmate/` remapping. Solidity
imports must close over the complete selected literal inventory. No file,
network or import callback supplies missing content.

Run one compiler group per foreground process:

```sh
node --max-old-space-size=1536 contracts/rebuild-upstream-predeploys.cjs \
  --group multicall \
  --compiler ./inputs/soljson-v0.8.12+commit.f00d7308.js \
  --source-bundle ./inputs/multicall.sources.json \
  --core ../zvm-core
node --test contracts/upstream-predeploy-build.test.cjs
```

Apply a three-minute process deadline and 8 MiB stdout/1 MiB stderr caps. Input
bundles are bounded at 8 MiB, individual sources at 1 MiB, aggregate selected
source bytes at 4 MiB, and compiler modules at 16 MiB. Nothing is downloaded,
installed, signed or broadcast. The script writes only its stdout receipt and
sanitized refusal messages. Compiler code executes from the exact captured,
hash-verified bytes with the reviewed low-level API, without a CommonJS cache
entry or a second pre-execution path read. One process performs one compilation.

The default receipt contains digest and comparison results without local paths.
`--emit-bundle` additionally includes the complete Standard JSON compiler input
and output. Treat raw third-party source comments and diagnostics as private
review material before exporting that bundle.

Six targets require complete byte-for-byte runtime equality, including metadata.
Two targets have compiler-declared immutable ranges:

- Permit2's committed cached words describe chain ID 10 and its canonical
  address. The locked historical domain was independently derived using the
  source's EIP-712 formula. Current ZVM chain IDs require separate functional
  `DOMAIN_SEPARATOR()` checks and constructor witnesses.
- EntryPoint's immutable is its canonical SenderCreator, independently derived
  from `CREATE(canonicalEntryPoint, 1)`. A fresh-copy deployment creates a child
  at its own derived address and legitimately has different immutable bytes.

For these two, the tool checks every zero-template immutable range, every
committed word and every byte outside those exact ranges. It reports
`fullRuntimeEqual: false`. It does not patch the compiler template, strip
metadata, derive a new historical context during the run or execute constructors.
The legacy Yul compiler returns creation code only; the tool checks the exact
83-byte creation program and its fixed 14-byte return prefix, retaining the
69-byte runtime without inventing a Solidity ABI, layout or metadata.

This receipt is source reconstruction evidence. Actual constructor state,
internal creation, canonical deployment, current-genesis behavior, system calls,
block processing, all-twelve-predeploy signed build policy, hosted compiler runs,
supported platforms and independent operator/release acceptance retain their
own gates. The existing core verifier still refuses undeclared substitutions;
this tool does not change its complete-coverage requirements. All emitted
receipts retain `releaseAccepted: false`.
