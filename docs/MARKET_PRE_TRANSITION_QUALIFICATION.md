# Pre-transition navigation qualification

The market harness now requires known earlier protocol requests to terminate and
the network to remain quiet before it starts `page.goto`. It keeps the existing
quiet wait after the document transition. Both waits and document setup consume
the same original 20-second deadline, with 100 ms of quiet and zero active
requests. No request type is filtered or retired because a different page is
visible. Headers, cache signals and extra-info remain passive observations.

The previous source waits for quiet only after `page.goto`, so an active request
from the prior page can still exist when the next navigation starts. A retained
ERC-1271 Karum failure showed an item-1 GET pending while item 2 was visible. This
ordering observation does not establish the historical timeout's root cause or
prove that the current source repairs it.

`node scripts/ci-market-navigation-transition.mjs` runs two bounded standard-library
children once each. The exact prior module must exit 1 at the specific assertion
that an earlier request remains active when `page.goto` starts. The current
module must pass nine controls: one and multiple earlier requests, incomplete
headers/cache, failed loads, redirect identity/timestamp checks, closure,
conflicting identities, one shared deadline and isolation between pages. A clock
is advanced only inside the pure shared-deadline control; the runtime helper has
no new clock or timeout option. Each child has a five-second wall limit and
32-KiB output limits. These controls execute no browser, SDK, HTTP or wallet.

The original 21 protocol/passive controls, exact header-only failure and eight
terminal-loading controls remain required. Every original source selection and
named final-gate predicate remains. The current source must also execute all
original Go/race, 38 full-client terminal events, ten whole-client SIGKILL joins,
complete SQL table images, compiler/application materials and all 52 financial
and durable-wallet profiles. Existing per-test, matrix and job deadlines remain.
The extra pre-transition quiet wait can add time; no elapsed speedup is claimed.

The original genesis, dependency pins, API/RPC replies, request arguments,
signature, receipt, balance and wallet ownership checks remain. A passed fake
protocol control is not real response-delivery, historical-cause, live-chain,
hardware, release-genesis, independent NoM or production proof. All 42 original
acceptance requirements remain in scope. The PR remains Draft and production
remains `NO_GO` until the original independent gates are satisfied.
