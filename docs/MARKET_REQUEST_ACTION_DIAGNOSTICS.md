# Owned request-action diagnostics

The marketplace fixture observes the public `continue()` and `abort()` calls it
already makes. Each operation still receives the same arguments and runs once.
The observer reads no request fields, headers, bodies, URLs, result values or
error messages. It sends no protocol command and replaces no product RPC reply.

On a navigation failure, `pageRequestActionMatches` joins the existing bounded
same-page request-ID, URL and method observations to that exact page request
object's recorded method outcomes. Multiple matching page objects stay ambiguous.
Only allowlisted action kinds, states, scalar ages and bounds are reported.
The request ID, URL, error and fulfillment value are not emitted by this observer.
There are at most three records per weakly held request and 256 pending observed
actions. Overflow omits diagnostics and still invokes the original operation.

For a native Promise, the observer returns a derived Promise with the same
fulfillment value or rejection reason. Promise identity and one settlement
microtask differ. Rejection is rethrown so an unhandled rejection is not consumed.
Synchronous values and throws retain their identities. Unknown thenables and
Promise subclasses are returned without assimilation. A refused observer
attachment is marked `UNOBSERVED_PROMISE` and returns the original Promise.

A `FULFILLED` method outcome establishes neither a protocol acknowledgement nor
network completion or server-to-browser delivery. Puppeteer's pinned
[request implementation](https://github.com/puppeteer/puppeteer/blob/puppeteer-v25.11.0/packages/puppeteer-core/src/cdp/HTTPRequest.ts)
sets interception state separately from its internal command handling. This
fixture observes the public method's returned Promise, not the internal command
transport. The independent Network request completion gate remains authoritative.

The first whole-client kernel-kill workflow at
`cffd7a4185f621fbed7dc4bd7facd65d216d9c9d` passed its native scope and failed in the
Karum partial-batch browser navigation. Its cause remains `NOT_ESTABLISHED`.
These diagnostics do not repair that cause, qualify the complete matrix, or turn
a later passing run into a root-cause result. All original signature, price,
receipt, refund, payout, balance and profile controls remain. The overall
20-second deadline, 100 ms quiet period and zero-active-request requirement are
unchanged. No request category, ExtraInfo event, cache signal or method outcome
is accepted as a substitute for Network completion.

Standard-library controls cover pending, fulfilled, rejected and synchronous
outcomes, exact error/value propagation, a separately joined Node rejection
child, privacy, allocation bounds, ambiguous matches and preservation of the
original Network timeout. They require no browser, SDK or network. Hosted browser
and native qualification still require the exact new source and first automatic
workflow evidence. Full N04 and production acceptance remain open.
