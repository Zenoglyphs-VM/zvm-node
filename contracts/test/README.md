# Source-bound contract behavior

This lane depends on the reviewed source/archive/compiler helpers from the
upstream-predeploy build-tools Draft. It freshly compiles the original project
sources with the official Solidity 0.8.30 WASM module, optimizer 200 and Prague,
using OpenZeppelin 5.6.1. All 79 original application, benchmark and WZNN exports
must match complete ABI/creation/runtime bytes where the original export supplies
them. Metadata is retained. The three system sources and controlled fixtures are
also compiled freshly. System behavior is not an accepted release/genesis proof.

Supply `ZVM_TEST_COMPILER_FILE` and `ZVM_TEST_OPENZEPPELIN_TGZ` as absolute paths
to the files in `pins.mjs`, then run `npm ci --ignore-scripts && npm test` here.
Both files are checked by hash, size and identity before use. The dependency
archive is parsed in memory with the parent helpers; it is never extracted to a
package tree. There is no compiler wrapper installation. CI fetches only those
two bounded pinned files into its fresh runner temporary directory. Local
qualification can reuse retained read-only files without downloading or copying
them. The independent 39-location test lock leaves production dependencies alone.
One worker compiles the complete cohort once and then runs the behavior suites.

Tests use actual constructors, clones, pool creation, transfers, calls and revert
checkpoints. They cover bridge/QSR gross/net accounting and payout anomalies;
fee liabilities, pull payments and burn mirroring; collection metadata, royalty
and drop boundaries; exchange single/bulk EIP712, ERC1271 and on-chain orders,
NFT/ERC20/native settlement, cancellation and rejected-recipient credits; and
farm time/weight/budget accounting, measured token receipts, signed debt,
emergency exits and actual V2 protocol-fee harvesting. Benchmark contracts run
from freshly compiled source as well. Independent reserve, payment, liability
and rational entitlement models check the economic effects. Failed frames must
restore the complete state root and retain no logs.

Bridge/QSR use explicit synthetic genesis code at their fixed addresses, plus
exactly three initial bridge fee slots validated against the fresh storage
layout. Privileged SYSTEM calls assume authority in this fixture; they do not
establish NoM facts. There are no storage overrides after that declared setup.
Other contracts and V2 counterparts use actual constructors with no code aliases.
The VM uses Osaka while the project is compiled for Prague. A public scalar-one
account signs only offline typed-order fixtures; no wallet files or private user
keys are read. No RPC, NoM transaction, real funds or canonical transaction
signing is used. Canonical fees/inclusion, the accepted ZVM engines and release
bindings, browser/Go-engine integration, arbitrary economic configurations,
custody/governance and real deployment remain separate acceptance gates.

## Actual marketplace adapter lane

The same process also executes pinned NFT/Karum order adapters against fresh
Solidity. Install both `market-runtime/zvm-nft` and `market-runtime/zvm-karum`
locks with `npm ci --ignore-scripts --prefix` before running the suite.
See [MARKET_INTERFACES.md](MARKET_INTERFACES.md) for exact inputs, bounds,
coverage and remaining browser/indexer qualification.
