# Signed market admission and canonical recovery

`TestExecutedMarketAdmissionAndCanonicalRecovery` connects the actual market
HTTP routes to `Book.Submit`, block-hash Multicall3/exchange checks, the pinned
op-geth executor, NFT indexing and the production derivation/index/book loops.
Its single-signature and bulk-signature profiles submit two real EIP-712
inputs before any on-chain order approval exists. The native buy executes the
same signed input, including the bulk proof where applicable.

The fixture starts with an empty book and makes no SQL order inserts or cache
updates. It verifies the HTTP signing domain, exact order hashes and integer
prices, signature/proof preservation, executed and indexed ownership, listing
visibility and sale history. Wrong-domain, expired, unowned-token and corrupted
signature submissions must fail while preserving the accepted intents. The
expired and unowned controls must actually reach the exchange checker.

After a fill, the declared source changes to another branch at the same
horizon. The production deriver selects the replacement; its normal index
rewind callback invalidates the book observations in the shared transaction.
The production index follower and book worker then converge without test calls
to `IndexBlock`, `Rewind` or `Book.step`. The orphaned fill reopens only after a
fresh executed check. Returning to the first branch must reproduce the same
HTTP owners, listings and canonical sale while retaining original input bytes.

The fixture reuses the existing archive-backed engine with 16 MiB trie caches
and one SQLite connection. Three bounded production workers are joined at each
stable observation horizon. The test runs in the ordinary Go suite and in the
selected race suite; no frontend dependency installation or local SDK download
is needed for this additional integration boundary.

This is executed local consistency under synthetic source authority. The NoM
observations are unsigned fixtures, the Engine/eth endpoints are custom
loopback adapters and the scalar-one signer is a public test key held only in
memory. Transaction transport still uses the native forced-transaction lane.
This does not qualify an unmodified daemon, an independent NoM verifier,
hardware wallet, browser provider signing, public-chain settlement or physical
power-loss recovery. The separately pinned real-browser qualification retains
its own explicit cached-order/provider controls; this test does not relabel
those browser actions as executions of the signed market fixture.

The checker observer records completed successful calls, unexpected RPC errors
and calls canceled by their joined worker context separately. A canceled attempt
never counts as a successful exchange check or a distinct accepted header. The
zero-unexpected-error requirement, canonical-hash assertions and original-input
checks remain in force. Shutdown cancellations are reported explicitly rather
than treated as completed execution failures or silently discarded.


### Independent passive page-lifecycle observation

Revision 30019289aadb3927391e20ef2ead5753ee7ed2cc passed its Go and
full-client jobs, including signed orders across the declared colocated process
exit. Its first automatic browser job failed in the Karum single-order partial
batch case at the original 20-second navigation deadline. The dedicated Network
session retained one item API read with only request/response extra-info signals;
the failure cause remains NOT_ESTABLISHED. Extra-info status 200 is not a
response/loading completion, and the failure remains retained.

The additional observer records the public Page request, response, finished and
failed lifecycle separately, without issuing requests or resolving interception.
Page records use exact request-object identity internally. Correlation with the
dedicated Network session uses URL and method only and explicitly establishes no
protocol identity. At most 256 active and eight recent page records are retained,
with eight signal names per record; overflow and getter errors remain visible.
Diagnostics omit URL query values, headers, bodies and raw error text. They can
distinguish an observed page terminal event from a missing dedicated-session
terminal event if the failure recurs, while preserving uncertain correlations.

Page observations never complete a tracked Network request. The original
20-second overall deadline, DOMContentLoaded, zero active Network requests,
100-ms quiet interval and all route, DOM, HTTP/error, signature, receipt and
balance checks remain unchanged. The original 16 pure protocol controls remain,
with five additional passive-observer controls. Synthetic event controls do not
establish a browser root cause, actual browser recovery, complete N04 acceptance
or production readiness. NoM and owner/platform/release gates remain separate.
