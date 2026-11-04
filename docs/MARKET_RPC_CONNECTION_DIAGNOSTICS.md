# Owned HTTP/1.1 connection diagnostics

The browser fixture separately observes the native Go server connection cycle
after the existing tagged `POST /rpc` handler returns. This extends diagnostics;
it does not establish a transport repair, body delivery or navigation success.
The first publication-lifecycle workflow at
`5d3a5191010ff9c865f2e7548f8b0386f44a28f6`,
[run 41552614369](https://github.com/edgepillar/zvm-node/actions/runs/41552614369),
remains failed. Its final durable Karum bulk QSR case reached the original
20-second network-quiet deadline. A tagged handler return and primary header
echo did not establish browser completion. Its cause is `NOT_ESTABLISHED`.

Fresh owned servers install `ConnContext` and `ConnState` before starting. An
existing hook is refused, never replaced. Each valid owned nonce can bind only
one native HTTP/1.1 active cycle from the same observer. Duplicate nonces,
reused/ambiguous cycles, missing or foreign context, and other HTTP versions
cannot become unique connection evidence. No `ResponseWriter`, `Request` or
`net.Conn` is replaced, and no body, status, request value or address is read
into the diagnostic. All original financial/RPC replies and HTTP arguments
continue unchanged.

The connection map retains at most 32 active connection objects, releases its
entry on closed/hijacked terminal state and preserves its overflow flag. The
existing nonce ledger still retains at most 256 records. Request records keep
only bounded cycle booleans after connection release. A later request's idle or
close cannot be attached to an earlier cycle. An idle/closed/hijacked transition
before the handler-return record marks that cycle ambiguous.

The separate read-only `/__fixture/rpc-connections` route uses the same fixture
capability and a unique selection of at most eight owned nonces. Nonces and the
capability do not appear in its response. The version-one connection snapshot
does not change the original version-one handler-correlation schema. Its Node
reader limits bytes to 16,384, chunks to 64 and time to 750 ms, refuses redirects
and non-loopback/configuration/schema changes, and reports fixed errors without
private transport text. The selected nonce mapping stays internal. The browser
checker reads it only after the original checks or original failure, before
closing its owned browser. No extra CDP command or product RPC is introduced.

An observed HTTP/1.1 idle transition is stronger server-lifecycle evidence than
handler return alone. In the selected
[Go 1.25.0 source](https://github.com/golang/go/blob/go1.25.0/src/net/http/server.go),
the server calls `finishRequest` and checks connection reuse before notifying
idle. This does not prove peer receipt, body consumption or the matching
browser/CDP session. Closed or hijacked states also do not prove a write error
or its cause. HTTP/2 state is connection-wide and is explicitly not used as
per-request evidence. The observer never substitutes any of these facts for
the unchanged 20-second deadline, 100-ms quiet interval, zero-active-request
rule, signatures, receipts, balances, ownership or other financial assertions.

The checker also gives the browser driver its own child cancellation context.
Cleanup stops and joins that browser, then joins the Relayer while the common
scope is still live, then cancels the common scope. The existing Relayer exit,
warning/error and parent-deadline checks remain unchanged. This corrects a
secondary cleanup-order ambiguity after a browser failure; it is not an
explanation or repair of the original network-quiet failure.

Twenty-four finite connection controls and two child-shutdown controls execute
inside the existing normal/race-selected `TestMarketRPCProgress` parent. Every
original named native test remains. Twenty JavaScript reader controls are
separate from actual HTTP, SDK, browser and financial execution. A local parser
or synthetic result does not qualify the hosted Node 22 production-page lane.
The source still requires all 52 profiles, 1,244 financial checks, 70 wallet
transactions/test NoM publications, 106 joined Relayer CLI lifetimes, 36 wallet
SIGKILL joins and all original compiler/full-client/SQL oracles at the new
revision. All 42 original requirements remain active; production is `NO_GO`.
