# Exact signed-order timestamps

The exchange signs `listingTime` and `expirationTime` as uint64. The HTTP wire
format keeps numbers through 9007199254740991 and uses unsigned decimal strings
above that value through 18446744073709551615. The node refuses larger numeric
JSON tokens, fractions, exponents, null, negatives and overflow. This prevents
JavaScript parsing from silently rounding an adjacent signed value. Ordinary
legacy input and response bytes remain unchanged. Full-range clients require
a matching node; an older server rejects decimal-string timestamps.

`WireInput` converts only at the HTTP boundary and in the Go HTTP client. Core's
order type, EIP-712 domain/hash, signature/proof bytes and contract ABI remain
unchanged. Original input JSON stored by the book keeps its Core encoding and
is read by Go without a JavaScript number conversion. Existing signed input
records are not migrated or rewritten.

## SQLite representation

Existing timestamps through MaxInt64 remain INTEGER. Larger uint64 values bind
as TEXT consisting of `u` and exactly twenty decimal digits. The nonnumeric tag
prevents INTEGER affinity from converting the value to REAL. All such values
sort after all legacy integers, and their fixed-width suffix preserves unsigned
ordering within the high range. `TimeValue` must also encode any high-range SQL
cutoff. Current wall-clock queries naturally use the legacy integer range.

Strict readers accept only nonnegative INTEGER or canonical high-range TEXT.
They refuse floating-point values, BLOBs, negatives and malformed tags. API
order rows, bids and best-item listings emit the same exact numeric/string wire
format. There is no table rebuild, legacy-data cleanup or historical mutation.
A downgrade does not understand the high-range representation.
The representation follows SQLite's documented [type affinity and comparison
rules](https://www.sqlite.org/datatype3.html); the hosted tests verify them with
the project's actual SQLite driver.

## Interfaces

Both order modules retain bigint values for signing and ABI encoding. They
send large timestamps as decimal strings and decode them back exactly on
`fetchInput`. NFT calendar labels convert to milliseconds only inside Date's
range and show `Far future` beyond it. Expiry checks use exact seconds. Karum
countdowns use bigint division/remainder for their complete uint64 range.
Package locks, SDK versions, Core gitlinks and golden vectors are unchanged.

## Required evidence

Standard-Node wire/formatter controls have no SDK, browser or settlement
capabilities. The hosted SQLite controls reopen an actual database and compare
SQL ordering/cutoffs at both integer boundaries and across a deterministic
sample. Codec controls retain legacy bytes and complete input/hash values.
A separate simulated-EVM approval transaction exercises the log-import inlet
with both times above MaxInt64. The follower must retain exact values, return
NOT_STARTED from its actual contract check and preserve the original payload
when the log is read again after a declared rewind.

The opt-in `TestMarketFullWidthTimeClients` lane loads each actual pinned order
module with its own locked Viem. Single and bulk profiles sign four full-width
orders with a deliberately public fixture account, forward the actual wire
payload to the real Go API, execute block-hash Solidity checks and read the
persisted input back through the module's `fetchInput` and ABI encoder. Go
independently compares hashes, EIP-712 digests, signatures and Merkle proofs.
A high future listing must reach Solidity and return NOT_STARTED, and an unsafe
numeric JSON submission must return HTTP 400. This narrow lane sends no EVM
transaction and starts no Chrome process. Its events remain separate from the
16 existing transaction/recovery browser profiles and navigation controls.

Public test keys, synthetic NoM canonicality/plasma and a custom Engine/eth RPC
adapter remain fixture authority. Earlier complete financial adapter campaigns
retain their original source pins; a new order-module revision does not make
those campaigns fresh. Full batch-fill/refund/currency/royalty/ERC-1271 browser
paths, final supported-product agreement, independent NoM, unmodified execution
daemons, physical wallets and public settlement remain separate requirements.
