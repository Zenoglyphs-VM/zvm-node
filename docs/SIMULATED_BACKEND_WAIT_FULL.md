# Pinned simulated-backend condition wait

The normal Go job at Node source `73934a367e9e040a8d52fa5116f04f374dd19ca5`
timed out in `TestBookIsBounded` after 600 seconds. The retained stack waits in
`miner.(*Payload).WaitFull`, called by the real simulated backend. The exact
notification schedule was not observed in that CI run.

The pinned op-geth `v1.101702.2` source at
[`e8800cffe53d459cde8a07c8e8f1de9d86e79e07`](https://github.com/ethereum-optimism/op-geth/blob/e8800cffe53d459cde8a07c8e8f1de9d86e79e07/miner/payload_building.go#L226)
waits unconditionally, including when the completed full payload was published
before the waiter arrived. The proposed predicate loop handles that specific
late-waiter case. It does not establish the exact schedule of the earlier CI
timeout or complete error/cancellation behavior in the upstream client.

The native regression publishes through the actual upstream implementation
before calling `WaitFull`. On the original source it records the missed
notification as an actual test failure, sends one rescue notification, and
joins the waiter. The same test must pass after the one-method correction.
Compiler errors, unexpected failures, skipped tests or a different source hash
cannot satisfy the original negative control.

The Go job uses a separate, explicitly modified test-only dependency copy and
temporary Go workspace for these controls, the normal suite and selected race
tests. The original module cache and tracked module files remain unchanged.
The copy is limited to 256 MiB and 20,000 regular files, without links, and
requires 10 GiB plus its allocation to remain free. Every original file hash
and the exact two-file delta are checked afterward. A refusal preserves the
failed job; it does not trigger a retry or alternate dependency selection.

Production build/vet and the separate full-client and marketplace jobs retain
the original pinned dependency. Passing tests in the modified workspace are
evidence for that explicit test subject. They are not proof that the original
dependency's normal suite passed. No dependency upgrade, production-client
repair, automatic retry, full N04 acceptance, release or deployment is claimed.

This candidate has not yet run in hosted CI. All original acceptance fields
and retained failures remain unchanged.
