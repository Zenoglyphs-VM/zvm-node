// Package bench deploys a standard EVM contract set on the ZVM and drives
// load through both lanes, so the numbers come from the same contracts
// everyone else benchmarks.
package bench

import (
	"embed"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
)

// Compiled with solc 0.8.30 (--optimize, --evm-version prague) from
// contracts/bench, and Uniswap V2 from its published npm artifacts.
//
//go:embed artifacts/*.json
var artifactFS embed.FS

type Artifact struct {
	ABI  abi.ABI
	Bin  []byte
	Name string
}

type Artifacts struct {
	byName map[string]*Artifact
}

func hexBytes(s string) []byte {
	return common.FromHex(strings.TrimSpace(s))
}

// AddCombined registers every contract in a `solc --combined-json abi,bin`
// output, by contract name. Other packages embed their own compiler output
// and add it to the same set.
func (a *Artifacts) AddCombined(raw []byte) error {
	var combined struct {
		Contracts map[string]struct {
			ABI json.RawMessage `json:"abi"`
			Bin string          `json:"bin"`
		} `json:"contracts"`
	}
	if err := json.Unmarshal(raw, &combined); err != nil {
		return err
	}
	for key, c := range combined.Contracts {
		name := key[strings.LastIndex(key, ":")+1:]
		parsed, err := abi.JSON(strings.NewReader(string(c.ABI)))
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		a.byName[name] = &Artifact{ABI: parsed, Bin: hexBytes(c.Bin), Name: name}
	}
	return nil
}

func LoadArtifacts() (*Artifacts, error) {
	out := &Artifacts{byName: map[string]*Artifact{}}

	raw, err := artifactFS.ReadFile("artifacts/bench.json")
	if err != nil {
		return nil, err
	}
	if err := out.AddCombined(raw); err != nil {
		return nil, fmt.Errorf("bench artifacts: %w", err)
	}

	for file, name := range map[string]string{
		"artifacts/UniswapV2Factory.json":  "UniswapV2Factory",
		"artifacts/UniswapV2Router02.json": "UniswapV2Router02",
		"artifacts/WZNN.json":              "WZNN",
		"artifacts/UniswapV2Pair.json":     "UniswapV2Pair",
	} {
		raw, err := artifactFS.ReadFile(file)
		if err != nil {
			continue
		}
		var art struct {
			ABI      json.RawMessage `json:"abi"`
			Bytecode string          `json:"bytecode"`
		}
		if err := json.Unmarshal(raw, &art); err != nil {
			return nil, fmt.Errorf("%s: %w", file, err)
		}
		parsed, err := abi.JSON(strings.NewReader(string(art.ABI)))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		out.byName[name] = &Artifact{ABI: parsed, Bin: hexBytes(art.Bytecode), Name: name}
	}
	return out, nil
}

func (a *Artifacts) Get(name string) (*Artifact, error) {
	art, ok := a.byName[name]
	if !ok {
		return nil, fmt.Errorf("no artifact named %q", name)
	}
	return art, nil
}

func (a *Artifacts) MustGet(name string) *Artifact {
	art, err := a.Get(name)
	if err != nil {
		panic(err)
	}
	return art
}

// Deploy returns the init code for a constructor call.
func (art *Artifact) Deploy(args ...any) ([]byte, error) {
	packed, err := art.ABI.Pack("", args...)
	if err != nil {
		return nil, fmt.Errorf("%s constructor: %w", art.Name, err)
	}
	return append(append([]byte{}, art.Bin...), packed...), nil
}

// Call packs a method call.
func (art *Artifact) Call(method string, args ...any) ([]byte, error) {
	packed, err := art.ABI.Pack(method, args...)
	if err != nil {
		return nil, fmt.Errorf("%s.%s: %w", art.Name, method, err)
	}
	return packed, nil
}
