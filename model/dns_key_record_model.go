package model

// §12.8's POST /dns/key-record and the TLSA it writes (DNS-M7-02; D-MAIL-4, I-15, FS-41, FS-42, FI-12): the only
// TLSA writer. A receiver, a sidecar the dns block has found holding §8's listen on smtp/25/mx, or a member scope's
// smtp/25/mx.<scope> from that member's consented devices (AclGateView.MayReceive, owner decision 126), posts its
// key's SubjectPublicKeyInfo with a TlsKeyAttestation its identity key signed for that key and the host's identifier
// set (§13.8). The key's row goes into mail_host_key (UpsertMailHostKeyInTx, MAIL's), and the host's TLSA RRset at
// _25._tcp.<host> is rebuilt from every published key of the host, 3 1 1 a key, TTL dns.yml's tlsa, in one write
// through §12.6's path with an admin event, whose post hints the network's sidecars (DnsWritePost).
//
// A rotation retires its old key through the same write (§15.5; owner decision 443): once its new key serves, the
// receiver posts that key again, a repeat, with the rotated-out key's SPKI hash in retire_spki_sha256, and the write
// retires, in its own transaction, the caller's own published mail_host_key rows of the host the list names before it
// rebuilds the RRset, so a key rotated on suspicion leaves TLSA as soon as its successor serves. A hash naming no
// published key of the caller's at the host matches no row and is ignored: another receiver's key, or one of another
// host, is never retired. A post with no list retires nothing.
//
// A removal ends a receiver's TLSA in the deactivation's own transaction (I-15): the "dns_tlsa" removal effect retires
// the receiver's keys and rebuilds every TLSA RRset its network's zones hold from the keys still published, so it
// is idempotent, whichever of MAIL's effect and this one runs first, and §19.10's replay of a removal after the dump
// commits it again above restore_serial as an ordinary write, or writes nothing.
//
// A write and a removal of the same receiver serialize on the receiver's lock (lockMailHostReceiverInTx), which each
// takes before it reads the receiver's identity or touches its keys: a write that waited out a removal reads the
// identity the removal revoked and is refused, and a removal that waited out a write retires the key the write
// published. Without it a write that read the identity before a removal committed could publish the removed
// receiver's key after it, and a write locking two of a receiver's rows could deadlock a removal locking them in
// another order.
//
// Nothing here reads or writes unless the binary links MG2, MG5 and MG7.

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"slices"
	"strings"

	"github.com/miekg/dns"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/dnsservice"

	"github.com/urnetwork/server"
)

// The admin event a key write adds (§7.11, §12.8): the receiver is its target.
const AdminEventActionDnsKeyRecord = "dns.key_record"

// The name the TLSA removal effect registers under (§7.10).
const RemovalEffectDnsTlsa = "dns_tlsa"

// The barrier a key-record write passes inside its transaction just before it takes the receiver's lock (§22.8),
// called with the root like the write path's: a schedule holds it to commit a removal of the receiver first.
const DnsKeyRecordBarrierLock = "model.dns_key_record.lock"

// One key-record write: the receiver the dns block authenticated and found holding the listen, and its body.
type DnsKeyRecord struct {
	Root              string
	NetworkId         server.Id
	ReceiverSidecarId server.Id
	Request           *sidecar.DnsKeyRecordRequest
}

// The TLSA owner of host, mx or mx.<scope>, in the zone apex.
func dnsTlsaOwner(host string, apex string) string {
	return "_25._tcp." + host + "." + apex
}

// The host and its scope, "" for the network's own mx, or false for a host no receiver holds.
func dnsKeyRecordHost(host string) (string, bool) {
	if host == "mx" {
		return "", true
	}
	scope, found := strings.CutPrefix(host, "mx.")
	if !found || !isDnsCtLabel(scope) {
		return "", false
	}
	return scope, true
}

