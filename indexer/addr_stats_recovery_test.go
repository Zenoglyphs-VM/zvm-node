package indexer

import "testing"

func TestAddrStatsRebuildCountsEachTransactionOncePerAddress(t *testing.T) {
	_, db := newReorgIndex(t, newFakeChain())
	tx, err := db.SQL().Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for i, parties := range [][3]string{{"a", "a", "a"}, {"a", "a", ""}, {"a", "b", "b"}, {"a", "b", "c"}} {
		var contract any
		if parties[2] != "" {
			contract = parties[2]
		}
		if _, err := tx.Exec(`INSERT INTO txs(hash,block,idx,timestamp,type,lane,from_addr,to_addr,value,gas,gas_used,gas_price,fee,nonce,status,input_size,contract)
			VALUES(?,1,?,1,126,'native',?,?,'0',1,1,'0','0',0,1,0,?)`, i, i, parties[0], parties[1], contract); err != nil {
			t.Fatal(err)
		}
	}
	if err := rebuildAddrStats(tx); err != nil {
		t.Fatal(err)
	}
	for address, want := range map[string]int{"a": 4, "b": 2, "c": 1} {
		var got int
		if err := tx.QueryRow("SELECT txs FROM addr_stats WHERE addr=?", address).Scan(&got); err != nil || got != want {
			t.Fatalf("rebuilt count for %s = %d, want %d: %v", address, got, want, err)
		}
	}
}
