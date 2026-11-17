//go:build sidecar_pending

package model

// POST /dns/key-record's write and the TLSA removal effect (dns_key_record_model.go; DNS-M7-02, §22's member-scope
// key-record check, FS-41 and FS-42's M7 dns step): a receiver's attested key publishes TLSA 3 1 1 at _25._tcp.mx
// with its mail_host_key row; a key that is not the caller's is refused with nothing written; a re-registration
// after a restore writes nothing new; a removal deletes the receiver's TLSA in the deactivation's own transaction, and
// the effect run twice is one result.
//
// A rotation's retirement (owner decision 443): the new key posted again with the old key's hash in
// retire_spki_sha256 leaves exactly the new key's TLSA; a hash naming another receiver's key, or no key, retires
// nothing; a repeat without the list changes nothing; a malformed list is refused with nothing written; a removal
// after rotations still retires every key of the receiver (I-15); and a write that waited on a removal of its
// receiver is refused, publishing nothing.

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
	"slices"
	"strings"
	"sync"
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

// A fresh P-256 key's DER SubjectPublicKeyInfo: a rotation's successor, or a key no receiver published.
func newTestDnsReceiverSpki(t testing.TB) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	spki, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return spki
}

// The receiver's write of spki at the network's own mx, attested by its identity key, retiring the keys whose SPKIs
// retire lists.
func (self *testDnsReceiver) record(t testing.TB, networkId server.Id, root string, label string, spki []byte, retire ...[]byte) *DnsKeyRecord {
	t.Helper()
	retireSpkiSha256s := [][]byte{}
	for _, retired := range retire {
		spkiSha256 := sha256.Sum256(retired)
		retireSpkiSha256s = append(retireSpkiSha256s, spkiSha256[:])
	}
	return &DnsKeyRecord{Root: root, NetworkId: networkId, ReceiverSidecarId: self.sidecarId, Request: &sidecar.DnsKeyRecordRequest{
		Host:              "mx",
		Spki:              spki,
		Attestation:       self.attest(t, networkId, root, label, spki),
		RetireSpkiSha256s: retireSpkiSha256s,
	}}
}

// The receiver's write of spki, retiring retire's keys, made.
func (self *testDnsReceiver) post(t testing.TB, ctx context.Context, networkId server.Id, root string, label string, spki []byte, retire ...[]byte) (*sidecar.DnsKeyRecordAnswer, error) {
	t.Helper()
	return PublishDnsKeyRecord(ctx, self.record(t, networkId, root, label, spki, retire...))
}

// TLSA 3 1 1's value for spki.
func testDnsTlsaValue(spki []byte) string {
	spkiSha256 := sha256.Sum256(spki)
	return "3 1 1 " + strings.ToUpper(hex.EncodeToString(spkiSha256[:]))
}

// Whether values hold exactly the TLSA of spkis, in any order.
func testDnsTlsaHolds(values []string, spkis ...[]byte) bool {
	want := []string{}
	for _, spki := range spkis {
		want = append(want, testDnsTlsaValue(spki))
	}
	got := slices.Clone(values)
	slices.Sort(got)
	slices.Sort(want)
	return slices.Equal(got, want)
}

// The network's host key rows by the hex of the SHA-256 they hold.
func testDnsMailHostKeys(ctx context.Context, networkId server.Id) map[string]*MailHostKey {
	keys := map[string]*MailHostKey{}
	for _, key := range ListMailHostKeys(ctx, networkId) {
		keys[hex.EncodeToString(key.SpkiSha256)] = key
	}
	return keys
}

// The hex of spki's SHA-256, as testDnsMailHostKeys keys a row.
func testDnsSpkiHex(spki []byte) string {
	spkiSha256 := sha256.Sum256(spki)
	return hex.EncodeToString(spkiSha256[:])
}

