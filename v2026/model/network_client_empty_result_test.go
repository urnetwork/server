package model

import (
	"encoding/json"
	"testing"

	"github.com/urnetwork/server/v2026"
)

// An account with no active clients must cross the API boundary as an empty
// JSON array. Encoding null leaves generated pointer-list clients vulnerable
// to a nil dereference even though the request itself succeeded.
func TestEmptyNetworkClientsResultEncodesArray(t *testing.T) {
	result := newNetworkClientsResult(map[server.Id]*NetworkClientInfo{})
	responseJson, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if string(responseJson) != `{"clients":[]}` {
		t.Fatalf("empty network clients response = %s, expected clients array", responseJson)
	}
}
