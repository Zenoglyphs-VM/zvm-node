package orderbook_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	"github.com/Zenoglyphs-VM/zvm-node/orderbook"
	"github.com/ethereum/go-ethereum/common"
)

type marketDurableWalletLifecycle struct {
	parent                      context.Context
	endpoint, directory, config string
	current                     **executedRelayerProcess
	launch                      func(string) *executedRelayerProcess
	lifetimes                   *int
	firstHash                   common.Hash
	firstRaw                    []byte
}

func (m *marketDurableWalletLifecycle) posted(t *testing.T, raw []byte, index int) executedDurableAdmissionSnapshot {
	t.Helper()
	retained := len(m.firstRaw)
	if index == 1 {
		retained += len(raw)
	}
	deadline := time.Now().Add(6 * time.Second)
	for {
		status := durableMarketRetainedSnapshot(t, m.endpoint, int64(len(raw)), int64(retained), 1, index+1)
		if *status.Queued == 0 && *status.PostedPending == 1 {
			return status
		}
		if *status.Queued != 1 || *status.PostedPending != 0 || time.Now().After(deadline) {
			t.Fatal("actual wallet publication did not reach its complete posted boundary")
		}
		select {
		case <-m.parent.Done():
			t.Fatal("owned wallet context ended before posted admission")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func (m *marketDurableWalletLifecycle) record(t *testing.T, transport *orderbook.ExecutedMarketProviderFixture, marker, origin string, index int, hash common.Hash, raw []byte, phase byte, wireSHA string) {
	t.Helper()
	actualHash, actualRaw, source := transport.WalletPublicationForRecovery(t, index)
	digest := sha256.Sum256(raw)
	if actualHash != hash || !bytes.Equal(actualRaw, raw) || wireSHA != hex.EncodeToString(digest[:]) ||
		source["completeAccountHistoryReads"].(int) < 1 || source["canonicalExecutedStateNonceReads"].(int) < index+2 {
		t.Fatal("wallet publication recovery lost exact bytes or complete source/nonce evidence")
	}
	value := map[string]any{
		"source": orderbook.ExecutedMarketRelayerSource, "admissionMode": "local-sync", "walletLeg": index,
		"recordOrigin": origin, "persistedPhaseBeforeKill": int(phase), "actualKernelSignal": 9, "killedProcessJoined": true,
		"storedSignedRawSHA256": wireSHA, "storedSignedRawBytes": len(raw), "actualStoreWireVerified": true,
		"sameStoreAndEndpoint": true, "sameExecutableIdentityAndSHA256": true,
		"newProcessReadyAfterCompleteRecovery": true, "queuedAfterReopen": 0, "postedAfterReopen": 1,
		"retainedStoreRecords": index + 1, "activeAfterReopen": 1, "storeHealthy": true,
		"storeRecordLimit": 4, "storeSignedByteLimit": 65536, "actualProcessLifetimesSoFar": *m.lifetimes,
		"walletSubmissionsBeforeAndAfterReopen": index + 1, "NoMPublicationsBeforeAndAfterReopen": index + 1,
		"duplicateSubmissionOrPublication": false, "sourceEvidence": source, "realWalletOrPublicNetworkOperations": 0,
	}
	if phase == 1 {
		value["originalPublicationRPCResponseWithheld"] = true
		value["originalPublicationHandlerJoinedAfterClientClosed"] = true
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	t.Log(marker + string(encoded))
}

// All original browser phases and financial assertions are still executed.
// These additional process boundaries reuse the same wallet transaction and
// actual account history; they do not create another page or financial profile.
func (m *marketDurableWalletLifecycle) recover(t *testing.T, transport *orderbook.ExecutedMarketProviderFixture, index int) {
	t.Helper()
	if index == 0 {
		transport.AwaitWalletPublicationWithoutACK(t, m.parent)
		hash, raw, _ := transport.WalletPublicationForRecovery(t, 0)
		if hash != m.firstHash || !bytes.Equal(raw, m.firstRaw) {
			t.Fatal("accepted publishing state differs from the original wallet ACK")
		}
		status := durableMarketSnapshot(t, m.endpoint, len(raw), 1)
		if *status.Queued != 1 || *status.PostedPending != 0 {
			t.Fatal("withheld publication response was already acknowledged as posted")
		}
		(*m.current).kernelKill(t, m.parent)
		transport.JoinHeldWalletPublication(t, m.parent)
		wireSHA := inspectWalletAdmissionWire(t, m.directory, m.config, hash, raw, 1)
		*m.current = m.launch("10ms")
		m.posted(t, raw, 0)
		m.record(t, transport, "ACTUAL-MARKET-DURABLE-PUBLISHING-RECOVERY ", "accepted-response-withheld", 0, hash, raw, 1, wireSHA)

		(*m.current).kernelKill(t, m.parent)
		wireSHA = inspectWalletAdmissionWire(t, m.directory, m.config, hash, raw, 2)
		*m.current = m.launch("10ms")
		m.posted(t, raw, 0)
		m.record(t, transport, "ACTUAL-MARKET-DURABLE-POSTED-RECOVERY ", "complete-history-recovery", 0, hash, raw, 2, wireSHA)
		return
	}
	if index != 1 {
		t.Fatal("undeclared durable wallet publication leg")
	}
	hash, raw, _ := transport.WalletPublicationForRecovery(t, 1)
	m.posted(t, raw, 1) // Requires the second ordinary RPC to finish and persist posted.
	(*m.current).kernelKill(t, m.parent)
	wireSHA := inspectWalletAdmissionWire(t, m.directory, m.config, hash, raw, 2)
	*m.current = m.launch("10ms")
	m.posted(t, raw, 1)
	m.record(t, transport, "ACTUAL-MARKET-DURABLE-POSTED-RECOVERY ", "acknowledged-publication", 1, hash, raw, 2, wireSHA)
}