// The root's serial now.
func testDnsRootSerial(t testing.TB, ctx context.Context, root string) uint64 {
	t.Helper()
	state, err := GetDnsRootState(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	return state.Serial
}

// A rotation: the successor's TLSA goes out beside the old key's while it is pending, and once it serves its repeat
// with the old key's hash in the list leaves exactly its own TLSA, the old row retired, with one more admin event;
// the same repeat again, as after a lost answer, writes nothing.
func TestDnsKeyRecordRotationLeavesOnlyTheNewKeysTlsa(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := testDnsRoot(t, ctx)
		networkId, apex := testDnsNetwork(t, ctx, root)
		label := strings.TrimSuffix(apex, "."+root)
		receiver := newTestDnsReceiver(t, ctx, networkId)
		old := receiver.spki
		if _, err := receiver.post(t, ctx, networkId, root, label, old); err != nil {
			t.Fatal(err)
		}
		next := newTestDnsReceiverSpki(t)
		if _, err := receiver.post(t, ctx, networkId, root, label, next); err != nil {
			t.Fatal(err)
		}
		if values := testDnsTlsaValues(t, ctx, root, networkId, apex); !testDnsTlsaHolds(values, old, next) {
			t.Fatalf("while the successor is pending: %v", values)
		}
		events := len(testDnsAdminEventResults(ctx, networkId, AdminEventActionDnsKeyRecord))

		answer, err := receiver.post(t, ctx, networkId, root, label, next, old)
		if err != nil {
			t.Fatal(err)
		}
		if values := testDnsTlsaValues(t, ctx, root, networkId, apex); !testDnsTlsaHolds(values, next) {
			t.Fatalf("after the swap %v, want only %s", values, testDnsTlsaValue(next))
		}
		if serial := testDnsRootSerial(t, ctx, root); answer.Version != serial {
			t.Fatalf("answered version %d, the root at %d", answer.Version, serial)
		}
		keys := testDnsMailHostKeys(ctx, networkId)
		if key := keys[testDnsSpkiHex(old)]; key == nil || key.State != MailHostKeyStateRetired || key.RetireTime.IsZero() {
			t.Fatalf("the old key's row %+v", key)
		}
		if key := keys[testDnsSpkiHex(next)]; key == nil || key.State != MailHostKeyStatePublished {
			t.Fatalf("the new key's row %+v", key)
		}
		if after := len(testDnsAdminEventResults(ctx, networkId, AdminEventActionDnsKeyRecord)); after != events+1 {
			t.Fatalf("%d key-record events after the retirement, %d before", after, events)
		}

		again, err := receiver.post(t, ctx, networkId, root, label, next, old)
		if err != nil || again.Version != answer.Version || testDnsRootSerial(t, ctx, root) != answer.Version {
			t.Fatalf("the retirement again: %+v %v", again, err)
		}
		if key := testDnsMailHostKeys(ctx, networkId)[testDnsSpkiHex(old)]; key == nil || !key.RetireTime.Equal(keys[testDnsSpkiHex(old)].RetireTime) {
			t.Fatalf("the retirement again moved the old key's retire time: %+v", key)
		}
	})
}

// A list naming another receiver's published key, or a key nobody published, retires nothing: the write is the
// caller's repeat and takes no serial, and both receivers' TLSA and rows stay.
func TestDnsKeyRecordRetiresOnlyTheCallersOwnKeys(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := testDnsRoot(t, ctx)
		networkId, apex := testDnsNetwork(t, ctx, root)
		label := strings.TrimSuffix(apex, "."+root)
		caller := newTestDnsReceiver(t, ctx, networkId)
		other := newTestDnsReceiver(t, ctx, networkId)
		for _, receiver := range []*testDnsReceiver{caller, other} {
			if _, err := receiver.post(t, ctx, networkId, root, label, receiver.spki); err != nil {
				t.Fatal(err)
			}
		}
		serial := testDnsRootSerial(t, ctx, root)
		answer, err := caller.post(t, ctx, networkId, root, label, caller.spki, other.spki, newTestDnsReceiverSpki(t))
		if err != nil {
			t.Fatal(err)
		}
		if answer.Version != serial || testDnsRootSerial(t, ctx, root) != serial {
			t.Fatalf("a list of keys not the caller's wrote: version %d, serial %d then %d", answer.Version, serial, testDnsRootSerial(t, ctx, root))
		}
		if values := testDnsTlsaValues(t, ctx, root, networkId, apex); !testDnsTlsaHolds(values, caller.spki, other.spki) {
			t.Fatalf("tlsa %v", values)
		}
		keys := testDnsMailHostKeys(ctx, networkId)
		for _, spki := range [][]byte{caller.spki, other.spki} {
			if key := keys[testDnsSpkiHex(spki)]; key == nil || key.State != MailHostKeyStatePublished {
				t.Fatalf("a row %+v", key)
			}
		}
		if len(keys) != 2 {
			t.Fatalf("%d rows", len(keys))
		}
	})
}

