# Dependency and distribution review

CI action implementations use full commit pins with readable major-version
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
