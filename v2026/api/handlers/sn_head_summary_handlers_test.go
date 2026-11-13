// POST /sn/head-summary over real HTTP with a real client JWT (sn
// docs/spec/head-summary-v1.md, Transport): 401 without a session, 400 for a
// transformed body, 413 past 16 MiB, 403 for a validly signed summary of an
// unaccepted hotkey, and 204 for a stored summary and its replay; and sn's own
// golden vector is stored through the same route. Keys and identities are
// synthetic.
package handlers

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	subkey "github.com/vedhavyas/go-subkey/v2"
	"github.com/vedhavyas/go-subkey/v2/sr25519"

	"github.com/urfoundation/sn/v2026/protocol/headscore"
	"github.com/urfoundation/sn/v2026/protocol/headsummary"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
)

// A synthetic validator key and its signed canonical summary body.
func snHeadSummaryHandlerTestBody(t testing.TB) ([32]byte, []byte) {
	t.Helper()
	var seed [32]byte
	rand.Read(seed[:])
	keypair, err := subkey.DeriveKeyPair(sr25519.Scheme{}, "0x"+hex.EncodeToString(seed[:]))
	if err != nil {
		t.Fatal(err)
	}
	hotkey := [32]byte(keypair.Public())
	hash := func(b byte) string { return "0x" + strings.Repeat(hex.EncodeToString([]byte{b}), 32) }
	summary := &headsummary.Summary{
		Schema: headsummary.Schema, Netuid: 25, NativeEpoch: 900, NativeSnapshotBlock: 7_000_000, NativeSnapshotHash: hash(0x6d),
		EVMSnapshotBlock: 6_999_990, EVMSnapshotHash: hash(0x2a), SettlementEpoch: 7, PolicyID: 1, PolicyHash: hash(0x9c),
		MeasurementArtifactHash: "sha256:" + strings.Repeat("41", 32), ValidatorHotkey: headsummary.Hex32(hotkey), ValidatorUID: 1, MaximumHeadFleets: 200,
		Entries: []headsummary.Entry{{Rank: 1, UID: 42, Hotkey: hash(0x8f), Score: headscore.Rational{Numerator: "3", Denominator: "2"}, Selected: true,
			Fleets: []headsummary.Fleet{{FleetID: hash(0x11), Generation: 1}}}},
		SelectedUIDs: []uint16{42}, RejectedUIDs: []uint16{},
	}
	body, err := headsummary.Sign(summary, hotkey, time.Date(2026, 10, 29, 18, 4, 5, 0, time.UTC), keypair.Sign)
	if err != nil {
		t.Fatal(err)
	}
	return hotkey, body
}

// Posts a body to the route over real HTTP and returns the status.
func snHeadSummaryHandlerTestPost(tb testing.TB, endpoint string, token string, contentType string, data []byte) int {
	tb.Helper()
	request, err := http.NewRequest(http.MethodPost, endpoint+headsummary.Path, bytes.NewReader(data))
	if err != nil {
		tb.Fatal(err)
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	request.Header.Set("Content-Type", contentType)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		tb.Fatal(err)
	}
	defer response.Body.Close()
	return response.StatusCode
}

func TestSnUploadHeadSummaryOverHttp(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		_, client := snAttemptUploadTestIdentity(tb)
		hotkey, body := snHeadSummaryHandlerTestBody(tb)
		_, otherBody := snHeadSummaryHandlerTestBody(tb)
		controller.SetStConfig(&controller.StConfig{ChainId: 945, ContractAddress: common.HexToAddress("0x2000000000000000000000000000000000000028"), Netuid: 25, ValidatorHotkeys: [][32]byte{hotkey}})
		defer controller.SetStConfig(nil)
		endpoint := httptest.NewServer(http.HandlerFunc(SnUploadHeadSummary))
		defer endpoint.Close()
		post := func(token string, contentType string, data []byte) int {
			return snHeadSummaryHandlerTestPost(tb, endpoint.URL, token, contentType, data)
		}
		token := client.Testing_Sign()
		if status := post("", "application/json", body); status != http.StatusUnauthorized {
			tb.Fatalf("without a session: %d", status)
		}
		if status := post(token, "text/plain", body); status != http.StatusBadRequest {
			tb.Fatalf("a transformed body: %d", status)
		}
		if status := post(token, "application/json", append([]byte(" "), body...)); status != http.StatusBadRequest {
			tb.Fatalf("a non-canonical body: %d", status)
		}
		if status := post(token, "application/json", make([]byte, controller.SnHeadSummaryMaxBytes+1)); status != http.StatusRequestEntityTooLarge {
			tb.Fatalf("past 16 MiB: %d", status)
		}
		if status := post(token, "application/json", otherBody); status != http.StatusForbidden {
			tb.Fatalf("an unaccepted hotkey: %d", status)
		}
		if status := post(token, "application/json", body); status != http.StatusNoContent {
			tb.Fatalf("a stored summary: %d", status)
		}
		if status := post(token, "application/json", body); status != http.StatusNoContent {
			tb.Fatalf("a replay: %d", status)
		}
		stored := model.GetLatestSnHeadSummary(context.Background(), 25, &hotkey)
		if stored == nil || stored.NativeEpoch != 900 || !bytes.Equal(stored.Body, body) || len(stored.Fleets) != 1 {
			tb.Fatal("stored summary", stored)
		}
	})
}

// sn's golden vector (protocol/testdata/head-summary-v1.json), the exact
// request body its reference implementation signs, is stored through the
// route for an operator that accepts its hotkey, with its own signing hash.
func TestSnUploadHeadSummaryStoresSnGoldenVector(t *testing.T) {
	encoded, err := os.ReadFile("../../../sn/protocol/testdata/head-summary-v1.json")
	if err != nil {
		t.Fatalf("sn's golden vector is unavailable: %v", err)
	}
	var vector struct {
		PublicKey   string `json:"public_key"`
		SigningHash string `json:"signing_hash"`
		Body        string `json:"body"`
	}
	if err := json.Unmarshal(encoded, &vector); err != nil {
		t.Fatal(err)
	}
	hotkey, err := headsummary.ParseHex32(vector.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		_, client := snAttemptUploadTestIdentity(tb)
		controller.SetStConfig(&controller.StConfig{ChainId: 945, ContractAddress: common.HexToAddress("0x2000000000000000000000000000000000000029"), Netuid: 25, ValidatorHotkeys: [][32]byte{hotkey}})
		defer controller.SetStConfig(nil)
		endpoint := httptest.NewServer(http.HandlerFunc(SnUploadHeadSummary))
		defer endpoint.Close()
		if status := snHeadSummaryHandlerTestPost(tb, endpoint.URL, client.Testing_Sign(), "application/json", []byte(vector.Body)); status != http.StatusNoContent {
			tb.Fatalf("sn's golden vector: %d", status)
		}
		stored := model.GetLatestSnHeadSummary(context.Background(), 25, &hotkey)
		if stored == nil || stored.SigningHash != vector.SigningHash || stored.ValidatorHotkey != hotkey || !bytes.Equal(stored.Body, []byte(vector.Body)) ||
			stored.EntryCount == 0 || len(stored.Fleets) < stored.EntryCount {
			tb.Fatal("the golden vector was not stored as sent", stored)
		}
	})
}
