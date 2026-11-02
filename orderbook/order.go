// Package orderbook keeps the NFT market's signed orders: it stores the ones
// that can be filled and re-checks every one against the exchange before it is
// shown (DECISIONS.md D28).
//
// The order itself — the EIP-712 hashing, the bulk listing's Merkle tree,
// signing and recovery — is zvm-core/orderbook, because it is an encoding three
// separate programs have to agree on: this package, the browser, and
// ZVMExchange.sol. Storage and matching need a database and the compiled
// contract, so they stay here.
//
// The names below are aliases rather than a wrapper layer. book.go and the
// tests were written against a single package and read better for not knowing
// the split happened; a wrapper would also give the exchange's constants two
// definitions, which is exactly the drift moving the encoding was meant to end.
package orderbook

import (
	core "github.com/Zenoglyphs-VM/zvm-core/orderbook"
)

// Sides and signature kinds, as the exchange numbers them.
const (
	SideSell = core.SideSell
	SideBuy  = core.SideBuy

	KindSingle  = core.KindSingle
	KindBulk    = core.KindBulk
	KindOnChain = core.KindOnChain
)

// Why an order cannot be filled, as checkOrder reports it.
const (
	CodeOK           = core.CodeOK
	CodeBadSignature = core.CodeBadSignature
	CodeCancelled    = core.CodeCancelled
	CodeStaleNonce   = core.CodeStaleNonce
	CodeNotStarted   = core.CodeNotStarted
	CodeExpired      = core.CodeExpired
	CodeFilled       = core.CodeFilled
	CodeNotHeld      = core.CodeNotHeld
	CodeNotFunded    = core.CodeNotFunded
	CodeMalformed    = core.CodeMalformed

	// CodeUnchecked is a local cache state, never a checkOrder ABI code.
	CodeUnchecked uint8 = 255
)

// The EIP-712 domain the exchange signs under.
const (
	DomainName    = core.DomainName
	DomainVersion = core.DomainVersion
)

var (
	OrderTypeHash = core.OrderTypeHash
	RootTypeHash  = core.RootTypeHash
)

type (
	Order    = core.Order
	Input    = core.Input
	ABIOrder = core.ABIOrder
	ABIInput = core.ABIInput
)

var (
	Domain       = core.Domain
	OrderDigest  = core.OrderDigest
	RootDigest   = core.RootDigest
	Tree         = core.Tree
	ProcessProof = core.ProcessProof
	Sign         = core.Sign
	SignBulk     = core.SignBulk
	Recover      = core.Recover
	ABIOrders    = core.ABIOrders
	FromABI      = core.FromABI
	U            = core.U
	B            = core.B
)

func CodeText(code uint8) string {
	if code == CodeUnchecked {
		return "awaiting a fresh chain check"
	}
	return core.CodeText(code)
}
