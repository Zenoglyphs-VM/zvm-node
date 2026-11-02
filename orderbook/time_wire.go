package orderbook

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/Zenoglyphs-VM/zvm-core/znn"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
)

const maxSafeJSONTime = uint64(1<<53 - 1)

// TimeJSON retains legacy safe numbers and uses decimal strings for the rest
// of uint64. Signed order bytes and the shared Core/contract ABI are unchanged.
func TimeJSON(value uint64) any {
	if value <= maxSafeJSONTime {
		return value
	}
	return strconv.FormatUint(value, 10)
}

type wireTime uint64

func (value wireTime) MarshalJSON() ([]byte, error) { return json.Marshal(TimeJSON(uint64(value))) }

func (value *wireTime) UnmarshalJSON(raw []byte) error {
	if len(raw) == 0 || len(raw) > 128 {
		return errors.New("order time must be an unsigned decimal uint64")
	}
	var decimal string
	quoted := raw[0] == '"'
	if quoted {
		if err := json.Unmarshal(raw, &decimal); err != nil {
			return err
		}
	} else {
		decimal = string(raw)
	}
	if len(decimal) == 0 || len(decimal) > 20 {
		return errors.New("order time must be an unsigned decimal uint64")
	}
	for _, digit := range decimal {
		if digit < '0' || digit > '9' {
			return errors.New("order time must be an unsigned decimal uint64")
		}
	}
	exact, err := strconv.ParseUint(decimal, 10, 64)
	if err != nil {
		return errors.New("order time exceeds uint64")
	}
	if !quoted && exact > maxSafeJSONTime {
		return errors.New("order time above the safe JSON integer range requires a decimal string")
	}
	*value = wireTime(exact)
	return nil
}

// Keep this field order equal to Core's Order so ordinary legacy responses
// retain their canonical bytes. The wire-only timestamp type never reaches
// EIP-712 hashing, ABI encoding or the original stored input.
type wireOrder struct {
	Trader         common.Address `json:"trader"`
	Side           uint8          `json:"side"`
	Collection     common.Address `json:"collection"`
	TokenID        *znn.BigInt    `json:"tokenId"`
	AnyToken       bool           `json:"anyToken"`
	Amount         *znn.BigInt    `json:"amount"`
	PaymentToken   common.Address `json:"paymentToken"`
	Price          *znn.BigInt    `json:"price"`
	ListingTime    wireTime       `json:"listingTime"`
	ExpirationTime wireTime       `json:"expirationTime"`
	Nonce          *znn.BigInt    `json:"nonce"`
	Salt           *znn.BigInt    `json:"salt"`
}

// WireInput is the HTTP boundary. Core's Input remains the original encoding
// for internal callers, persisted intent bytes, signatures and contract calls.
type WireInput struct {
	Order     wireOrder     `json:"order"`
	Kind      uint8         `json:"kind"`
	Signature hexutil.Bytes `json:"signature"`
	Proof     []common.Hash `json:"proof"`
	Root      *common.Hash  `json:"root,omitempty"`
}

func ToWireInput(in *Input) *WireInput {
	if in == nil {
		return nil
	}
	o := in.Order
	return &WireInput{Order: wireOrder{
		Trader: o.Trader, Side: o.Side, Collection: o.Collection, TokenID: o.TokenID, AnyToken: o.AnyToken,
		Amount: o.Amount, PaymentToken: o.PaymentToken, Price: o.Price, ListingTime: wireTime(o.ListingTime),
		ExpirationTime: wireTime(o.ExpirationTime), Nonce: o.Nonce, Salt: o.Salt},
		Kind: in.Kind, Signature: in.Signature, Proof: in.Proof, Root: in.Root}
}

func (in *WireInput) Input() *Input {
	if in == nil {
		return nil
	}
	o := in.Order
	return &Input{Order: Order{
		Trader: o.Trader, Side: o.Side, Collection: o.Collection, TokenID: o.TokenID, AnyToken: o.AnyToken,
		Amount: o.Amount, PaymentToken: o.PaymentToken, Price: o.Price, ListingTime: uint64(o.ListingTime),
		ExpirationTime: uint64(o.ExpirationTime), Nonce: o.Nonce, Salt: o.Salt},
		Kind: in.Kind, Signature: in.Signature, Proof: in.Proof, Root: in.Root}
}

// TimeValue stores legacy values as SQLite INTEGER. Above MaxInt64 a tagged,
// zero-padded decimal TEXT value avoids INTEGER affinity's conversion to REAL.
// Every high value sorts after every INTEGER; the fixed 20-digit text suffix
// preserves unsigned ordering within that range. No legacy row is rewritten.
func TimeValue(value uint64) any {
	if value <= math.MaxInt64 {
		return int64(value)
	}
	return fmt.Sprintf("u%020d", value)
}

func TimeFromStorage(raw any) (uint64, error) {
	if value, ok := raw.(int64); ok {
		if value < 0 {
			return 0, errors.New("negative stored order time")
		}
		return uint64(value), nil
	}
	text, ok := raw.(string)
	if !ok || len(text) != 21 || text[0] != 'u' || strings.Trim(text[1:], "0123456789") != "" {
		return 0, errors.New("invalid stored order time encoding")
	}
	value, err := strconv.ParseUint(text[1:], 10, 64)
	if err != nil || value <= math.MaxInt64 || TimeValue(value) != text {
		return 0, errors.New("invalid stored uint64 order time")
	}
	return value, nil
}

// StoredTime is a strict scanner: floats, negative legacy values and arbitrary
// text cannot silently become an imprecise timestamp in an API response.
type StoredTime uint64

func (value *StoredTime) Scan(raw any) error {
	exact, err := TimeFromStorage(raw)
	if err != nil {
		return err
	}
	*value = StoredTime(exact)
	return nil
}
