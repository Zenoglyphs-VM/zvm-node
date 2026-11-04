# Owned RPC request and document context

The fixture records fixed RPC method, call and target-role classes from the
original intercepted request data, before its first existing continue call.
Only the owned loopback `POST /rpc` is eligible. Input is limited to 16 KiB of
UTF-8 JSON and 16 batch entries. Unknown methods, calls and targets become fixed
`OTHER` classes; unavailable or refused observations stay unavailable. Addresses,
request IDs, headers, calldata, JSON-RPC IDs, body values and error text are never
reported. Call selectors are computed lazily by the already pinned application
codec from declared public ABIs, without an SDK import in the observer module.

The same existing primary Network session separately observes Document request
events. A pending RPC's original frame/loader binding is compared locally with
the latest Document request in that frame. The output contains only ordinals
and relation booleans. Scope, frame, start, binding and capture tables are
bounded; reused bindings cannot establish document context. A foreign navigation
invalidates the earlier owned document observation. Opaque identifiers remain in
the bounded local maps and never enter the report.

`OWNED-MARKET-RPC-CONTEXT` joins those observations only through the exact same
unique owned-header selection as the existing RPC diagnostics. Equal URL/method
pairs and forged selections cannot borrow another request's context. A recent
header sample has no pending entry and does not invent document provenance.
Opaque observer and capture handles cannot be forged into observations.

Document request events do not prove renderer document commitment. A method,
call role, loader relation or header echo does not prove protocol acknowledgement,
response-body delivery, canonicality or authentication. These diagnostics never
complete, retire, filter, replay or replace a request. They send no additional
HTTP request or protocol command and add no runtime clock, timer or deadline.
The original navigation module and workflow are unchanged. Original continue
arguments and outcomes, all financial predicates and every existing test remain.

The first workflow at `e10c16b2957d95009c579e14b6f98911cbfb6d7f` passed its Go,
full-client and compiler scope but failed in `karum-single` with a pending RPC.
Its purpose snapshot was `NOT_MARKED` while one deliberate approval hold was
marked. Its records showed three navigation attempts, two completions and three
Document requests, but did not record the pending RPC method or loader relation.
This observation addresses that specific gap. Its historical cause and complete
52-profile financial/wallet qualification remain unestablished.

Twenty standard-library controls cover classification, privacy, byte/batch/table
bounds, missing and reused identities, frame/session isolation, foreign document
invalidation, snapshot independence, exact header binding, lazy codec setup and
preservation of the original pending-request refusal. The existing request-purpose
entrypoint runs them after its original eighteen controls. No browser, SDK,
socket or HTTP request is run by those controls.

All 21 original protocol/passive, eight terminal-loading, nine pre-transition,
eleven quiet-reuse and eighteen purpose controls remain. The 20-second overall
deadline, fresh original/post-navigation 100-ms interval, zero-active requirement,
identity/timestamp/matching-terminal rules and all 52 financial/wallet profiles
remain. All original native, compiler and recovery oracles and all 42 acceptance
requirements remain authoritative. No historical root-cause repair or elapsed
speedup is claimed. Production is NO_GO.
