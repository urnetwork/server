//go:build sidecar_pending

package model

// §12.5's CT replay and label reclaim (dns_reclaim_model.go; DNS-M5-16, FS-41, FS-42's labels step): the replay
// inserts the names dns_label lacks and leaves a held one alone, rerunning to one result; a logged leaf's key re-binds
// its ownerless name and nothing else does, a wrong key, a wrong nonce, a bound name, an unlogged leaf or another
// network's tuple each refused with nothing written; an ownerless name resolves to nothing and holds its name from a
// rename; and names nobody re-binds tombstone after 90 days on a moved clock.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
)

// A P-256 key and a leaf on it naming names.
func testDnsReclaimLeaf(t testing.TB, names ...string) (*ecdsa.PrivateKey, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: names[0]},
		DNSNames:     names,
		NotBefore:    time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		NotAfter:     time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
	}
	leaf, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return key, leaf
}

// The request reclaiming name for networkId under incidentId, signed by key.
func testDnsReclaimRequest(t testing.TB, key *ecdsa.PrivateKey, leaf []byte, name string, networkId server.Id, incidentId server.Id) *sidecar.DnsLabelReclaimRequest {
	t.Helper()
	proof := &sidecar.DnsReclaimProof{Name: name, NetworkId: sidecar.Id(networkId), IncidentId: sidecar.Id(incidentId)}
	signature, err := proof.Sign(key)
	if err != nil {
		t.Fatal(err)
	}
	return &sidecar.DnsLabelReclaimRequest{Name: name, NetworkId: sidecar.Id(networkId), IncidentId: sidecar.Id(incidentId), Leaf: leaf, Signature: signature}
}

// A root's name for label, as CT logs it.
func testDnsCtName(root string, label string) string {
	return label + "." + strings.TrimSuffix(root, ".")
}

// The replay inserts each label dns_label lacks as ownerless and retiring with every logged leaf, leaves a held one
// as it is with no leaf recorded, ignores names outside the root, and a rerun inserts nothing more.
func TestDnsCtReplayInsertsOnlyMissingNames(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := testDnsRoot(t, ctx)
		_, heldApex := testDnsNetwork(t, ctx, root)
		heldLabel := strings.TrimSuffix(heldApex, "."+root)
		incidentId := server.NewId()
		_, firstLeaf := testDnsReclaimLeaf(t, testDnsCtName(root, "lost00001"))
		_, secondLeaf := testDnsReclaimLeaf(t, "*."+testDnsCtName(root, "lost00001"))
		_, heldLeaf := testDnsReclaimLeaf(t, testDnsCtName(root, heldLabel))
		names := []*DnsCtName{
			{Name: testDnsCtName(root, "lost00001"), LeafSha256: sidecar.DnsReclaimLeafSha256(firstLeaf)},
			{Name: "*." + testDnsCtName(root, "lost00001"), LeafSha256: sidecar.DnsReclaimLeafSha256(secondLeaf)},
			{Name: testDnsCtName(root, heldLabel), LeafSha256: sidecar.DnsReclaimLeafSha256(heldLeaf)},
			{Name: "elsewhere.example", LeafSha256: sidecar.DnsReclaimLeafSha256(heldLeaf)},
		}
		for run, want := range []int{1, 0} {
			inserted, err := ReplayDnsCtNames(ctx, root, incidentId, names, server.NowUtc())
			if err != nil || inserted != want {
				t.Fatalf("run %d: %d inserted, %v", run, inserted, err)
			}
		}
		recorded, err := GetDnsRestoreIncidentNames(ctx, root, incidentId)
		if err != nil || len(recorded) != 1 || recorded[0] != "lost00001" {
			t.Fatalf("recorded %v %v", recorded, err)
		}
		if _, _, err := ResolveNetworkLabel(ctx, root, "lost00001", true); !errors.Is(err, sidecar.ErrNetworkLabelUnknown) {
			t.Fatalf("an ownerless name resolved: %v", err)
		}
		held := false
		server.Db(ctx, func(conn server.PgConn) {
			held = DnsNameHeldInTx(ctx, conn, "lost00001")
		})
		if !held {
			t.Fatalf("an ownerless name holds nothing from a rename")
		}
	})
}

// A logged leaf's key re-binds the ownerless name to its network as its label, the network then resolving it and
// its zone made; a second reclaim of the bound name is refused.
func TestDnsLabelReclaimRebindsTheName(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := testDnsRoot(t, ctx)
		networkId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "dnsr-"+networkId.String()[24:], server.NewId())
		incidentId := server.NewId()
		name := testDnsCtName(root, "lost00002")
		key, leaf := testDnsReclaimLeaf(t, name, "*."+name)
		if _, err := ReplayDnsCtNames(ctx, root, incidentId, []*DnsCtName{{Name: name, LeafSha256: sidecar.DnsReclaimLeafSha256(leaf)}}, server.NowUtc()); err != nil {
			t.Fatal(err)
		}
		reclaim := &DnsLabelReclaim{Root: root, NetworkId: networkId, Request: testDnsReclaimRequest(t, key, leaf, name, networkId, incidentId), ActorKind: AdminEventActorSystem}
		answer, err := ReclaimDnsLabel(ctx, reclaim)
		if err != nil || answer.Kind != DnsLabelKindLabel || answer.Name != name {
			t.Fatalf("the reclaim: %+v %v", answer, err)
		}
		resolved, kind, err := ResolveNetworkLabel(ctx, root, "lost00002", false)
		if err != nil || resolved != networkId || kind != sidecar.NetworkLabelKindLabel {
			t.Fatalf("resolved %v %s %v", resolved, kind, err)
		}
		if GetNetworkLabel(ctx, networkId, root) != "lost00002" {
			t.Fatalf("the network's label")
		}
		_, err = ReclaimDnsLabel(ctx, reclaim)
		if apiError := testDnsApiError(t, err); apiError.Status != http.StatusConflict {
			t.Fatalf("a second reclaim: %+v", apiError)
		}
	})
}

