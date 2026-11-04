# Current request handling and browser connection observations

This is a correction to passive test-fixture diagnostics. It does not establish
the cause or repair of a failed browser profile. Production, dependencies,
workflow selection, request continuation/abort calls and their arguments remain
unchanged. Navigation retains its original 20-second overall deadline, 100 ms
quiet period, zero-active gate and every profile/financial assertion.

The failed first workflow at source
`eb28181c796096b5fdfd00e90b5a623eefd2c883` retained a single pending RPC POST
in `karum-single-partial-batch`. Its local continue method promise fulfilled,
but neither observed Network session recorded a matching response or terminal
loading event before the deadline. All 124 owned RPC handler lifetimes had
returned normally, with no exact CDP-to-handler correlation. Those facts do not
prove wire acknowledgement or response delivery. The root cause remains
`NOT_ESTABLISHED`.

Workflow: https://github.com/edgepillar/zvm-node/actions/runs/41415603685.

The lifecycle observer records the handling flag at its last page event. That
is not necessarily its current value. A new separate observation reads only
the reviewed SDK's `isInterceptResolutionHandled()` boolean when diagnostics
are queried. Matching still requires the existing cached Page/request ID, URL
and method; ambiguity, missing APIs, invalid values and exceptions remain
explicitly unavailable. No headers, body, error text or raw identifiers are
reported. The original gate does not consume this observation.

In the reviewed Puppeteer 25.11.0 source, the flag is set before the continuation
protocol call. The error path resets it and tolerates some protocol errors.
Method fulfillment therefore remains distinct from acknowledgement. A current
true or false flag is also not independent proof of transport outcome, server
identity or root cause. It can become stale immediately after the query.

The same correction preserves `reason: null` for an available page-client
observation instead of displaying `NOT_OBSERVED`. Failure diagnostics read the
reviewed `Browser.connected` boolean, with a guarded legacy method fallback,
instead of returning unknown solely because the older method is absent.
Connection state is not network or application readiness.

Primary SDK source at the resolved reviewed commit:

- [HTTP request API and internal error handling](https://github.com/puppeteer/puppeteer/blob/043cd46e594b39ea77343e221a59724183448059/packages/puppeteer-core/src/api/HTTPRequest.ts)
- [CDP continuation implementation](https://github.com/puppeteer/puppeteer/blob/043cd46e594b39ea77343e221a59724183448059/packages/puppeteer-core/src/cdp/HTTPRequest.ts)
- [Browser connection API](https://github.com/puppeteer/puppeteer/blob/043cd46e594b39ea77343e221a59724183448059/packages/puppeteer-core/src/api/Browser.ts)

Standard-library fake-object controls cover fresh state changes, unavailable
and ambiguous matches, private/operational property refusal, snapshot bounds,
modern and legacy browser state, truthful available reasons and unchanged
network completion. Every original navigation, request-ID, client, action and
RPC-snapshot control remains. Hosted qualification still belongs to its exact
source/run; no passing parent run is transferred to a new revision. Signed NoM,
default-scale histories, wallets, hardware/platform and independent acceptance
remain separate. Full N04 and production remain `NO_GO`.