// POST /dns/key-record's write (see the file header): the zone's version after it, or a *sidecar.DnsApiError for a
// host, key or attestation it refuses, with nothing written.
func PublishDnsKeyRecord(ctx context.Context, record *DnsKeyRecord) (*sidecar.DnsKeyRecordAnswer, error) {
	if !server.SidecarMg2Linked() || !server.SidecarMg5Linked() || !server.SidecarMg7Linked() {
		return nil, dnsRecordsUnavailable("the zone and mail tables are not linked")
	}
	request := record.Request
	if request == nil {
		return nil, dnsRecordsInvalid("no body")
	}
	scope, ok := dnsKeyRecordHost(request.Host)
	if !ok {
		return nil, dnsRecordsInvalid("%q is not mx or mx.<scope>", request.Host)
	}
	publicKey, err := x509.ParsePKIXPublicKey(request.Spki)
	if ecdsaKey, isEcdsa := publicKey.(*ecdsa.PublicKey); err != nil || !isEcdsa || ecdsaKey.Curve != elliptic.P256() {
		return nil, dnsRecordsInvalid("the key is not a P-256 SubjectPublicKeyInfo")
	}
	spkiSha256 := sha256.Sum256(request.Spki)
	// the keys to retire: each a SHA-256, and never the key this write publishes, which the caller would then hold
	// with no TLSA; the body's bound bounds the list, and a hash listed twice matches its row once
	for _, retireSpkiSha256 := range request.RetireSpkiSha256s {
		switch {
		case len(retireSpkiSha256) != sha256.Size:
			return nil, dnsRecordsInvalid("a key to retire is not named by its SPKI's SHA-256")
		case bytes.Equal(retireSpkiSha256, spkiSha256[:]):
			return nil, dnsRecordsInvalid("the write retires the key it publishes")
		}
	}
	root := dnsLabelRoot(record.Root)
	var answer *sidecar.DnsKeyRecordAnswer
	var returnErr error
	err = dnsCatchRefusal(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			answer, returnErr = publishDnsKeyRecordInTx(ctx, tx, root, scope, spkiSha256, record)
			if returnErr != nil {
				panic(dnsWriteRefusal{err: returnErr})
			}
		}, server.TxReadCommitted)
	})
	if err != nil {
		return nil, dnsRecordsApiError(err)
	}
	return answer, nil
}

func publishDnsKeyRecordInTx(ctx context.Context, tx server.PgTx, root string, scope string, spkiSha256 [sha256.Size]byte, record *DnsKeyRecord) (*sidecar.DnsKeyRecordAnswer, error) {
	networkId := record.NetworkId
	// a scope's mail host is only a Team scope's the network's projection lists (DNS-M8-01); with no scope source
	// none is, the closed direction
	if scope != "" {
		if scopes, ok := dnsNetworkScopeLabels(ctx, networkId); !ok || !slices.Contains(scopes, scope) {
			return nil, dnsRecordsRefused("%s is no Team scope of the network", scope)
		}
	}
	// the receiver's lock before its identity is read (the file header): a removal that committed while this write
	// waited is seen below, and one that comes later waits for this write, then retires what it published
	dnsWriteBarrier(DnsKeyRecordBarrierLock, root)
	lockMailHostReceiverInTx(ctx, tx, networkId, record.ReceiverSidecarId)
	identityPublicKey := getActiveSidecarIdentityKeyInTx(ctx, tx, networkId, record.ReceiverSidecarId)
	if identityPublicKey == nil {
		return nil, dnsRecordsRefused("the caller is no active sidecar of the network")
	}
	label, err := EnsureNetworkLabelInTx(ctx, tx, networkId, root)
	if err != nil {
		return nil, err
	}
	identifierSet, err := sidecar.TlsIdentifierSet(strings.TrimSuffix(root, "."), label, scope, getNetworkLiveAliasesInTx(ctx, tx, networkId, root))
	if err != nil {
		return nil, dnsRecordsInvalid("no identifier set for the host")
	}
	identifierSetSha256, err := sidecar.TlsIdentifierSetHash(identifierSet)
	if err != nil {
		return nil, dnsRecordsInvalid("no identifier set for the host")
	}
	_, err = sidecar.VerifyTlsKeyAttestation(
		record.Request.Attestation,
		identityPublicKey,
		sidecar.Id(networkId),
		sidecar.Id(record.ReceiverSidecarId),
		spkiSha256,
		identifierSetSha256,
	)
	if err != nil {
		// a key that is not the caller's, or an attestation for another network, host or set
		return nil, dnsRecordsRefused("the attestation does not prove the key is the caller's")
	}
	if _, err := UpsertMailHostKeyInTx(ctx, tx, &MailHostKey{
		NetworkId:         networkId,
		Host:              record.Request.Host,
		ReceiverSidecarId: record.ReceiverSidecarId,
		SpkiSha256:        spkiSha256[:],
	}); err != nil {
		return nil, err
	}
	if 0 < len(record.Request.RetireSpkiSha256s) {
		retireMailHostReceiverKeysInTx(ctx, tx, networkId, record.Request.Host, record.ReceiverSidecarId, record.Request.RetireSpkiSha256s)
	}
	receiverId := record.ReceiverSidecarId
	result, err := DnsPutTlsaInTx(ctx, tx, networkId, root, record.Request.Host, &AdminEvent{
		NetworkId:  networkId,
		ActorKind:  AdminEventActorSidecar,
		ActorId:    &receiverId,
		Action:     AdminEventActionDnsKeyRecord,
		TargetKind: AdminEventTargetSidecar,
		TargetId:   &receiverId,
		Result:     AdminEventResultOk,
	})
	if err != nil {
		return nil, err
	}
	version := uint64(0)
	if result != nil {
		version = result.Serial
	}
	if version == 0 {
		// nothing changed: the key was published already, as after a restore's re-registration (§19.10), and nothing
		// listed was the caller's published key, as at a repeat of a retirement whose answer was lost
		if version, err = getDnsZoneVersionOrZeroInTx(ctx, tx, root, networkId); err != nil {
			return nil, err
		}
	}
	return &sidecar.DnsKeyRecordAnswer{Version: version}, nil
}

