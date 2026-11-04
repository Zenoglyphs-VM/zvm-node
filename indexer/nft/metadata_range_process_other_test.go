//go:build !linux

package nft

import "testing"

func metadataIndexProcessChild(t *testing.T) bool { return false }

func requireMetadataIndexProcessInterruption(t *testing.T) {
	t.Helper()
	t.Skip("the explicit SQLite process-interruption fixture requires Linux SIGKILL")
}