// A rotation's successor posted again with no list, as a restore's re-registration posts it, keeps the old key's
// TLSA and takes no serial: only a listed hash retires.
func TestDnsKeyRecordRepeatWithoutTheListChangesNothing(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := testDnsRoot(t, ctx)
		networkId, apex := testDnsNetwork(t, ctx, root)
		label := strings.TrimSuffix(apex, "."+root)
		receiver := newTestDnsReceiver(t, ctx, networkId)
		next := newTestDnsReceiverSpki(t)
		for _, spki := range [][]byte{receiver.spki, next} {
			if _, err := receiver.post(t, ctx, networkId, root, label, spki); err != nil {
				t.Fatal(err)
			}
		}
		serial := testDnsRootSerial(t, ctx, root)
		answer, err := receiver.post(t, ctx, networkId, root, label, next)
		if err != nil || answer.Version != serial || testDnsRootSerial(t, ctx, root) != serial {
			t.Fatalf("a repeat with no list: %+v %v, serial %d", answer, err, serial)
		}
		if values := testDnsTlsaValues(t, ctx, root, networkId, apex); !testDnsTlsaHolds(values, receiver.spki, next) {
			t.Fatalf("tlsa %v", values)
		}
		for _, key := range testDnsMailHostKeys(ctx, networkId) {
			if key.State != MailHostKeyStatePublished {
				t.Fatalf("a row %+v", key)
			}
		}
	})
}

// A hash that is not 32 bytes, or one naming the key the write publishes, is refused 400 with nothing written, the
// new key's row and TLSA included.
func TestDnsKeyRecordRefusesAMalformedRetireList(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := testDnsRoot(t, ctx)
		networkId, apex := testDnsNetwork(t, ctx, root)
		label := strings.TrimSuffix(apex, "."+root)
		receiver := newTestDnsReceiver(t, ctx, networkId)
		if _, err := receiver.post(t, ctx, networkId, root, label, receiver.spki); err != nil {
			t.Fatal(err)
		}
		serial := testDnsRootSerial(t, ctx, root)
		next := newTestDnsReceiverSpki(t)
		oldSpkiSha256 := sha256.Sum256(receiver.spki)
		nextSpkiSha256 := sha256.Sum256(next)
		cases := []struct {
			name   string
			retire [][]byte
		}{
			{name: "a short hash", retire: [][]byte{oldSpkiSha256[:31]}},
			{name: "a long hash", retire: [][]byte{append(oldSpkiSha256[:], 0)}},
			{name: "an empty hash", retire: [][]byte{{}}},
			{name: "the key it publishes", retire: [][]byte{oldSpkiSha256[:], nextSpkiSha256[:]}},
		}
		for _, c := range cases {
			record := receiver.record(t, networkId, root, label, next)
			record.Request.RetireSpkiSha256s = c.retire
			_, err := PublishDnsKeyRecord(ctx, record)
			if apiError := testDnsApiError(t, err); apiError.Status != http.StatusBadRequest || apiError.Code != sidecar.DnsApiCodeInvalid {
				t.Errorf("%s: %+v", c.name, apiError)
			}
		}
		if testDnsRootSerial(t, ctx, root) != serial {
			t.Fatal("a refused list wrote")
		}
		if values := testDnsTlsaValues(t, ctx, root, networkId, apex); !testDnsTlsaHolds(values, receiver.spki) {
			t.Fatalf("tlsa %v", values)
		}
		if keys := testDnsMailHostKeys(ctx, networkId); len(keys) != 1 || keys[testDnsSpkiHex(receiver.spki)].State != MailHostKeyStatePublished {
			t.Fatalf("rows %v", keys)
		}
	})
}

// After rotations, a removal of the receiver, with another receiver in the same removal, still retires every key of
// both and drops their TLSA (I-15), a third receiver's kept; a key the rotation retired keeps its retire time.
func TestDnsTlsaRemovalAfterRotationsRetiresEveryKey(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := testDnsRoot(t, ctx)
		networkId, apex := testDnsNetwork(t, ctx, root)
		label := strings.TrimSuffix(apex, "."+root)
		rotated := newTestDnsReceiver(t, ctx, networkId)
		alsoRemoved := newTestDnsReceiver(t, ctx, networkId)
		kept := newTestDnsReceiver(t, ctx, networkId)
		second := newTestDnsReceiverSpki(t)
		third := newTestDnsReceiverSpki(t)
		posts := []struct {
			receiver *testDnsReceiver
			spki     []byte
			retire   [][]byte
		}{
			{receiver: rotated, spki: rotated.spki},
			{receiver: rotated, spki: second},
			{receiver: rotated, spki: second, retire: [][]byte{rotated.spki}},
			// the next rotation's successor, pending
			{receiver: rotated, spki: third},
			{receiver: alsoRemoved, spki: alsoRemoved.spki},
			{receiver: kept, spki: kept.spki},
		}
		for _, post := range posts {
			if _, err := post.receiver.post(t, ctx, networkId, root, label, post.spki, post.retire...); err != nil {
				t.Fatal(err)
			}
		}
		if values := testDnsTlsaValues(t, ctx, root, networkId, apex); !testDnsTlsaHolds(values, second, third, alsoRemoved.spki, kept.spki) {
			t.Fatalf("before the removal %v", values)
		}
		retired := testDnsMailHostKeys(ctx, networkId)[testDnsSpkiHex(rotated.spki)]

		removal := &Removal{
			Subjects: []*RemovalSubject{
				{NetworkId: networkId, Kind: sidecar.RevocationKindSidecar, SubjectId: rotated.sidecarId},
				{NetworkId: networkId, Kind: sidecar.RevocationKindSidecar, SubjectId: alsoRemoved.sidecarId},
			},
			Time: server.NowUtc(),
		}
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
		if values := testDnsTlsaValues(t, ctx, root, networkId, apex); !testDnsTlsaHolds(values, kept.spki) {
			t.Fatalf("after the removal %v, want only the kept receiver's", values)
		}
		keys := testDnsMailHostKeys(ctx, networkId)
		for _, spki := range [][]byte{rotated.spki, second, third, alsoRemoved.spki} {
			if key := keys[testDnsSpkiHex(spki)]; key == nil || key.State != MailHostKeyStateRetired {
				t.Fatalf("a removed receiver's row %+v", key)
			}
		}
		if key := keys[testDnsSpkiHex(rotated.spki)]; !key.RetireTime.Equal(retired.RetireTime) {
			t.Fatalf("the removal moved a retired key's retire time: %v, was %v", key.RetireTime, retired.RetireTime)
		}
		if key := keys[testDnsSpkiHex(kept.spki)]; key == nil || key.State != MailHostKeyStatePublished {
			t.Fatalf("the kept receiver's row %+v", key)
		}
	})
}