// Takes the receiver's lock for the rest of the caller's transaction (the file header): a key-record write takes it
// before it reads the receiver's identity, and the removal effect before it retires the receiver's keys. A
// transaction holding it already takes it again at once.
func lockMailHostReceiverInTx(ctx context.Context, tx server.PgTx, networkId server.Id, receiverSidecarId server.Id) {
	server.RaisePgResult(tx.Exec(
		ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended('mail_host_key:' || $1::uuid::text || ':' || $2::uuid::text, 0))`,
		networkId,
		receiverSidecarId,
	))
}

// Retires the receiver's published keys of host whose SPKI SHA-256 spkiSha256s names, in the caller's transaction
// under the receiver's lock, retire_time now: a rotation's retirement (owner decision 443). A hash naming no published
// key of this receiver's at host matches no row, so another receiver's key or a key of another host stays as it is,
// and a key retired already keeps its retire_time.
func retireMailHostReceiverKeysInTx(ctx context.Context, tx server.PgTx, networkId server.Id, host string, receiverSidecarId server.Id, spkiSha256s [][]byte) {
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			UPDATE mail_host_key
			SET state = $5, retire_time = $6
			WHERE
				network_id = $1 AND
				host = $2 AND
				receiver_sidecar_id = $3 AND
				spki_sha256 = ANY($4::bytea[]) AND
				state = $7
		`,
		networkId,
		host,
		receiverSidecarId,
		spkiSha256s,
		string(MailHostKeyStateRetired),
		server.NowUtc(),
		string(MailHostKeyStatePublished),
	))
}

// The identity key of the network's sidecar while it is active, nil otherwise.
func getActiveSidecarIdentityKeyInTx(ctx context.Context, query server.PgCanQuery, networkId server.Id, sidecarId server.Id) []byte {
	if !server.SidecarMg1Linked() {
		return nil
	}
	var identityPublicKey []byte
	result, err := query.Query(
		ctx,
		`SELECT identity_public_key FROM sidecar_identity WHERE sidecar_id = $1 AND network_id = $2 AND status = $3`,
		sidecarId,
		networkId,
		SidecarIdentityStatusActive,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&identityPublicKey))
		}
	})
	return identityPublicKey
}

// The network's live aliases in root, in name order.
func getNetworkLiveAliasesInTx(ctx context.Context, query server.PgCanQuery, networkId server.Id, root string) []string {
	aliases := []string{}
	result, err := query.Query(
		ctx,
		`SELECT label FROM dns_label WHERE network_id = $1 AND root = $2 AND kind = $3 AND state = $4 ORDER BY label`,
		networkId,
		root,
		DnsLabelKindAlias,
		DnsLabelStateLive,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var alias string
			server.Raise(result.Scan(&alias))
			aliases = append(aliases, alias)
		}
	})
	return aliases
}

// Rebuilds host's TLSA RRset in the network's zone in root from every published key of the host, 3 1 1 a key in
// hash order, or removes it when none is left, in the caller's transaction, which must be read committed: one write
// through §12.6's path with event, or none when the RRset is already that. Returns the write, nil when nothing was
// written.
func DnsPutTlsaInTx(ctx context.Context, tx server.PgTx, networkId server.Id, root string, host string, event *AdminEvent) (*DnsWriteResult, error) {
	if !server.SidecarMg2Linked() || !server.SidecarMg5Linked() || !server.SidecarMg7Linked() {
		return nil, ErrDnsUnlinked
	}
	root = dnsLabelRoot(root)
	label, err := EnsureNetworkLabelInTx(ctx, tx, networkId, root)
	if err != nil {
		return nil, err
	}
	// the zone's lock before the keys are read, so two receivers' writes rebuild one after the other and the second
	// reads the first's committed key: the zone on first need takes it
	if _, err := EnsureNetworkZoneInTx(ctx, tx, networkId, root, nil); err != nil {
		return nil, err
	}
	owner := dnsTlsaOwner(host, label+"."+root)
	values := []string{}
	result, err := tx.Query(
		ctx,
		`SELECT spki_sha256 FROM mail_host_key WHERE network_id = $1 AND host = $2 AND state = $3 ORDER BY spki_sha256`,
		networkId,
		host,
		string(MailHostKeyStatePublished),
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var spkiSha256 []byte
			server.Raise(result.Scan(&spkiSha256))
			value := "3 1 1 " + strings.ToUpper(hex.EncodeToString(spkiSha256))
			if !slices.Contains(values, value) {
				values = append(values, value)
			}
		}
	})
	held := (*dnsservice.DnsRecordSet)(nil)
	for _, row := range getDnsZoneRowsInTx(ctx, tx, root, networkId) {
		if row.set.Owner == owner && row.set.Type == dns.TypeTLSA {
			set := row.set
			held = &set
		}
	}
	ttl := dnsZoneTtl().Tlsa
	args := &DnsWriteArgs{Root: root, NetworkId: networkId, AdminEvent: event}
	switch {
	case len(values) == 0 && held == nil:
		return nil, nil
	case len(values) == 0:
		args.Deletes = []dnsservice.DnsRecordKey{held.Key()}
	default:
		set := dnsservice.DnsRecordSet{Owner: owner, Type: dns.TypeTLSA, Ttl: ttl, Kind: dnsservice.DnsRecordKindKey, Rdata: values}
		if held != nil && held.Kind == set.Kind && held.Ttl == set.Ttl && dnsTlsaValuesEqual(held.Rdata, values) {
			return nil, nil
		}
		args.Upserts = []dnsservice.DnsRecordSet{set}
	}
	return DnsWriteInTx(ctx, tx, args)
}

