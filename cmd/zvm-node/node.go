package main

import (
	"github.com/Zenoglyphs-VM/zvm-node/nodecmd"
)

func init() {
	register("node", "derive the ZVM from NoM into op-reth, index it, and serve the explorer, the NFT market and the DEX", runNode)
}

// runNode is the node with the two extensions this binary ships: the NFT
// market and the DEX's pools and prices. Both go through nodecmd's Extender
// seam, the same one anything out of tree would use, so a node built without
// them carries neither their routes nor their tables.
func runNode(args []string) error {
	return nodecmd.Run(args, &nodecmd.Market{}, &nodecmd.Markets{})
}
