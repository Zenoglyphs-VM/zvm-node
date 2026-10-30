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


### Fixture connection state transition

The first automatic workflow of revision 4a6c484444457ff734f4718047fd92e2370b073e
passed Go, full-client recovery and all 21 pure protocol/passive controls, but
failed the Karum single-order approval profile waiting for Connect wallet while
the retained page diagnostic already showed its connected account. All nine
navigations completed with matched Network starts/completions. This differs
from the earlier item-read navigation deadline, whose cause remains
NOT_ESTABLISHED. The current live failure's causal attribution is also pending.

The old helper sampled whether connection was needed, then asynchronously
waited/clicked the disconnected label. Ordinary auto-restoration can invalidate
that snapshot. The replacement reads the current header and acts in one
synchronous page evaluation. It accepts only the selected public fixture
account's unambiguous displayed address among the two declared test identities,
refuses wrong/ambiguous labels, and never clicks an already connected account
button. Connect and Fixture wallet are each selected at most once per call.
It waits for the connected header within one original twelve-second UI budget;
there is no reload, repeated prompt, provider mutation or new transaction.
Short display matching coordinates this fixed fixture and is not general wallet
authentication. Original actual signature, HTTP, receipt and balance oracles
remain independent requirements.

Ten pure deterministic controls reproduce the stale check/click sequence and
exercise restoration, disabled controls, identity refusal, one-action limits and
the unchanged deadline. They do not qualify a real browser or establish either
live failure's root cause. The existing 21 protocol controls, twenty-second
navigation budget, zero-active Network requirement and 100-ms quiet interval
remain unchanged. Full browser/owner/platform/NoM/release acceptance remains
open; all 42 original acceptance fields remain authoritative and production
remains NO_GO.
