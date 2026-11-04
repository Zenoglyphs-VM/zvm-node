# Compiler service failure boundary

The Node uses `verify.RemoteRunner` only when an explicit verifier endpoint is
configured. This source adopts the separate bounded compiler/cache service into
the index/recovery candidate. It does not change the devnet default, add local
fallback, retry a failed verification or select another endpoint.

The service adds exactly one `X-ZVM-Compiler-Error` response header on its known
failures. The fixed version-one combinations are:

| HTTP status | Code | Client error class |
|---|---|---|
| 503 | `v1.busy` | `ErrCompilerBusy` |
| 503 | `v1.cache-full` | `ErrCompilerCacheFull` |
| 400 | `v1.invalid` | `ErrInvalid` |
| 400 | `v1.output-limit` | `ErrCompilerOutputLimit` |
| 504 | `v1.deadline` | `context.DeadlineExceeded` |
| 500 | `v1.unavailable` | `ErrCompilerUnavailable` |

The client never reads an unsuccessful response body. Missing headers from an
older service retain only its known status classes: 400 invalid, 503 capacity,
504 deadline, and otherwise unavailable. A duplicate, empty, unknown or
status-inconsistent code is unavailable. A failure code on HTTP 200 also
refuses success. This is error classification from a configured endpoint, not
remote attestation or proof that a compiler ran.

Redirects cannot silently switch the compiler provider or carry its token to a
different endpoint. Caller-owned HTTP client options are copied, not mutated.
Transport and body-read failures expose fixed public errors; caller cancellation
and deadlines retain their types. Successful bodies keep the finite decoded
output cap, refuse a declared overflow before reading, and reject a streaming
overflow rather than returning a truncated success.

Compiler version names are limited to 128 bytes before release/cache work or
service diagnostic logging. Release identity, SHA-256/pin checks, per-service
concurrency/deadline/input/output/stderr limits and non-evicting cache bounds
remain separate. The verification worker still finishes a failed job and drops
its submitted input under the established lifecycle; a later explicit request
is independent. Fixed compiler-service errors keep a proxy or transport's
private response text out of that persisted job message. This does not qualify
every registry, RPC or application diagnostic surface.

The source adds component/race cases for typed and legacy failures, no body
reads, bounded success, transport redaction, duplicate headers, redirect refusal,
version preflight and actual SQLite verification-job outcomes. Its declared
isolated production-service lane retains all six original lifetimes and adds
five using the Node's actual RemoteRunner for service/client output caps,
cache pressure, busy/deadline/reuse and invalid-version rejection. Controlled
compiler fixtures and host-only synthetic canaries remain explicit; real vendor
compilers, real Node secrets/databases, live NoM and production keys are unused.

Fresh exact-source CI is required before claiming these results. Original Node
index/full-client/market/browser controls and the original compiler scope remain
in the workflow. Production workload/capacity, deployment/TLS/firewall policy,
operator cache ownership, independent publisher authority, per-invocation
descendant containment, supported platforms and maintainer/operator acceptance
remain open. This does not complete N01, N04 or the original 42-item roadmap.
