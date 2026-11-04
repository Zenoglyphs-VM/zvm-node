# Configured Engine payload retrieval

The node command passes its loaded rollup configuration to Core's
`DialEngineForConfig`. An absent profile and explicit `isthmus` retrieve payloads
with `engine_getPayloadV4`; `karst` uses `engine_getPayloadV5`. Fork choice stays
V3, payload import stays V4, and a refused request does not trigger a version
downgrade. Unknown profiles are refused while loading configuration.

This integration currently pins Core
`acf39556a3f465a6885f23fc3d0cc8065891d76c` from the public `edgepillar/zvm-core`
fork, pending the separate upstream Core review. The integration depends on
[Core PR #7](https://github.com/Zenoglyphs-VM/zvm-core/pull/7). The compiler
isolation lane checks this same exact Node dependency. The separately pinned
Relayer retains its original Core dependency and original evidence; those
historical references are not rewritten as new Node qualification.

`TestNodeCommandEngineProfile` launches the actual `nodecmd.Run` path in owned
bounded children against declared loopback Engine/Ethereum/NoM responses. It
checks one payload retrieval for each of legacy, Isthmus and Karst, then
interrupts and joins the child. The declared RPC refusal stops before payload
import. This control proves command wiring and cancellation, not VM execution,
NoM authority, receipt/state correctness or complete Karst Node operation.

The unchanged complete Node workflow remains required: normal and selected
race tests, original simulator red/green control, compiler isolation and vendor
artifacts, full-client recovery and every signed marketplace profile. The Core
four-profile complete-client cohort remains separate exact-source evidence.
Bounded default and selected constructor-state Node/client controls are
described separately. NoM finality, broad signed-user/economic/default-scale
behavior and maintainer/operator/release acceptance remain separate gates.

The previous Core 75b0bb2bb4a798519226217bb566badb9f71ac6d and Node c706b048d16d1d9c9b7cb324d379ad16dde34f26
qualifications remain historical at their exact pins. Adoption of the new Core
and constructor-state genesis requires this source's own complete seven-job CI;
older results are not transferred to this revision. No shipped chain file or
operator database is migrated by this contribution.
