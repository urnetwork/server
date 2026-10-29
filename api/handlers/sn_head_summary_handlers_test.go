// POST /sn/head-summary over real HTTP with a real client JWT (sn
// docs/spec/head-summary-v1.md, Transport): 401 without a session, 400 for a
// transformed body, 413 past 16 MiB, 403 for a validly signed summary of an
// unaccepted hotkey, and 204 for a stored summary and its replay. Keys and
// identities are synthetic.
package handlers

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ChainSafe/go-schnorrkel"
	"github.com/ethereum/go-ethereum/common"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// A synthetic validator key and its signed canonical summary body.
func snHeadSummaryHandlerTestBody(t testing.TB) ([32]byte, []byte) {
	t.Helper()
	var seed [32]byte
	rand.Read(seed[:])
	mini, err := schnorrkel.NewMiniSecretKeyFromRaw(seed)
	if err != nil {
		t.Fatal(err)
	}
	secret := mini.ExpandEd25519()
	public, err := secret.Public()
	if err != nil {
		t.Fatal(err)
	}
	hotkey := public.Encode()
	hash := func(b byte) string { return "0x" + strings.Repeat(hex.EncodeToString([]byte{b}), 32) }
	summary := &controller.SnHeadSummary{
		Schema: controller.SnHeadSummarySchemaV1, Netuid: 25, NativeEpoch: 900, NativeSnapshotBlock: 7_000_000, NativeSnapshotHash: hash(0x6d),
		EvmSnapshotBlock: 6_999_990, EvmSnapshotHash: hash(0x2a), SettlementEpoch: 7, PolicyId: 1, PolicyHash: hash(0x9c),
		MeasurementArtifactHash: "sha256:" + strings.Repeat("41", 32), ValidatorHotkey: "0x" + hex.EncodeToString(hotkey[:]), ValidatorUid: 1, MaximumHeadFleets: 200,
		Entries: []controller.SnHeadSummaryEntry{{Rank: 1, Uid: 42, Hotkey: hash(0x8f), Score: controller.SnHeadSummaryRational{Numerator: "3", Denominator: "2"}, Selected: true,
			Fleets: []controller.SnHeadSummaryFleet{{FleetId: hash(0x11), Generation: 1}}}},
		SelectedUids: []uint16{42}, RejectedUids: []uint16{}, SignedAt: "2026-10-29T18:04:05Z", SignatureScheme: "sr25519",
	}
	digest := controller.SnHeadSummaryDigest(summary)
	signature, err := secret.Sign(schnorrkel.NewSigningContext([]byte("substrate"), digest[:]))
	if err != nil {
		t.Fatal(err)
	}
	encoded := signature.Encode()
	summary.SigningHash, summary.Signature = "sha256:"+hex.EncodeToString(digest[:]), "0x"+hex.EncodeToString(encoded[:])
	body, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	return hotkey, body
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
			request, err := http.NewRequest(http.MethodPost, endpoint.URL+"/sn/head-summary", bytes.NewReader(data))
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
