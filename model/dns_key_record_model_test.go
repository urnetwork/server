//go:build sidecar_pending

package model

// POST /dns/key-record's write and the TLSA removal effect (dns_key_record_model.go; DNS-M7-02, §22's member-scope
// key-record check, FS-41 and FS-42's M7 dns step): a receiver's attested key publishes TLSA 3 1 1 at _25._tcp.mx
// with its mail_host_key row; a key that is not the caller's is refused with nothing written; a re-registration
// after a restore writes nothing new; a removal deletes the receiver's TLSA in the deactivation's own transaction, and
// the effect run twice is one result.

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"

	"github.com/miekg/dns"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"

	"github.com/urnetwork/server"
)

// A receiver: an active sidecar of the network with its identity key, and a P-256 key it holds.
type testDnsReceiver struct {
	sidecarId   server.Id
	identityKey ed25519.PrivateKey
	key         *ecdsa.PrivateKey
	spki        []byte
}

func newTestDnsReceiver(t testing.TB, ctx context.Context, networkId server.Id) *testDnsReceiver {
	t.Helper()
	publicKey, identityKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	spki, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	receiver := &testDnsReceiver{sidecarId: server.NewId(), identityKey: identityKey, key: key, spki: spki}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO sidecar_identity (sidecar_id, network_id, identity_public_key, identity_alg, status, create_time)
				VALUES ($1, $2, $3, $4, $5, $6)
			`,
			receiver.sidecarId,
			networkId,
			[]byte(publicKey),
			SidecarIdentityAlgEd25519,
			SidecarIdentityStatusActive,
			server.NowUtc(),
		))
	})
	return receiver
}

// The receiver's attestation of spki for the network's own mx in root, signed by its identity key.
func (self *testDnsReceiver) attest(t testing.TB, networkId server.Id, root string, label string, spki []byte) []byte {
	t.Helper()
	identifierSet, err := sidecar.TlsIdentifierSet(strings.TrimSuffix(root, "."), label, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	setSha256, err := sidecar.TlsIdentifierSetHash(identifierSet)
	if err != nil {
		t.Fatal(err)
	}
	spkiSha256 := sha256.Sum256(spki)
	attestation := &protocol.TlsKeyAttestation{
		NetworkId:           networkId.Bytes(),
		SidecarId:           self.sidecarId.Bytes(),
		SpkiSha256:          spkiSha256[:],
		IdentifierSetSha256: setSha256[:],
		IssueTimeMillis:     uint64(server.NowUtc().UnixMilli()),
	}
	signed, err := sidecar.SignTlsKeyAttestation(attestation, func(purpose string, signedType bridge.SignedType, body []byte, agentId *sidecar.Id) (*bridge.Signed, error) {
		return &bridge.Signed{
			Body:      body,
			SignerId:  self.sidecarId.Bytes(),
			Signature: ed25519.Sign(self.identityKey, bridge.SignedBytes(purpose, signedType, body)),
			Type:      uint32(signedType),
		}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

// The TLSA values at the network's mx in root.
func testDnsTlsaValues(t testing.TB, ctx context.Context, root string, networkId server.Id, apex string) []string {
	t.Helper()
	values := []string{}
	server.Db(ctx, func(conn server.PgConn) {
		for _, row := range getDnsZoneRowsInTx(ctx, conn, root, networkId) {
			if row.set.Owner == "_25._tcp.mx."+apex && row.set.Type == dns.TypeTLSA && !row.withheld {
				values = append(values, row.set.Rdata...)
			}
		}
	})
	return values
}

// A receiver's attested key publishes its TLSA 3 1 1 and its row; posting it again, as a restore's re-registration
// does (FS-41), writes nothing new; a key the attestation does not cover is refused with nothing written.
func TestDnsKeyRecordPublishesTheCallersKeyOnly(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := testDnsRoot(t, ctx)
		networkId, apex := testDnsNetwork(t, ctx, root)
		label := strings.TrimSuffix(apex, "."+root)
		receiver := newTestDnsReceiver(t, ctx, networkId)
		record := &DnsKeyRecord{Root: root, NetworkId: networkId, ReceiverSidecarId: receiver.sidecarId, Request: &sidecar.DnsKeyRecordRequest{
			Host: "mx", Spki: receiver.spki, Attestation: receiver.attest(t, networkId, root, label, receiver.spki),
		}}
		answer, err := PublishDnsKeyRecord(ctx, record)
		if err != nil {
			t.Fatal(err)
		}
		spkiSha256 := sha256.Sum256(receiver.spki)
		want := "3 1 1 " + strings.ToUpper(hex.EncodeToString(spkiSha256[:]))
		if values := testDnsTlsaValues(t, ctx, root, networkId, apex); len(values) != 1 || values[0] != want {
			t.Fatalf("tlsa %v, want %s", values, want)
		}
		again, err := PublishDnsKeyRecord(ctx, record)
		if err != nil || again.Version != answer.Version {
			t.Fatalf("a re-registration: %+v %v (first %d)", again, err, answer.Version)
		}

		other, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		otherSpki, _ := x509.MarshalPKIXPublicKey(&other.PublicKey)
		notTheCallers := &DnsKeyRecord{Root: root, NetworkId: networkId, ReceiverSidecarId: receiver.sidecarId, Request: &sidecar.DnsKeyRecordRequest{
			Host: "mx", Spki: otherSpki, Attestation: receiver.attest(t, networkId, root, label, receiver.spki),
		}}
		_, err = PublishDnsKeyRecord(ctx, notTheCallers)
		if apiError := testDnsApiError(t, err); apiError.Status != http.StatusForbidden {
			t.Fatalf("a key that is not the caller's: %+v", apiError)
		}
		stranger := newTestDnsReceiver(t, ctx, server.NewId())
		bySomeoneElse := &DnsKeyRecord{Root: root, NetworkId: networkId, ReceiverSidecarId: stranger.sidecarId, Request: &sidecar.DnsKeyRecordRequest{
			Host: "mx", Spki: stranger.spki, Attestation: stranger.attest(t, networkId, root, label, stranger.spki),
		}}
		_, err = PublishDnsKeyRecord(ctx, bySomeoneElse)
		if apiError := testDnsApiError(t, err); apiError.Status != http.StatusForbidden {
			t.Fatalf("another network's sidecar: %+v", apiError)
		}
		if values := testDnsTlsaValues(t, ctx, root, networkId, apex); len(values) != 1 || values[0] != want {
			t.Fatalf("a refusal wrote: %v", values)
		}
	})
}

// A removal of the receiver deletes its TLSA in the removal's own transaction, keeping another receiver's; the
// effect run again, as §19.10's replay of a removal does (FS-42), writes nothing.
func TestDnsTlsaRemovalEffectIsOneResult(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := testDnsRoot(t, ctx)
		networkId, apex := testDnsNetwork(t, ctx, root)
		label := strings.TrimSuffix(apex, "."+root)
		removed := newTestDnsReceiver(t, ctx, networkId)
		kept := newTestDnsReceiver(t, ctx, networkId)
		for _, receiver := range []*testDnsReceiver{removed, kept} {
			if _, err := PublishDnsKeyRecord(ctx, &DnsKeyRecord{Root: root, NetworkId: networkId, ReceiverSidecarId: receiver.sidecarId, Request: &sidecar.DnsKeyRecordRequest{
				Host: "mx", Spki: receiver.spki, Attestation: receiver.attest(t, networkId, root, label, receiver.spki),
			}}); err != nil {
				t.Fatal(err)
			}
		}
		if values := testDnsTlsaValues(t, ctx, root, networkId, apex); len(values) != 2 {
			t.Fatalf("tlsa %v", values)
		}
		removal := &Removal{Subjects: []*RemovalSubject{{NetworkId: networkId, Kind: sidecar.RevocationKindSidecar, SubjectId: removed.sidecarId}}, Time: server.NowUtc()}
		apply := func() {
			err := dnsCatchRefusal(func() {
				server.Tx(ctx, func(tx server.PgTx) {
					if err := applyDnsTlsaRemovalEffectInTx(ctx, tx, removal); err != nil {
						panic(dnsWriteRefusal{err: err})
					}
				}, server.TxReadCommitted)
			})
			if err != nil {
				t.Fatal(err)
			}
		}
		apply()
		keptSha256 := sha256.Sum256(kept.spki)
		want := "3 1 1 " + strings.ToUpper(hex.EncodeToString(keptSha256[:]))
		if values := testDnsTlsaValues(t, ctx, root, networkId, apex); len(values) != 1 || values[0] != want {
			t.Fatalf("after the removal %v, want %s", values, want)
		}
		state, err := GetDnsRootState(ctx, root)
		if err != nil {
			t.Fatal(err)
		}
		apply()
		after, err := GetDnsRootState(ctx, root)
		if err != nil || after.Serial != state.Serial {
			t.Fatalf("the effect again wrote: %d, then %d", state.Serial, after.Serial)
		}
	})
}
