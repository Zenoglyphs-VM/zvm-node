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
service as separate execution inputs. The existing `ubuntu-latest` label,
Docker base tags, package repositories and downloaded Go toolchain are mutable;
action pins do not make those inputs immutable. Container/platform checks,
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
authentication is removed, and unsafe PR checkout remains disabled. Hosted
execution must qualify this actual profile; another repository's action success
does not qualify Node. Enabled Go caches include both Node and Core lock files.

`tools/dependency-review/source_closure.py` verifies the exact selected Node
commit, every tracked regular-file blob and mode, and the one allowed Core
Gitlink. It independently checks Core commit
`29e6d9b30be94de3772c850c7a1e9811585a8833`, its configured public URL, source tree,
lock hashes and regular files. Other Gitlinks, links, changed indices,
untracked/ignored inputs or local authentication config require review. Config
values and private checkout paths are not printed. This profile preserves the
existing Core source; it does not adopt the separately proposed Core dependency
or Engine-authentication changes.

## Temporary exact lock preparation

The read-permission `Dependency lock preparation` workflow selects Go 1.26.11
without automatic toolchain switching and generates proposed Node locks in its
disposable checkout. The recipe is bound to the original root lock hashes and
selects reviewed brotli, websocket, compress, OpenTelemetry, x/crypto and x/text
versions. The ordinary Node SDK/locks are unchanged until that candidate is
independently inspected and committed for a fresh native qualification.

The producer permits only root `go.mod` and `go.sum` changes, then rechecks the
complete Node/Core source closure. It verifies the resolved selections, original
module/replacement identities and checksum consistency. Public module projections
omit cache paths. A 540-second aggregate command budget and individual stream
caps bound collection. Actual exits, completeness, source/lock pins, generated
locks and failures are retained for fourteen days in an explicit ZIP archive.
The producer does not execute a build, tests, vulnerability scan, release or
repository writeback. Native compatibility, Node advisory coverage and shipped
notice/license review remain separate requirements. The temporary preparation
workflow and recipe are removed after their outputs are adopted; original Git
revisions and qualification records remain historical evidence.
