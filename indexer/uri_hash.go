package indexer

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
)

// HashURIHost is an optional capability. A plugin requiring historical URI
// observations must not substitute the latest-state Host.CallURI operation.
type HashURIHost interface {
	CallURIAtHash(context.Context, common.Address, []byte, common.Hash) (string, error)
}

func decodeObservedURI(out []byte) (string, error) {
	if len(out) > MaxURIBytes+96 {
		return "", errors.New("URI response exceeds its bound")
	}
	var text string
	if len(out) == 32 {
		// Preserve the older bytes32 return form accepted by CallURI.
		text = strings.TrimRight(string(out), "\x00")
	} else {
		typ, err := abi.NewType("string", "", nil)
		if err != nil {
			return "", err
		}
		values, err := (abi.Arguments{{Type: typ}}).Unpack(out)
		if err != nil || len(values) != 1 {
			return "", errors.New("URI response is not a complete ABI string")
		}
		var ok bool
		text, ok = values[0].(string)
		if !ok {
			return "", errors.New("URI response has an unexpected type")
		}
	}
	if len(text) > MaxURIBytes || !utf8.ValidString(text) {
		return "", errors.New("URI response is oversized or invalid UTF-8")
	}
	return text, nil
}

func (ix *Indexer) CallURIAtHash(ctx context.Context, address common.Address, data []byte, hash common.Hash) (string, error) {
	if hash == (common.Hash{}) {
		return "", errors.New("URI observation requires an explicit block hash")
	}
	if ix.eth == nil {
		return "", errors.New("URI observation has no execution client")
	}
	out, err := ix.eth.CallContractAtHash(ctx, ethereum.CallMsg{To: &address, Data: data, Gas: uriGas}, hash)
	if err != nil {
		return "", err
	}
	return decodeObservedURI(out)
}