// A wrong key, a wrong nonce, a bound name, an unlogged leaf and another network's tuple are each refused, and the
// name stays ownerless.
func TestDnsLabelReclaimRefusals(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := testDnsRoot(t, ctx)
		boundNetworkId, boundApex := testDnsNetwork(t, ctx, root)
		networkId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "dnsr-"+networkId.String()[24:], server.NewId())
		incidentId := server.NewId()
		name := testDnsCtName(root, "lost00003")
		key, leaf := testDnsReclaimLeaf(t, name)
		otherKey, _ := testDnsReclaimLeaf(t, name)
		unloggedKey, unloggedLeaf := testDnsReclaimLeaf(t, name)
		boundName := strings.TrimSuffix(boundApex, ".")
		boundKey, boundLeaf := testDnsReclaimLeaf(t, boundName)
		if _, err := ReplayDnsCtNames(ctx, root, incidentId, []*DnsCtName{
			{Name: name, LeafSha256: sidecar.DnsReclaimLeafSha256(leaf)},
			{Name: boundName, LeafSha256: sidecar.DnsReclaimLeafSha256(boundLeaf)},
		}, server.NowUtc()); err != nil {
			t.Fatal(err)
		}
		wrongKey := testDnsReclaimRequest(t, otherKey, leaf, name, networkId, incidentId)
		wrongNonce := testDnsReclaimRequest(t, key, leaf, name, networkId, server.NewId())
		wrongNonce.IncidentId = sidecar.Id(incidentId)
		cases := []struct {
			name      string
			networkId server.Id
			request   *sidecar.DnsLabelReclaimRequest
			status    int
		}{
			{name: "a wrong key", networkId: networkId, request: wrongKey, status: http.StatusForbidden},
			{name: "a wrong nonce", networkId: networkId, request: wrongNonce, status: http.StatusForbidden},
			{name: "a bound name", networkId: networkId, request: testDnsReclaimRequest(t, boundKey, boundLeaf, boundName, networkId, incidentId), status: http.StatusConflict},
			{name: "an unlogged leaf", networkId: networkId, request: testDnsReclaimRequest(t, unloggedKey, unloggedLeaf, name, networkId, incidentId), status: http.StatusForbidden},
			{name: "another network's tuple", networkId: boundNetworkId, request: testDnsReclaimRequest(t, key, leaf, name, networkId, incidentId), status: http.StatusForbidden},
		}
		for _, c := range cases {
			_, err := ReclaimDnsLabel(ctx, &DnsLabelReclaim{Root: root, NetworkId: c.networkId, Request: c.request, ActorKind: AdminEventActorSystem})
			if apiError := testDnsApiError(t, err); apiError.Status != c.status {
				t.Errorf("%s: %+v, want %d", c.name, apiError, c.status)
			}
		}
		if _, _, err := ResolveNetworkLabel(ctx, root, "lost00003", true); !errors.Is(err, sidecar.ErrNetworkLabelUnknown) {
			t.Fatalf("a refused reclaim bound the name: %v", err)
		}
	})
}

// Ownerless names tombstone once their 90 days have passed, not before, and a tombstone still holds its name.
func TestDnsUnclaimedLabelsTombstoneAfter90Days(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := testDnsRoot(t, ctx)
		now := server.NowUtc()
		_, leaf := testDnsReclaimLeaf(t, testDnsCtName(root, "lost00004"))
		if _, err := ReplayDnsCtNames(ctx, root, server.NewId(), []*DnsCtName{{Name: testDnsCtName(root, "lost00004"), LeafSha256: sidecar.DnsReclaimLeafSha256(leaf)}}, now); err != nil {
			t.Fatal(err)
		}
		state := func() string {
			state := ""
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT state FROM dns_label WHERE root = $1 AND label = $2`, root, "lost00004").Scan(&state))
			})
			return state
		}
		if _, _, err := TombstoneUnclaimedDnsLabels(ctx, now.Add(89*24*time.Hour)); err != nil || state() != DnsLabelStateRetiring {
			t.Fatalf("before 90 days: %s %v", state(), err)
		}
		if _, _, err := TombstoneUnclaimedDnsLabels(ctx, now.Add(91*24*time.Hour)); err != nil || state() != DnsLabelStateTombstoned {
			t.Fatalf("after 90 days: %s %v", state(), err)
		}
		held := false
		server.Db(ctx, func(conn server.PgConn) {
			held = DnsNameHeldInTx(ctx, conn, "lost00004")
		})
		if !held {
			t.Fatalf("a tombstone holds nothing")
		}
	})
}
