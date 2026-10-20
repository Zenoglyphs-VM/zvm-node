package verify

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// A runtime with one immutable at bytes 2..3 and a CBOR tail: a1 (map of one)
// then 4 bytes, then the length 0x0005.
func runtimeHex(immutable, metaByte string) string {
	return "6001" + immutable + "5b" + "a1" + metaByte + "000000" + "0005"
}

func TestCompareRuntime(t *testing.T) {
	imm := immutableRefs{"7": {{Start: 2, Length: 2}}}
	onchain, _ := hex.DecodeString(runtimeHex("abcd", "11"))

	m, meta, err := compareRuntime(runtimeHex("0000", "11"), nil, imm, onchain)
	if err != nil || m != Exact || len(meta) != 7 {
		t.Fatalf("same code, immutable filled: got %q %x %v", m, meta, err)
	}
	if m, _, _ := compareRuntime(runtimeHex("0000", "22"), nil, imm, onchain); m != Partial {
		t.Fatalf("different metadata hash: got %q", m)
	}
	if m, _, _ := compareRuntime("6002"+"0000"+"5b"+"a111000000"+"0005", nil, imm, onchain); m != "" {
		t.Fatalf("different program: got %q", m)
	}
	if m, _, _ := compareRuntime(runtimeHex("0000", "11")+"00", nil, imm, onchain[:3]); m != "" {
		t.Fatalf("an immutable past the end of the chain's code must not match: %q", m)
	}
}

func TestLinkPlaceholders(t *testing.T) {
	// PUSH20 <library>, with solc's placeholder where the address goes.
	placeholder := "__$" + strings.Repeat("a", 34) + "$__"
	compiled := "73" + placeholder + "00"
	lib := strings.Repeat("12", 20)
	onchain, _ := hex.DecodeString("60" + lib + "00") // not a library self-address: first byte differs
	links := linkRefs{"L.sol": {"L": {{Start: 1, Length: 20}}}}
	if m, _, err := compareRuntime(compiled, links, nil, onchain); err != nil || m != "" {
		t.Fatalf("first byte differs, must not match: %q %v", m, err)
	}
	onchain[0] = 0x73
	if m, _, err := compareRuntime(compiled, links, nil, onchain); err != nil || m != Exact {
		t.Fatalf("linked library: got %q %v", m, err)
	}
}

func TestLibraryAddress(t *testing.T) {
	compiled := "73" + strings.Repeat("00", 20) + "3014"
	onchain, _ := hex.DecodeString("73" + strings.Repeat("ab", 20) + "3014")
	if m, _, _ := compareRuntime(compiled, nil, nil, onchain); m != Exact {
		t.Fatalf("a library's own address is written at deployment: got %q", m)
	}
}

func TestConstructorArgs(t *testing.T) {
	creation := "6080" + "a1" + "11000000" + "0005"
	args := strings.Repeat("00", 31) + "2a"
	input, _ := hex.DecodeString(creation + args)
	_, meta := splitMetadata(mustHex(runtimeHex("0000", "11")))

	m, found, err := constructorArgs(creation, nil, meta, input)
	if err != nil || m != Exact || hex.EncodeToString(found) != args {
		t.Fatalf("exact: %q %x %v", m, found, err)
	}
	other, _ := hex.DecodeString("6080" + "a1" + "22000000" + "0005" + args)
	m, found, _ = constructorArgs(creation, nil, meta, other)
	if m != Partial || hex.EncodeToString(found) != args {
		t.Fatalf("partial: %q %x", m, found)
	}
	if m, _, _ := constructorArgs("60806080", nil, meta, input[:2]); m != "" {
		t.Fatalf("input shorter than creation code: %q", m)
	}
}

func TestSanitize(t *testing.T) {
	ok := `{"language":"Solidity","sources":{"a/A.sol":{"content":"import \"../B.sol\"; contract A {}"},"B.sol":{"content":"contract B {}"}},
		"settings":{"optimizer":{"enabled":true},"outputSelection":{"*":{"*":["*"]}},"modelChecker":{"engine":"all"}}}`
	in, err := sanitize([]byte(ok))
	if err != nil {
		t.Fatal(err)
	}
	if _, has := in.Settings["modelChecker"]; has {
		t.Error("the model checker must be removed")
	}
	if string(in.Settings["outputSelection"]) != outputs {
		t.Error("output selection must be this node's")
	}
	for name, bad := range map[string]string{
		"a url":              `{"sources":{"A.sol":{"urls":["/config/jwt.hex"]}}}`,
		"an absolute import": `{"sources":{"A.sol":{"content":"import \"/config/jwt.hex\";"}}}`,
		"a named import":     `{"sources":{"A.sol":{"content":"import {X} from \"C:/secret.sol\";"}}}`,
		"a remapping":        `{"sources":{"A.sol":{"content":""}},"settings":{"remappings":["x=/config/"]}}`,
		"vyper":              `{"language":"Vyper","sources":{"A.vy":{"content":""}}}`,
		"no sources":         `{"language":"Solidity","sources":{}}`,
	} {
		if _, err := sanitize([]byte(bad)); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: want ErrInvalid, got %v", name, err)
		}
	}
}

// A comment is whitespace to solc, so an import with a comment where the
// space would be is still an import, and a comment is no place to hide one.
// A string's slashes are not a comment.
func TestSanitizeSeesImportsThroughComments(t *testing.T) {
	refused := []string{
		`import/**/"/config/jwt.hex";`,
		`import/*a*/ "/config/jwt.hex";`,
		"import//\n\"/config/jwt.hex\";",
		`string constant u = "http://x"; import "/config/jwt.hex";`,
	}
	for _, src := range refused {
		input := `{"language":"Solidity","sources":{"a.sol":{"content":` + mustJSON(t, src) + `}}}`
		if _, err := sanitize([]byte(input)); !errors.Is(err, ErrInvalid) {
			t.Errorf("%q was accepted", src)
		}
	}
	allowed := []string{
		`// import "/config/jwt.hex";` + "\ncontract A {}",
		`/* import "/config/jwt.hex"; */ contract A {}`,
		`import "./B.sol"; string constant s = "/* not a comment */";`,
	}
	for _, src := range allowed {
		input := `{"language":"Solidity","sources":{"a.sol":{"content":` + mustJSON(t, src) + `}}}`
		if _, err := sanitize([]byte(input)); err != nil {
			t.Errorf("%q was refused: %v", src, err)
		}
	}
}

func mustJSON(t *testing.T, s string) string {
	t.Helper()
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestPick(t *testing.T) {
	out := &solcOutput{Contracts: map[string]map[string]compiled{
		"a/Token.sol": {"Token": {}, "Helper": {}},
		"b/Token.sol": {"Token": {}},
	}}
	if name, _, err := pick(out, "Helper"); err != nil || name != "a/Token.sol:Helper" {
		t.Fatalf("unique name: %q %v", name, err)
	}
	if _, _, err := pick(out, "Token"); err == nil {
		t.Fatal("an ambiguous name must ask for the path")
	}
	if name, _, err := pick(out, "b/Token.sol:Token"); err != nil || name != "b/Token.sol:Token" {
		t.Fatalf("path and name: %q %v", name, err)
	}
}

func mustHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}