// Whether two TLSA value lists hold the same records, whatever their order or the case of their hex.
func dnsTlsaValuesEqual(first []string, second []string) bool {
	canonical := func(values []string) []string {
		out := []string{}
		for _, value := range values {
			out = append(out, strings.ToUpper(strings.Join(strings.Fields(value), " ")))
		}
		slices.Sort(out)
		return out
	}
	return slices.Equal(canonical(first), canonical(second))
}

// Retires every key of the network's receiver and rebuilds each TLSA RRset the network's zones hold from the keys
// still published, in the caller's transaction: the "dns_tlsa" removal effect's work for one receiver (I-15). It
// takes the receiver's lock first, so a key-record write under way finishes before the keys are read, and the zones'
// locks root by root in name order. A second run writes nothing.
func DnsDeleteTlsaInTx(ctx context.Context, tx server.PgTx, networkId server.Id, receiverSidecarId server.Id) error {
	if !server.SidecarMg2Linked() || !server.SidecarMg5Linked() || !server.SidecarMg7Linked() {
		return nil
	}
	lockMailHostReceiverInTx(ctx, tx, networkId, receiverSidecarId)
	if _, err := RetireMailHostKeysInTx(ctx, tx, networkId, receiverSidecarId); err != nil {
		return err
	}
	roots := getNetworkDnsRootsInTx(ctx, tx, networkId)
	slices.Sort(roots)
	for _, root := range roots {
		apexes := getDnsZoneApexesInTx(ctx, tx, root, networkId)
		if len(apexes) == 0 {
			continue
		}
		apex := apexes[0]
		hosts := []string{}
		for _, row := range getDnsZoneRowsInTx(ctx, tx, root, networkId) {
			if row.set.Type != dns.TypeTLSA {
				continue
			}
			host, ok := strings.CutPrefix(strings.TrimSuffix(row.set.Owner, "."+apex), "_25._tcp.")
			if ok && !slices.Contains(hosts, host) {
				hosts = append(hosts, host)
			}
		}
		for _, host := range hosts {
			if _, err := DnsPutTlsaInTx(ctx, tx, networkId, root, host, nil); err != nil && !IsDnsZoneWaiting(err) {
				return err
			}
		}
	}
	return nil
}

// The removal effect: each removed sidecar's TLSA (I-15). Every removed receiver's lock comes first, in id order, so a
// removal of several never holds a zone's lock while it waits on a receiver's write that needs that zone, and two
// removals take their receivers' locks in one order.
func applyDnsTlsaRemovalEffectInTx(ctx context.Context, tx server.PgTx, removal *Removal) error {
	subjects := removal.SubjectsOf(sidecar.RevocationKindSidecar)
	if server.SidecarMg2Linked() && server.SidecarMg5Linked() && server.SidecarMg7Linked() {
		ordered := slices.Clone(subjects)
		slices.SortFunc(ordered, func(a *RemovalSubject, b *RemovalSubject) int {
			if order := a.NetworkId.Cmp(b.NetworkId); order != 0 {
				return order
			}
			return a.SubjectId.Cmp(b.SubjectId)
		})
		for _, subject := range ordered {
			lockMailHostReceiverInTx(ctx, tx, subject.NetworkId, subject.SubjectId)
		}
	}
	for _, subject := range subjects {
		if err := DnsDeleteTlsaInTx(ctx, tx, subject.NetworkId, subject.SubjectId); err != nil {
			return err
		}
	}
	return nil
}

func init() {
	RegisterRemovalEffect(RemovalEffectDnsTlsa, applyDnsTlsaRemovalEffectInTx)
}
