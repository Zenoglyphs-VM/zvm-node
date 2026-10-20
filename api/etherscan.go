package api

// The contract module of Etherscan's API, so `forge verify-contract` and
// hardhat-verify publish to this explorer with their verifier URL set to
// <explorer>/api. Blockscout and Routescan serve the same shape for the same
// reason: it is what the tools already speak.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/Zenoglyphs-VM/zvm-node/verify"
)

func etherscanReply(w http.ResponseWriter, ok bool, result any) {
	status, message := "0", "NOTOK"
	if ok {
		status, message = "1", "OK"
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": status, "message": message, "result": result})
}

func (s *Server) etherscan(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if s.cfg.Verify != nil {
		r.Body = http.MaxBytesReader(w, r.Body, int64(s.cfg.Verify.MaxInputBytes())*2+1<<20)
	}
	if err := r.ParseForm(); err != nil {
		etherscanReply(w, false, "could not read the request: "+err.Error())
		return
	}
	get := func(k string) string { return strings.TrimSpace(r.FormValue(k)) }
	if get("module") != "contract" {
		etherscanReply(w, false, "only the contract module is served here")
		return
	}
	if id := get("chainid"); id != "" && id != strconv.FormatUint(s.cfg.Rollup.L2ChainID, 10) {
		etherscanReply(w, false, fmt.Sprintf("this explorer serves chain %d, not %s", s.cfg.Rollup.L2ChainID, id))
		return
	}
	if s.cfg.Verify == nil {
		etherscanReply(w, false, "contract verification is off on this node")
		return
	}

	switch get("action") {
	case "verifysourcecode":
		addr, err := parseAddress(get("contractaddress"))
		if err != nil {
			etherscanReply(w, false, "Invalid contractaddress")
			return
		}
		input := get("sourceCode")
		switch get("codeformat") {
		case "solidity-standard-json-input":
		case "solidity-single-file", "":
			runs, _ := strconv.Atoi(get("runs"))
			input, err = singleFileInput(get("contractname"), input, get("optimizationUsed") == "1", runs, get("evmversion"))
			if err != nil {
				etherscanReply(w, false, err.Error())
				return
			}
		default:
			etherscanReply(w, false, fmt.Sprintf("codeformat %q is not served; send solidity-standard-json-input", get("codeformat")))
			return
		}
		args := get("constructorArguements") // sic, Etherscan's spelling
		if args == "" {
			args = get("constructorArguments")
		}
		job, err := s.cfg.Verify.Submit(ctx, verify.Request{
			Address: addr, Compiler: get("compilerversion"), Contract: get("contractname"),
			Input: json.RawMessage(input), ConstructorArgs: args,
		})
		switch {
		case errors.Is(err, verify.ErrAlreadyVerified):
			etherscanReply(w, false, "Contract source code already verified")
		case err != nil:
			etherscanReply(w, false, err.Error())
		default:
			etherscanReply(w, true, job.ID)
		}

	case "checkverifystatus":
		job, err := s.cfg.Verify.Job(ctx, get("guid"))
		if err != nil {
			etherscanReply(w, false, "Unknown GUID")
			return
		}
		switch job.Status {
		case "verified":
			etherscanReply(w, true, "Pass - Verified")
		case "failed":
			etherscanReply(w, false, "Fail - Unable to verify. "+job.Message)
		default:
			etherscanReply(w, false, "Pending in queue")
		}

	case "getabi", "getsourcecode":
		addr, err := parseAddress(get("address"))
		if err != nil {
			etherscanReply(w, false, "Invalid Address format")
			return
		}
		code, err := s.cfg.Eth.CodeAt(ctx, addr, nil)
		if err != nil {
			etherscanReply(w, false, err.Error())
			return
		}
		v, err := s.cfg.Verify.Lookup(ctx, addr, code)
		if err != nil {
			etherscanReply(w, false, err.Error())
			return
		}
		if get("action") == "getabi" {
			if v == nil {
				etherscanReply(w, false, "Contract source code not verified")
				return
			}
			etherscanReply(w, true, string(v.ABI))
			return
		}
		etherscanReply(w, true, []any{s.sourceEntry(ctx, addr, code, v)})

	default:
		etherscanReply(w, false, fmt.Sprintf("action %q is not served here", get("action")))
	}
}

// sourceEntry is getsourcecode's answer for one address, in Etherscan's field
// names, including its double-braced standard JSON.
func (s *Server) sourceEntry(ctx context.Context, addr common.Address, code []byte, v *verify.Verified) map[string]string {
	entry := map[string]string{
		"SourceCode": "", "ABI": "Contract source code not verified", "ContractName": "", "CompilerVersion": "",
		"OptimizationUsed": "", "Runs": "", "ConstructorArguments": "", "EVMVersion": "Default", "Library": "",
		"LicenseType": "", "Proxy": "0", "Implementation": "", "SwarmSource": "",
	}
	if v != nil {
		name := v.Name
		if i := strings.LastIndex(name, ":"); i >= 0 {
			name = name[i+1:]
		}
		var settings struct {
			Optimizer struct {
				Enabled bool `json:"enabled"`
				Runs    int  `json:"runs"`
			} `json:"optimizer"`
			EVMVersion string `json:"evmVersion"`
		}
		json.Unmarshal(v.Settings, &settings)
		entry["SourceCode"] = "{" + string(v.Input) + "}"
		entry["ABI"] = string(v.ABI)
		entry["ContractName"] = name
		entry["CompilerVersion"] = "v" + v.Compiler
		entry["OptimizationUsed"] = map[bool]string{true: "1", false: "0"}[settings.Optimizer.Enabled]
		entry["Runs"] = strconv.Itoa(settings.Optimizer.Runs)
		entry["ConstructorArguments"] = v.ConstructorArgs
		if settings.EVMVersion != "" {
			entry["EVMVersion"] = settings.EVMVersion
		}
	}
	if p := verify.DetectProxy(ctx, s.cfg.Eth, addr, code); p != nil {
		entry["Proxy"], entry["Implementation"] = "1", p.Implementation
	}
	return entry
}

// singleFileInput wraps one flattened source the way Etherscan's single-file
// form means it.
func singleFileInput(contract, source string, optimize bool, runs int, evmVersion string) (string, error) {
	if strings.TrimSpace(source) == "" {
		return "", fmt.Errorf("sourceCode is empty")
	}
	name := contract
	if i := strings.LastIndex(name, ":"); i >= 0 {
		name = name[i+1:]
	}
	if name == "" {
		name = "Contract"
	}
	if runs <= 0 {
		runs = 200
	}
	settings := map[string]any{"optimizer": map[string]any{"enabled": optimize, "runs": runs}}
	if evmVersion != "" && !strings.EqualFold(evmVersion, "default") {
		settings["evmVersion"] = evmVersion
	}
	out, err := json.Marshal(map[string]any{
		"language": "Solidity",
		"sources":  map[string]any{name + ".sol": map[string]string{"content": source}},
		"settings": settings,
	})
	return string(out), err
}
