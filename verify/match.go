package verify

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// Match is how closely compiled code reproduced the code on chain. The terms
// are Sourcify's.
type Match string

const (
	// Exact: every byte, the metadata hash included, so these are the very
	// files that were compiled.
	Exact Match = "exact"
	// Partial: every byte but the trailing CBOR metadata, so the program is the
	// same and the sources may differ in comments, whitespace or file names.
	Partial Match = "partial"
)

type offset struct {
	Start  int `json:"start"`
	Length int `json:"length"`
}

// linkRefs is solc's evm.*.linkReferences: file → library → offsets.
type linkRefs map[string]map[string][]offset

// immutableRefs is evm.deployedBytecode.immutableReferences: AST id → offsets.
type immutableRefs map[string][]offset

var errLength = errors.New("compiled and on-chain code do not line up")

func (l linkRefs) offsets() []offset {
	var out []offset
	for _, libs := range l {
		for _, offs := range libs {
			out = append(out, offs...)
		}
	}
	return out
}

func (m immutableRefs) offsets() []offset {
	var out []offset
	for _, offs := range m {
		out = append(out, offs...)
	}
	return out
}

// fill decodes compiled hex after taking the bytes a linker or a constructor
// writes — library addresses and immutable values — from the chain. The source
// does not determine those bytes, so they cannot disagree with it.
func fill(compiledHex string, onchain []byte, holes []offset) ([]byte, error) {
	s := []byte(strings.TrimPrefix(compiledHex, "0x"))
	if len(s)%2 != 0 {
		return nil, fmt.Errorf("compiled code has an odd number of hex digits")
	}
	for _, o := range holes {
		end := o.Start + o.Length
		if o.Start < 0 || o.Length < 0 || end*2 > len(s) || end > len(onchain) {
			return nil, errLength
		}
		copy(s[o.Start*2:], hex.EncodeToString(onchain[o.Start:end]))
	}
	return hex.DecodeString(string(s))
}

// splitMetadata separates solc's CBOR metadata from the end of code, where the
// last two bytes give its length. Code without a well-formed tail has no
// metadata part.
func splitMetadata(code []byte) (body, meta []byte) {
	if len(code) < 3 {
		return code, nil
	}
	n := int(code[len(code)-2])<<8 | int(code[len(code)-1])
	if n == 0 || n+2 > len(code) {
		return code, nil
	}
	start := len(code) - 2 - n
	if code[start] < 0xa1 || code[start] > 0xa7 { // a CBOR map of one to seven entries
		return code, nil
	}
	return code[:start], code[start:]
}

// libraryAddress copies a library's own address into compiled code: a
// library's runtime opens with PUSH20 <address>, which is zero until deployed.
func libraryAddress(compiled, onchain []byte) {
	if len(compiled) > 21 && len(onchain) > 21 && compiled[0] == 0x73 && onchain[0] == 0x73 &&
		bytes.Equal(compiled[1:21], make([]byte, 20)) {
		copy(compiled[1:21], onchain[1:21])
	}
}

// compareRuntime reports how compiled runtime code matches the code on chain,
// or "" when it does not. meta is the compiled code's metadata tail, which a
// creation-code comparison needs to mask.
func compareRuntime(compiledHex string, links linkRefs, immutables immutableRefs, onchain []byte) (Match, []byte, error) {
	holes := append(links.offsets(), immutables.offsets()...)
	code, err := fill(compiledHex, onchain, holes)
	if errors.Is(err, errLength) {
		return "", nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	libraryAddress(code, onchain)
	body, meta := splitMetadata(code)
	if bytes.Equal(code, onchain) {
		return Exact, meta, nil
	}
	chainBody, chainMeta := splitMetadata(onchain)
	if meta != nil && chainMeta != nil && bytes.Equal(body, chainBody) {
		return Partial, meta, nil
	}
	return "", nil, nil
}

// constructorArgs finds compiled creation code at the start of the deploying
// transaction's input and returns what follows it: the ABI-encoded constructor
// arguments. On a partial match the runtime's metadata, which creation code
// embeds, is masked wherever it appears.
func constructorArgs(compiledHex string, links linkRefs, meta []byte, input []byte) (Match, []byte, error) {
	code, err := fill(compiledHex, input, links.offsets())
	if errors.Is(err, errLength) {
		return "", nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	if len(input) < len(code) {
		return "", nil, nil
	}
	if bytes.Equal(code, input[:len(code)]) {
		return Exact, input[len(code):], nil
	}
	if len(meta) == 0 {
		return "", nil, nil
	}
	masked := append([]byte(nil), code...)
	for from := 0; ; {
		i := bytes.Index(masked[from:], meta)
		if i < 0 {
			break
		}
		at := from + i
		copy(masked[at:at+len(meta)], input[at:at+len(meta)])
		from = at + len(meta)
	}
	if bytes.Equal(masked, input[:len(code)]) {
		return Partial, input[len(code):], nil
	}
	return "", nil, nil
}
