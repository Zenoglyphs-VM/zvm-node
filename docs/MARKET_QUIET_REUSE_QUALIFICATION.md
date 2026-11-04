# Reusing observed pre-navigation quiet

Only the pre-navigation gate may reuse a quiet interval already observed in its
own protocol session. If no request is active and at least the original 100 ms
has elapsed since tracked activity, it needs no new quiet timer. A partially
elapsed interval waits its remainder. New requests and genuine terminal events
update the activity baseline. Headers, cache provenance and extra-info do not
finish requests or erase active work.

The original `wait` API and post-navigation wait still start a fresh 100-ms
interval. Both navigation phases retain the same single 20-second overall
deadline, zero-active rule, every request type, page/session isolation and
closed/detached/broken/identity/timestamp/terminal guards. Reusing quiet cannot
accept an expired deadline or a closed page. The runtime has no new clock or
timeout option; only a private shared waiter selects the pre-navigation baseline.

`node scripts/ci-market-quiet-reuse.mjs` runs two bounded standard-library
children once each. The exact prior `3fa79576a58563bbe0a8e9b173fe7eae87c4fa52`
module must exit 1 at the specific redundant-window assertion. Its already-quiet
control schedules another pre-navigation timer and does not start the document
before the fake clock advances. The current module must pass eleven controls,
including zero extra pre-quiet timers, an exact remaining partial interval,
fresh original/post-navigation waits, new activity, headers and stale terminals,
closure/detachment/broken/expired refusals, page isolation and one shared budget.
Clock and timer observations occur only in these pure controls. Each child has
a five-second wall limit and 32-KiB output limits; it executes no browser, SDK,
HTTP request or wallet operation.

The original 21 protocol/passive, eight terminal-loading and nine pre-transition
controls remain independently required, including their exact prior-source
negatives. Every original Go/race, 38 full-client event, ten whole-client kill,
complete SQL image, compiler/application and 52-profile financial/durable-wallet
oracle remains. Existing test, matrix and job deadlines remain unchanged.

The source opportunity is avoiding a timer when the full quiet predicate is
already true. Retained hosted elapsed observations are not controlled benchmarks
or guaranteed savings; an actual browser speedup and historical timeout cause
remain unestablished. The original 42 acceptance requirements, independent NoM,
release-genesis, default-scale, real-wallet/platform/hardware and operator gates
remain. The PR remains Draft and production remains `NO_GO`.
