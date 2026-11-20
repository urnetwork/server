//go:build linux

// Tests strict document handling without opening a database or lease.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/urnetwork/server/v2026/model"
)

// Even a matching digest cannot authorize ambiguous JSON.
func TestApprovedCorrectionRejectsAmbiguousJson(t *testing.T) {
	value := model.CompetitionEpochThreeRuntimeCorrection{Schema: 1, Kind: "test"}
	canonical, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	digest := func(content []byte) string {
		sum := sha256.Sum256(content)
		return hex.EncodeToString(sum[:])
	}
	if _, err := decodeApprovedCorrection(canonical, digest(canonical)); err != nil {
		t.Fatal(err)
	}
	for _, content := range [][]byte{
		[]byte(`{"schema":1,"schema":1,"kind":"test"}`),
		append(append([]byte(nil), canonical[:len(canonical)-1]...), []byte(`,"unknown":"test"}`)...),
	} {
		if _, err := decodeApprovedCorrection(content, digest(content)); err == nil {
			t.Fatal("accepted ambiguous approved JSON")
		}
	}
}
