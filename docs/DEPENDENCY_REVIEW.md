# Dependency and distribution review

CI action implementations use full commit pins with readable release-version
comments. Weekly Dependabot proposals cover Actions, the root Go module and the
contracts npm lock, with at most two open ordinary version-update pull requests
per ecosystem. Each update needs source review and qualification before
acceptance. Scheduled updater execution and a complete advisory or license
review require their own evidence.

## Resolve the effective inputs

For Actions, resolve releases in their official repositories. Review the
old-to-new implementation, action metadata, bundled dependencies, permissions,
credential handling, network destinations and artifact behavior. Keep the full
commit pin and version comment together. A tag match alone does not review the
implementation.

For Go, review `go.mod` and `go.sum` with the actual toolchain and platform.
The `github.com/ethereum/go-ethereum` requirement is replaced by
`github.com/ethereum-optimism/op-geth`; inventory the replacement that is built.
The core requirement is replaced by `./zvm-core`, whose source comes from the
exact Git submodule commit and configured URL in [`.gitmodules`](../.gitmodules).
A module-update proposal does not independently select or qualify a new core
gitlink. Review the accepted core source and node compatibility together, and
verify that the recorded commit is fetchable through its configured URL.

For `contracts/package.json` and its lock, retain exact resolved versions,
archive URLs/integrity and install-script decisions for OpenZeppelin and Uniswap
inputs. A package range is not the installed version. Map imported Solidity to
the effective archive members and compiler settings. Include copied benchmark
artifacts and application/predeploy exports in the source/output comparison;
updating a dependency lock alone does not refresh or qualify saved bytecode.
Record actual bounded build, vet, relevant behavior tests and integrity checks.
Preserve failed, skipped and unknown results explicitly.

Triage advisories against exact resolved packages or replacements, advisory
revision and affected versions. Identify the call site or build/test step and
whether it reaches a shipped output. Preserve unknown reachability rather than
inferring safety from a scanner result. Report sensitive findings through the
repository's private security channel until coordinated disclosure.

## Bind review to the shipped output

Start with [`NOTICE.md`](../NOTICE.md) and [`LICENSE`](../LICENSE). The notice
table records separately licensed wrapped-token and Uniswap benchmark material;
the repository's own license does not describe every dependency or output.
Retain upstream license/notice text, source revisions and archive/member
digests, compiler/build settings and exact output digests. Include linked,
copied and transitive material that actually reaches a binary, container,
benchmark or contract export. Independent distribution/license review and
corresponding source obligations remain separate acceptance work.

An SBOM and provenance statement must identify the particular built output and
its resolved inputs. Review the compiler binaries fetched by the compiler
service as separate execution inputs. The `ubuntu-latest` runner, package repository contents, transitive APK inputs
and downloaded Go toolchain need independent origin and content review; full
action and selected image pins do not qualify every execution input. Container/platform checks,
output-bound notices, release signing and real operator/chain acceptance need
their own qualification.

Keep public records in English and omit private paths, personal identity,
credentials, wallet material and raw private diagnostics. Record exact source
and run identities, actual command outcomes and material evidence limits in
sanitized summaries. Successful CI does not approve a deployment or release.

