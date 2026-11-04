package main

import (
	"math/big"
	"sort"

	"github.com/Zenoglyphs-VM/zvm-core/derive"
	"github.com/Zenoglyphs-VM/zvm-core/rollup"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	"github.com/holiman/uint256"
)

func word(n uint64) common.Hash { return common.BigToHash(new(big.Int).SetUint64(n)) }
func initial(g *core.Genesis) *state.StateDB {
	st, err := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
	must(err)
	for address, a := range g.Alloc {
		st.SetBalance(address, uint256.MustFromBig(a.Balance), tracing.BalanceIncreaseGenesisBalance)
		st.SetNonce(address, a.Nonce, tracing.NonceChangeGenesis)
		st.SetCode(address, a.Code, tracing.CodeChangeGenesis)
		for key, value := range a.Storage {
			st.SetState(address, key, value)
		}
	}
	check(st.IntermediateRoot(true) == g.ToBlock().Root() && st.Error() == nil, "initial specification root differs")
	return st
}

// The state oracle does not invoke an EVM, block processor or RPC simulation.
// It applies the EIP-4788/2935 ring rules, L1Block packed storage layout, and
// two Regolith deposit nonce/mint rules to all twelve original genesis accounts.
func advance(st *state.StateDB, p *derive.Plan, parent common.Hash, number uint64, mint *big.Int) common.Hash {
	m := p.Momentum
	st.SetState(params.BeaconRootsAddress, word(m.Timestamp%8191), word(m.Timestamp))
	st.SetState(params.BeaconRootsAddress, word(m.Timestamp%8191+8191), common.Hash(m.Hash))
	st.SetState(params.HistoryStorageAddress, word((number-1)%8191), parent)
	packed := new(big.Int).Or(new(big.Int).Lsh(new(big.Int).SetUint64(m.Timestamp), 64), new(big.Int).SetUint64(m.Height))
	st.SetState(rollup.L1BlockAddr, word(0), common.BigToHash(packed))
	st.SetState(rollup.L1BlockAddr, word(2), common.Hash(m.Hash))
	st.SetState(rollup.L1BlockAddr, word(4), common.BytesToHash(rollup.Inbox[:]))
	st.SetNonce(rollup.L1InfoDepositor, number, tracing.NonceChangeGenesis)
	st.SetNonce(depositor, number, tracing.NonceChangeGenesis)
	st.AddBalance(depositor, uint256.MustFromBig(mint), tracing.BalanceIncreaseGenesisBalance)
	root := st.IntermediateRoot(true)
	check(st.Error() == nil, "specification state failure")
	return root
}

func addresses() []common.Address {
	out := []common.Address{depositor, rollup.L1InfoDepositor}
	for _, a := range l2Predeploys() {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Hex() < out[j].Hex() })
	check(len(out) == 14, "account scope differs")
	return out
}
