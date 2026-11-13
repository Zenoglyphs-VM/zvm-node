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
upstream URL. The effective op-geth selection remains v1.101702.2. Node's
simulated-backend tests still select the older networking dependency closure,
including DTLS v2.2.7. Node-specific advisory and call-reachability review remains
open. The separately proposed Core source scan result does not transfer to this
Node/Core source combination. No advisory waiver or reachability exclusion is
introduced by these lock updates.

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
