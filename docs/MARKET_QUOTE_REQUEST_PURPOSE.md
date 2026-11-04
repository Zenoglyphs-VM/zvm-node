# Owned delayed-approval request purpose

The existing Karum single-listing fixture holds two real approval RPC requests:
one for its account-change refusal and one for its form-edit snapshot control.
The fixture now marks each exact held request object, before its existing
`continue()` operation, with one of those two allowlisted purposes. The marker
reads no request field and retains at most two weakly held marks. It does not
change the approval matcher, hold/release order, wallet operation or product reply.

The existing owned RPC correlation helper snapshots that purpose before the
first original continue call. A separate `OWNED-MARKET-QUOTE-PURPOSE` record
joins it through the same unique owned-header record selected for the original
RPC diagnostics. Equal URL/method pairs cannot borrow another request's purpose.
Missing, duplicate, conflicting and unconfigured observations stay unavailable
or ambiguous. A late mark cannot reclassify an earlier continue operation.
An unmarked object means only that this observer did not mark it.

The output contains only purpose enums, scalar bounds and the selected target
index. It exposes no opaque request ID, header value, nonce, body, fulfillment
value or error text. It sends no additional HTTP request or protocol command.
Purpose-observer refusal preserves the original continue arguments, invocation
count and returned value or Promise. The report does not qualify navigation,
protocol acknowledgement, server identity, response-body delivery or authentication.

The first workflow at `9cd1ebffd9ce7335ee7f993156cb087ed761a8dd` failed with a
pending `POST /rpc` after the account-change refusal control. Its original owned
header linked that pending request to a normal handler return and an idle HTTP/1
cycle, while browser response/loading terminal delivery remained unproved.
Those records did not establish whether it was the deliberately held approval
request. This additional identity observation addresses that specific gap; it
does not repair or retrospectively establish the historical cause.

The pinned Puppeteer 25.11.0
[request implementation](https://github.com/puppeteer/puppeteer/blob/043cd46e594b39ea77343e221a59724183448059/packages/puppeteer-core/src/cdp/HTTPRequest.ts)
handles internal command errors separately from the public method's outcome.
Its [network lifecycle implementation](https://github.com/puppeteer/puppeteer/blob/043cd46e594b39ea77343e221a59724183448059/packages/puppeteer-core/src/cdp/NetworkManager.ts)
also distinguishes response metadata from loading completion. Neither source
inspection nor a fulfilled method, purpose mark, header echo or idle connection
replaces the original matching Network terminal.

Eighteen standard-library controls cover exact object identity, both purposes,
privacy, caps, ambiguous and missing echoes, late attribution, callback refusals,
unchanged operation outcomes and refusal to complete a pending Network request.
They use no SDK, browser or socket. Current hosted qualification still requires
the exact new source and its first automatic workflow. All 21 original protocol
and passive controls, eight terminal-loading, nine pre-transition and eleven
quiet-reuse controls remain, as do the original 20-second deadline, 100-ms quiet
interval, zero-active requirement and all 52 financial/wallet profiles.

All original native, full-client recovery, compiler and application requirements
remain. No timeout, request type, financial predicate or ownership gate is relaxed.
No browser elapsed speedup or historical root cause is claimed. Full N04 and all
42 acceptance requirements remain open where unqualified. Production is NO_GO.
