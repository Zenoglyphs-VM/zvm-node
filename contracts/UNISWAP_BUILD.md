# Reproduce the committed Uniswap artifacts

`rebuild-uniswap.cjs` reproduces the committed Factory, Pair and Router02 artifacts
from exact npm-lock source archives and official compiler modules. It checks all
12 core and 27 periphery compiler targets against `uniswap-build.lock.json`.
The three committed files receive an additional full comparison: ABI, interface
and bytecode aliases, creation/runtime bytes, opcode listings, source maps and
immutable/library reference maps. Factory and Pair also contain standalone
compiler metadata strings, which must match exactly. Router's file omits that
string; its full bytecode, including embedded CBOR metadata, still must match.

No metadata is removed, no immutable is filled from an observed address, and no
package JavaScript, installation or lifecycle script is run. The tool performs
no downloads and writes no extracted source tree. Supply previously verified
regular files using an owned, bounded acquisition workflow.

## Compiler modules

Use the exact official `soljson` WASM modules listed by the
[Solidity distribution manifest](https://binaries.soliditylang.org/wasm/list.json).

| Group | Version | Bytes | SHA-256 |
| --- | --- | ---: | --- |
| `core` | `0.5.16+commit.9c3226ce` | 7,625,538 | `a09c9cc2672678d461dc71100600bb58802db87be4de9424769241712ccbec03` |
| `periphery` | `0.6.6+commit.6c089d02` | 8,442,418 | `09f6098026622c5c334c7798c3ad2b8f7c0ebc62f87846c7d5e7e725c3d1cbc2` |

The official `solc` npm archive can also transport its literal `package/soljson.js`
member. Regardless of transport, the selected module must match the table above.
An archive's observed checksum alone does not replace that independent module
pin. The reported compiler version must include the matching `.Emscripten.clang`
suffix.

The loader compiles the captured, hash-verified module bytes without a second
file read or a CommonJS cache hit. The exact modules require only the Node `fs`
and `path` builtins. Solc 0.5.16 uses the two-argument compile API and has no
reset API; 0.6.6 uses three arguments and exposes reset. Each process permits
exactly one compiler call, and the original module path is checked again before
accepting the result. Compiler execution requires an independently qualified
Node runtime and a foreground supervisor; these checks do not create a sandbox.

## Source groups

The complete source package-lock hash and exact archive byte count, SHA-256,
SHA-512 integrity, package name and version are fixed. Both core versions below
are intentional and cannot be exchanged through npm hoisting:

| Group | Required archive flag | Package |
| --- | --- | --- |
| `core` | `--core-archive` | `@uniswap/v2-core@1.0.1` |
| `periphery` | `--periphery-archive` | `@uniswap/v2-periphery@1.1.0-beta.0` |
| `periphery` | `--periphery-core-archive` | nested `@uniswap/v2-core@1.0.0` |
| `periphery` | `--lib-archive` | `@uniswap/lib@1.1.1` |

The reader validates each complete USTAR archive, including unselected members,
and selects only literal Solidity sources and package identity. Only an explicit
empty `package` root directory is accepted as a root member. Links, extensions,
aliases, traversal, duplicate paths, nonzero padding and missing end markers are
refused. The literal source closure retains the original unit names and test,
example, interface and library targets that affect source/AST identifiers.
The optimizer is enabled with 999,999 runs and `evmVersion: istanbul`; libraries
and remappings are empty. A separate locked input digest binds those settings
and every literal source byte. The compiler receives no import callback.

```sh
node --max-old-space-size=1536 contracts/rebuild-uniswap.cjs \
  --group core --compiler "$SOLC_0516" --core-archive "$UNISWAP_CORE_101"

node --max-old-space-size=1536 contracts/rebuild-uniswap.cjs \
  --group periphery --compiler "$SOLC_066" \
  --periphery-archive "$UNISWAP_PERIPHERY" \
  --periphery-core-archive "$UNISWAP_CORE_100" --lib-archive "$UNISWAP_LIB"

node --test --test-reporter=tap \
  contracts/build-support.test.cjs contracts/uniswap-build.test.cjs
```

Supervise each compiler process with a two-minute deadline, an 8 MiB stdout cap
and a 1 MiB stderr cap. The selected archives have aggregate caps of 16 MiB
compressed, 64 MiB expanded and 4,096 members; each archive may expand to at
most 32 MiB. Each source is limited to 1 MiB, with 16 MiB and 1,024 units for
the complete closure. Input/output JSON is bounded and rejects duplicate keys,
unsafe numbers, invalid UTF-8, excessive nesting and trailing documents.

The default receipt reports fresh compiler execution, all target material
digests and the three explicit committed-file comparisons. `--emit-bundle`
instead returns the complete `{schema, compilerVersion, input, output}` document
for the selected group. Its compact `input` bytes must be preserved if another
verifier binds `inputSha256`. Receipts and bundles contain executable runtime
templates; they do not execute constructors or observe deployed runtime.

## Remaining acceptance

Uniswap requires separate actual Factory/Router deployment, token approvals,
`addLiquidity`, factory-created Pair and initialized pool-state witnesses. Direct
Pair deployment does not substitute for that recipe. Exact immutable maps and
zero-filled templates do not establish constructor substitutions or initialized
storage. Signed release/build integration must retain complete contract coverage
and all twelve genesis predeploys. Ten other predeploy reconstructions, hosted
compiler execution, supported-platform lanes and independent contract/release/
operator acceptance remain open. Compilation does not establish full N03
completion, live-chain integration or production readiness.
