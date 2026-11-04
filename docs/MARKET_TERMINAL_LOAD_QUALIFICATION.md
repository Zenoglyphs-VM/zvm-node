# Terminal loading events in market navigation qualification

The market navigation tracker retains each request until its matching
`Network.loadingFinished` or `Network.loadingFailed` event. Response headers,
extra-info and cache provenance remain passive diagnostics. A response being
available does not establish that its load is complete. The original request
identity, timestamp and redirect guards still apply.

The same overall 20-second navigation deadline, 100-ms quiet period,
zero-active-request requirement and complete request-type coverage remain.
DOM, signature, receipt, canonical-state, gas, payment, refund and balance
predicates are unchanged. The tracker does not infer completion from an
interception method promise, a server handler return, a cache signal or another
protocol session.

The hosted standard-library regression first executes the exact public
`fcbab39dbfcb32a5521e3469823fa7757da30a5c` tracker fixture. It requires the specific
header-only assertion failure and actual exit 1. It then executes the same
vector against the current module and requires all eight controls to pass.
Both children are bounded to five seconds and 32 KiB per output stream. There
is no browser, SDK, HTTP request, package installation or additional frontend
build in these controls. The original cached-request control now requires the
loading terminal as well as the response observation.

The complete original 52-profile production-page and wallet matrix remains
required in the current source's own first automatic CI workflow. The original
compiler, native, full-client, SQL and wallet lifecycle cases remain required.
No prior workflow is retried or transferred as current execution evidence.

Two prior sources retain browser navigation deadline failures at different
profiles. The premature header-completion behavior is a source defect; it is
not proof of the cause of every earlier timeout. A later successful matrix
does not establish the historical cause. Independent NoM authority/finality,
real wallets, release genesis, owner acceptance and production remain open.

Primary protocol reference:
[Chrome DevTools Network definitions](https://github.com/ChromeDevTools/devtools-protocol/blob/master/pdl/domains/Network.pdl).