// A write held inside its transaction before the receiver's lock while a removal of the receiver commits, keys and
// identity both, reads the revoked identity once it goes on and is refused 403: it neither publishes its key nor
// leaves the removed receiver any TLSA (I-15).
func TestDnsKeyRecordWriteWaitingOnTheReceiversRemovalIsRefused(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := testDnsRoot(t, ctx)
		networkId, apex := testDnsNetwork(t, ctx, root)
		label := strings.TrimSuffix(apex, "."+root)
		receiver := newTestDnsReceiver(t, ctx, networkId)
		if _, err := receiver.post(t, ctx, networkId, root, label, receiver.spki); err != nil {
			t.Fatal(err)
		}
		next := newTestDnsReceiverSpki(t)
		record := receiver.record(t, networkId, root, label, next, receiver.spki)

		held := make(chan struct{})
		release := make(chan struct{})
		var holdOnce sync.Once
		Testing_DnsWriteBarrier = func(name string, barrierRoot string) {
			if barrierRoot != root || name != DnsKeyRecordBarrierLock {
				return
			}
			holdOnce.Do(func() {
				close(held)
				<-release
			})
		}
		defer func() { Testing_DnsWriteBarrier = nil }()
		type outcome struct {
			answer *sidecar.DnsKeyRecordAnswer
			err    error
		}
		outcomes := make(chan outcome, 1)
		go func() {
			answer, err := PublishDnsKeyRecord(ctx, record)
			outcomes <- outcome{answer: answer, err: err}
		}()
		select {
		case <-held:
		case ended := <-outcomes:
			close(release)
			t.Fatalf("the write ended before the receiver's lock: %+v", ended)
		}

		removal := &Removal{
			Subjects: []*RemovalSubject{{NetworkId: networkId, Kind: sidecar.RevocationKindSidecar, SubjectId: receiver.sidecarId}},
			Time:     server.NowUtc(),
		}
		err := dnsCatchRefusal(func() {
			server.Tx(ctx, func(tx server.PgTx) {
				if err := applyDnsTlsaRemovalEffectInTx(ctx, tx, removal); err != nil {
					panic(dnsWriteRefusal{err: err})
				}
				if err := applySidecarIdentityRemovalEffectInTx(ctx, tx, removal); err != nil {
					panic(dnsWriteRefusal{err: err})
				}
			}, server.TxReadCommitted)
		})
		close(release)
		if err != nil {
			t.Fatal(err)
		}
		ended := <-outcomes
		if apiError := testDnsApiError(t, ended.err); apiError.Status != http.StatusForbidden {
			t.Fatalf("the write after the removal: %+v", apiError)
		}
		if values := testDnsTlsaValues(t, ctx, root, networkId, apex); len(values) != 0 {
			t.Fatalf("the removed receiver's tlsa %v", values)
		}
		keys := testDnsMailHostKeys(ctx, networkId)
		if key := keys[testDnsSpkiHex(receiver.spki)]; key == nil || key.State != MailHostKeyStateRetired {
			t.Fatalf("the removed receiver's row %+v", key)
		}
		if key := keys[testDnsSpkiHex(next)]; key != nil {
			t.Fatalf("the write published a removed receiver's key: %+v", key)
		}
	})
}