References: [GitHub Actions secure use](https://docs.github.com/en/actions/reference/security/secure-use)
and [Dependabot options](https://docs.github.com/en/code-security/reference/supply-chain-security/dependabot-options-reference).

## Exact Node and Core source profile

The selected CI actions are checkout 7.0.1, setup-go 7.0.0 and upload-artifact
7.0.2 at full commit pins. The checkout source was also inspected for the
submodule profile: submodule credentials are not persisted, temporary global
authentication is removed, and unsafe PR checkout remains disabled. Hosted execution must qualify this actual profile at each selected source;
another repository's action success does not qualify Node. Enabled Go caches include both Node and Core lock files.

`tools/dependency-review/source_closure.py` verifies the exact selected Node
commit, every tracked regular-file blob and mode, and the one allowed Core
Gitlink. It independently checks Core commit
`29e6d9b30be94de3772c850c7a1e9811585a8833`, its configured public URL, source tree,
lock hashes and regular files. Other Gitlinks, links, changed indices,
untracked/ignored inputs or local authentication config require review. Config
values and private checkout paths are not printed. This profile preserves the
existing Core source; it does not adopt the separately proposed Core dependency
or Engine-authentication changes.

## Node SDK, locks and native qualification

The root module and ordinary CI select Go 1.26.11. `GOTOOLCHAIN=local`,
`GOWORK=off`, `GOFLAGS=-mod=readonly` and disabled telemetry prevent automatic
SDK selection, workspace replacement and incidental build-time lock changes.
The SDK environment, checksum verification, tidy diff, build, vet and complete
normal/race suites are recorded by `native_qualification.py`. Test result caching
is disabled for both suites. The receipt preserves actual exits, stream EOF and
caps, failed/skipped test identities, and package outcomes. Counts include parent
tests and subtests, rather than independent scenarios. A 900-second aggregate
command budget and 8 MiB/256 KiB per-command stdout/stderr caps bound evidence.
Both source closures must match again after qualification. Failure and incomplete
outputs remain distinct from success. The workflow retains an explicit ZIP for
fourteen days, including failure receipts when available.

The locks were produced in the exact-source temporary preparation at commit
`9ce62c6def67b0d7ac9ab377500b06b6ff3fe35e`, automatic fork run
[`43409369521`](https://github.com/edgepillar/zvm-node/actions/runs/43409369521),
artifact `14029235859`, ZIP SHA-256
`c80794a1ff78751fb06efb6e4ca55e7455c94da7c1945c7364a63676fdbac6de`.
That bounded producer selected brotli 1.2.7, websocket 1.5.3, compress 1.20.2,
OpenTelemetry root/metric/trace 1.44.0, x/crypto 0.59.0 and x/text 0.44.0. The
actual resolved graph and original module/replacement identities are checked
before adoption. The temporary workflow, recipe and preparation helper are
removed after adoption; their Git revision and original receipts remain
historical evidence. Producing or inspecting locks does not qualify their native
compatibility; a subsequent exact-head run is required.

The Core Gitlink remains `29e6d9b30be94de3772c850c7a1e9811585a8833` at its canonical
upstream URL. The effective op-geth library is v1.101702.3. Its reviewed networking
selection uses STUN v3.1.5 and DTLS v3.1.4. The unchanged Core source still has its
historical module declarations; Node's effective dependency and import graphs
must be observed at the root. Complete Node root-module tests, including the
simulated backend, remain selected. The separately proposed Core source scan
result does not qualify this Node/Core combination. A fresh exact-head scan and
graph, upstream advisory mapping and distribution/license review remain required;
no advisory waiver or test exclusion is introduced.

## Container input observations

The Docker frontend, Go 1.26.11 Alpine builder and Alpine 3.22.8 runtime use
reviewed manifest-index digests. The builder checks its exact SDK and verifies
module checksums before the existing static CLI build. Direct runtime selections
are ca-certificates 20261020-r0, wget 1.25.0-r1 and gcompat 1.1.0-r4; the latter
preserves the existing compiler-service compatibility dependency. Their primary
package metadata is available from Alpine's
[ca-certificates](https://pkgs.alpinelinux.org/package/v3.22/main/x86_64/ca-certificates),
[wget](https://pkgs.alpinelinux.org/package/v3.22/main/x86_64/wget) and
[gcompat](https://pkgs.alpinelinux.org/package/v3.22/main/x86_64/gcompat) records.
These pins do not freeze every transitive package or establish image provenance.

The separate hosted container job builds a local image and exercises only
`zvm-node --help`, expecting exit 2. It runs without network access, with a
read-only root filesystem, UID/GID 65534, dropped capabilities and no new
privileges. `container_inventory.py` records the immutable local image ID,
platform, installed package versions and CLI digest with bounded command
receipts. This is a build/input observation, not a complete SBOM, container
vulnerability scan, compiler-service isolation test, signed output or release.
The existing image entrypoint and operator user policy remain unchanged. No
service, live chain, compiler download, image publication or deployment is
performed by this usage control.

## Node source and test vulnerability review

The read-permission `Dependency review` workflow builds govulncheck v1.9.0 with
Go 1.27.4 outside the application module, then selects Go 1.26.11 for source
analysis. It scans `./...` with Node root-module tests and CGO disabled. Imported
Core production packages remain included; Core's separate nested-module test
suite is outside that pattern. The complete Node/Core source profile runs before
and after observations. A 600-second scanner deadline and 8 MiB/256 KiB stream
caps preserve actual exits, EOF, failures and incomplete results.

`local_core.py` binds the actual projected Core module selection to the reviewed
commit, tree, canonical URL and both Core lock hashes. The only local replacement
profile is the declared `./zvm-core` with an empty effective version and nominal
`github.com/Zenoglyphs-VM/zvm-core v0.0.0`. Other local paths, aliases, changed
Core source, changed replacement versions and missing inventory records require
review. Local Core SBOM occurrences and findings stay in the report. The profile
adds source identity; it does not waive a finding or establish advisory coverage.
The producer's source claims still require independent Git-object comparison
when consuming its artifact.

The scanner consumer requires its expected protocol, SDK, database identity,
module inventory and advisory references. It distinguishes module, package and
source-call findings, keeps raw occurrence counts, and omits source locations
from its summary. Exit 1 means findings require review; exit 2 means failure or
incomplete evidence. A zero result means only that no matches were reported by
that particular scan. Database origin, replacement-fork advisory coverage,
exercised reachability, runtime safety and release acceptance remain separate.

After any executed scan attempt, including one reporting findings, a separate
metadata producer records closed CLI and Node test import graphs. It uses a
fixed Go template containing public package and nominal/effective module IDs,
not filesystem/cache paths. Package/edge bounds, test-variant identities,
acyclic closure, the exact local Core profile and before/after source/lock pins
are checked. Metadata commands each have a 120-second deadline; package output
is capped at 4 MiB and module archives are not downloaded in this graph step.
Repeated import IDs retain occurrence counts. Import presence is not exercised
function or exploit reachability, and no waiver is generated. Failed graph
observations remain failures even if a source scan completed.

This attempt's raw source scan, module-binding observations, execution receipts,
summary and import graphs are retained in one explicit ZIP for fourteen days,
including failures when available. Current qualification requires inspection of
that exact head, run/attempt, archive digest and complete streams. Existing Core
results and earlier Node native/container results are historical evidence for
their original source; they are not fresh qualification of this workflow or its
findings.

## Upstream advisory aliases for the replacement fork

The effective op-geth namespace can have an empty advisory query while upstream
Geth records still require assessment. The committed fact projection in
`tools/dependency-review/upstream-advisory-snapshot.json` preserves six complete
selected queries: upstream/effective module records, the effective source commit,
both repositories' public security channels, and the legacy Geth JSON feed.
Provider response hashes and individual record hashes bind the captured facts;
descriptions, credit names and personal data are omitted. This is a dated
snapshot, not a live feed or authenticated independent origin.

After graph collection, `upstream_advisories.py` replays the original profile,
local Core selection and closed Go import graph from complete, pinned streams.
The graph source must match the independently checked current Node/Core trees.
The catalog must be the source-bound repository file. The mapper joins explicit
aliases transitively and retains each provider occurrence, withdrawn record,
upstream fix-version note and explicit upstream commit reference. Package
annotations distinguish CLI, test-only and unobserved inclusion; recorded platform
constraints remain visible. Missing annotations stay unknown.

A successful mapping exits zero with
`UPSTREAM_ALIAS_MAPPING_RECORDED_REVIEW_REQUIRED`. It records review work and
never grants an advisory waiver or release approval. Upstream version ranges
are not compared numerically with fork versions; package absence, ancestry and
empty effective-namespace queries cannot establish that the fork is unaffected.
A changed selected fork version does not relabel the snapshot's original query.
Freshness policy, upstream fix semantics, function execution, additional build
profiles and independent assessment remain required. The same bounded artifact
retains `upstream-advisories/review.json` and `execution.json`; the mapper makes
no network requests or native Go calls.

## Reviewed networking library migration

The selected op-geth v1.101702.3 release is a library release. Its 38-path source
delta includes STUN/DTLS migration, Engine fields, optional header RLP, PostExec
hash/receipt changes, embedded registry data and Interop/Lagoon naming. The Node
SDK update does not qualify an op-geth executable, a real Engine or activation of
those newer chain rules.

The original candidate was produced at Node commit
`9a5c6d6145bc46979c08b7d3b3c06100ee4a8b3e`, automatic fork run
[`43423594443`](https://github.com/edgepillar/zvm-node/actions/runs/43423594443),
artifact `14035362014`, archive SHA-256
`5c7393653be404d4395285449e186dea249c21b05e59c3dabf8f6a22930c9daf`.
Its complete command outputs and Node/Core source/lock observations were
independently checked. The original STUN v2 parser failed the bounded malformed
attribute control as expected; the same body with only its module import changed
passed all five parent/subtest events on STUN v3.1.5. That legacy failure remains
historical evidence.

Old and candidate control executables used identical CI-formatted source and
Go 1.26.11. Both Core chain-321 genesis profiles, legacy/EIP-1559/deposit
transaction hashes/encodings and a legacy header were identical. The old payload
codec accepted the candidate payload with its additional null `targetGasLimit`
key. Slotful header encoding changed; Amsterdam, PostExec behavior, Lagoon,
actual Engine execution, real operator state and live-chain acceptance remain
unqualified. Temporary binaries stayed in a fresh hosted workspace and were not
published. The temporary lane, recipe and control-source copies were removed
after adoption; their original Git revision and receipts remain available.

`node/library_compatibility_test.go` freezes the retained legacy vector outputs
for future SDK review. `node/stun_dependency_test.go` retains the malformed-input
regression in complete normal/race Node suites. The generated candidate sums are
unchanged; its already selected STUN v3 requirement becomes direct because that
test imports it. Fresh integrity/tidy, build/vet, full Node tests, container and
source/graph qualification are required for the adopted source.

Registry results retain separate meanings. The earlier Go source scan reported
`GO-2026-4479` for DTLS v2.2.7. A separate GitHub version query also matched
`GHSA-34rh-wp3j-6cxc` for STUN v2.0.0. Neither matched the selected DTLS v3.1.4 or
STUN v3.1.5 in the reviewed GitHub snapshot; absence in a registry is not global
advisory coverage. References: [DTLS](https://pkg.go.dev/vuln/GO-2026-4479),
[STUN](https://github.com/advisories/GHSA-34rh-wp3j-6cxc), and the
[library release](https://github.com/ethereum-optimism/op-geth/releases/tag/v1.101702.3).
