package model

// A Team's direct MX, the routes' and the probe outcomes' writes (§15.4, §12.8, §15.15; R-MAIL-2, D-MAIL-12, I-8,
// I-15, FS-33; owner decisions 33, 98, 639; MAIL-M7-23, QD-1103).
//
//   - NameMailDirectMx is POST /mail/direct-mx's write: Team only, mail on, each named receiver holding a published
//     mail_host_key for the network's mx and its address held to §12.7's A rules, written pending, at most
//     MailDirectMxSettings.MaxReceiverCount receivers. Nothing points at a pending row.
//   - The prober (mxedgeservice.MxDirectMxProber) verifies each row from mxedge, which reads the rows through
//     NewMxDirectMxStore and posts each outcome on api's internal route (server.MailDirectMxProbeGatePath), whose
//     handler records it here (RecordMailDirectMxProbe), only while the row still stands: a pass marks it verified,
//     and every verified or failing address of the network becomes §12.8's records at mx (DnsSetDirectMxInTx), so the
//     network's {mxr_} routes drop; a failure marks a verified row failing and changes no record, since a direct MX
//     reverts only on DELETE.
//   - ClearMailDirectMx is DELETE /mail/direct-mx's write: every row and §12.8's direct records, the router rows back,
//     in one transaction, then the routes.
//   - A receiver's removal deletes its rows and points mx at the network's other verified receivers, or back at the
//     routers when none is left, in the deactivation's transaction (I-15): RemovalEffectMailDirectMx.
//
// So mxedge's process writes nothing here: its store reads, in a read-only transaction, and records through api, and
// the zone's writers stay api and the dns block (D-OPS-1). Nothing here logs an address (I-13). Nothing runs unless
// the binary links MG7: a read answers none and a write ErrMailUnlinked.

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/dnsservice"
	"github.com/urnetwork/sidecar/mxedgeservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

var (
	// The network has no mail_network row: a direct MX answers at the mail host mail on writes.
	ErrMailDirectMxMailOff = errors.New("mail direct mx: mail is off")
	// A named receiver holds no published mail_host_key for the network's mx: no key a probe could hold it to.
	ErrMailDirectMxNoReceiver = errors.New("mail direct mx: not a receiver with a published key")
	// A probe outcome names a network api has no row of: refused, with nothing written.
	ErrMailDirectMxProbeNetworkUnknown = errors.New("mail direct mx: the probe's network is unknown")
	// mxedge's store was given no route to record an outcome through: nothing is recorded.
	ErrMailDirectMxProbeNoGate = errors.New("mail direct mx: no route to record the probe through")
)

// One receiver POST /mail/direct-mx names.
type MailDirectMxReceiver struct {
	ReceiverSidecarId server.Id
	Address           netip.Addr
}

// Whether the session's caller is one of the network's Owners or Admins (§15.15's Admin role): a team credential's
// member, not pending, whose role holds the admin permission. A client token never is.
func MailDirectMxCallerPermitted(ctx context.Context, clientSession *session.ClientSession) (permitted bool) {
	server.Db(ctx, func(conn server.PgConn) {
		permitted = teamCallerPermitsWithQuery(ctx, conn, clientSession, RolePermissionAdmin)
	})
	return
}

// The receivers whose key for the network's mx is published, newest first.
func mailDirectMxPublishedReceiversWithQuery(ctx context.Context, query server.PgCanQuery, networkId server.Id) []server.Id {
	receiverIds := []server.Id{}
	result, err := query.Query(
		ctx,
		`
		SELECT receiver_sidecar_id FROM mail_host_key
		WHERE network_id = $1 AND host = $2 AND state = $3
		ORDER BY publish_time DESC, receiver_sidecar_id
		`,
		networkId,
		mailRouteHost,
		string(MailHostKeyStatePublished),
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var receiverId server.Id
			server.Raise(result.Scan(&receiverId))
			if !slices.Contains(receiverIds, receiverId) {
				receiverIds = append(receiverIds, receiverId)
			}
		}
	})
	return receiverIds
}

// The addresses §12.8 points the network's mx at: its verified and failing rows', by address.
func mailDirectMxPointedInTx(ctx context.Context, tx server.PgTx, networkId server.Id) []netip.Addr {
	addresses := []netip.Addr{}
	result, err := tx.Query(
		ctx,
		`
		SELECT DISTINCT address FROM mail_direct_mx
		WHERE network_id = $1 AND state IN ($2, $3)
		ORDER BY address
		`,
		networkId,
		string(MailDirectMxStateVerified),
		string(MailDirectMxStateFailing),
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var text string
			server.Raise(result.Scan(&text))
			if address, err := netip.ParseAddr(text); err == nil {
				addresses = append(addresses, address)
			}
		}
	})
	return addresses
}

// POST /mail/direct-mx's write (the file header): the network's rows after it, by receiver then address.
func NameMailDirectMx(ctx context.Context, networkId server.Id, receivers []*MailDirectMxReceiver) ([]*MailDirectMx, error) {
	if !server.SidecarMg7Linked() {
		return nil, ErrMailUnlinked
	}
	settings := DefaultMailDirectMxSettings()
	if len(receivers) == 0 || settings.MaxReceiverCount < len(receivers) {
		return nil, ErrMailDirectMxInvalid
	}
	addresses := []netip.Addr{}
	for _, receiver := range receivers {
		if receiver == nil || receiver.ReceiverSidecarId == (server.Id{}) || !mailDirectMxAddressValid(receiver.Address) {
			return nil, ErrMailDirectMxInvalid
		}
		addresses = append(addresses, receiver.Address.Unmap())
	}
	if err := dnsservice.ValidateDirectMxAddresses(addresses, &dnsZoneSettings().UserRecords); err != nil {
		return nil, errors.Join(ErrMailDirectMxInvalid, err)
	}
	err := dnsCatchRefusal(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			refuse := func(err error) {
				panic(dnsWriteRefusal{err: err})
			}
			// §12.8's own Team check, the one hook the zone's direct-MX writer asks too
			if err := dnsRequireTeamTierInTx(ctx, tx, networkId); err != nil {
				refuse(err)
			}
			if GetMailNetworkInTx(ctx, tx, networkId) == nil {
				refuse(ErrMailDirectMxMailOff)
			}
			published := mailDirectMxPublishedReceiversWithQuery(ctx, tx, networkId)
			for _, receiver := range receivers {
				if !slices.Contains(published, receiver.ReceiverSidecarId) {
					refuse(ErrMailDirectMxNoReceiver)
				}
				if err := PutMailDirectMxInTx(ctx, tx, networkId, receiver.ReceiverSidecarId, receiver.Address); err != nil {
					refuse(err)
				}
			}
		}, server.TxReadCommitted)
	})
	if err != nil {
		return nil, err
	}
	return ListMailDirectMx(ctx, networkId), nil
}

// DELETE /mail/direct-mx's write (the file header): how many rows went. The routes follow after the transaction, and
// the pass takes them up when Redis cannot.
func ClearMailDirectMx(ctx context.Context, networkId server.Id, root string) (removedCount int64, err error) {
	if !server.SidecarMg7Linked() {
		return 0, ErrMailUnlinked
	}
	err = dnsCatchRefusal(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			removed, err := RemoveMailDirectMxInTx(ctx, tx, networkId)
			if err == nil && 0 < removed && root != "" {
				_, err = DnsClearDirectMxInTx(ctx, tx, networkId, root, nil)
			}
			if err != nil {
				panic(dnsWriteRefusal{err: err})
			}
			removedCount = removed
		}, server.TxReadCommitted)
	})
	if err != nil {
		return 0, err
	}
	if _, projectErr := ProjectMailRoutes(ctx, networkId, root); projectErr != nil {
		glog.Infof("[mail]direct mx cleared; the routes wait for the pass: %s", projectErr)
	}
	return removedCount, nil
}

// Points the network's mx at its verified and failing addresses in root, or back at the routers with none, in tx.
func mailDirectMxRepointInTx(ctx context.Context, tx server.PgTx, networkId server.Id, root string) error {
	if root == "" {
		return nil
	}
	if addresses := mailDirectMxPointedInTx(ctx, tx, networkId); 0 < len(addresses) {
		_, err := DnsSetDirectMxInTx(ctx, tx, networkId, root, addresses, nil)
		return err
	}
	_, err := DnsClearDirectMxInTx(ctx, tx, networkId, root, nil)
	return err
}

// One probe's outcome, as api's internal route takes it from mxedge's prober (server.MailDirectMxProbeGateArgs).
type MailDirectMxProbe struct {
	NetworkId         server.Id
	ReceiverSidecarId server.Id
	Address           netip.Addr
	Passed            bool
}

// Whether the row stands, its lock taken in tx, so a DELETE or a receiver's removal either came first or waits for
// tx's end.
func mailDirectMxRowLockInTx(ctx context.Context, tx server.PgTx, networkId server.Id, receiverSidecarId server.Id, address netip.Addr) (stands bool) {
	result, err := tx.Query(
		ctx,
		`
		SELECT true FROM mail_direct_mx
		WHERE network_id = $1 AND receiver_sidecar_id = $2 AND address = $3
		FOR UPDATE
		`,
		networkId,
		receiverSidecarId,
		address.Unmap().String(),
	)
	server.WithPgResult(result, err, func() {
		stands = result.Next()
	})
	return
}

// api's record of one probe's outcome (the file header's prober row; QD-1103, owner decision 639), what the prober's
// store once wrote from mxedge: in one transaction, the row's lock, its state (SetMailDirectMxProbeInTx) and, on a
// pass, §12.8's records at every verified or failing address of the network in root; then, on a pass, the network's
// routes, which the pass takes up when Redis cannot. recorded false is a row that no longer stands, a DELETE or a
// receiver's removal that came first, and writes nothing, so a probe that raced a removal never points mx back at an
// address the removal took away; a network with no row in network is ErrMailDirectMxProbeNetworkUnknown. A records
// write the zone refuses, the tier or mail gone, rolls the state back with it and is the error, as it was: the next
// probe tries again.
func RecordMailDirectMxProbe(ctx context.Context, probe *MailDirectMxProbe, root string) (recorded bool, returnErr error) {
	if !server.SidecarMg7Linked() {
		return false, ErrMailUnlinked
	}
	if probe == nil || probe.NetworkId == (server.Id{}) || probe.ReceiverSidecarId == (server.Id{}) || !probe.Address.IsValid() {
		return false, ErrMailDirectMxInvalid
	}
	address := probe.Address.Unmap()
	returnErr = dnsCatchRefusal(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			// a rerun of the transaction starts over
			recorded = false
			refuse := func(err error) {
				panic(dnsWriteRefusal{err: err})
			}
			if !mailDirectMxRowLockInTx(ctx, tx, probe.NetworkId, probe.ReceiverSidecarId, address) {
				if !networkExistsInTx(ctx, tx, probe.NetworkId) {
					refuse(ErrMailDirectMxProbeNetworkUnknown)
				}
				return
			}
			if err := SetMailDirectMxProbeInTx(ctx, tx, probe.NetworkId, probe.ReceiverSidecarId, address, probe.Passed); err != nil {
				refuse(err)
			}
			if probe.Passed {
				if err := mailDirectMxRepointInTx(ctx, tx, probe.NetworkId, root); err != nil {
					refuse(err)
				}
			}
			recorded = true
		}, server.TxReadCommitted)
	})
	if returnErr != nil {
		return false, returnErr
	}
	if recorded && probe.Passed && root != "" {
		if _, projectErr := ProjectMailRoutes(ctx, probe.NetworkId, root); projectErr != nil {
			glog.Infof("[mail]a direct mx verified; the routes wait for the pass: %s", projectErr)
		}
	}
	return recorded, nil
}

// The name the direct-MX removal effect registers under.
const RemovalEffectMailDirectMx = "mail_direct_mx"

// The removal effect (I-15): each removed sidecar's direct-MX rows go, and in each root the network has a zone in,
// §12.8's records name its other verified receivers, or the routers with none left, in the deactivation's
// transaction, whichever process runs it. Idempotent: a removal applied again finds no row and writes nothing. A
// records write the zone refuses, the tier or mail gone, falls back to the routers, and a zone waiting on its lock
// is left to its next write, as the TLSA effect leaves it, so a removal never fails on a zone it cannot point.
func applyMailDirectMxRemovalEffectInTx(ctx context.Context, tx server.PgTx, removal *Removal) error {
	if !server.SidecarMg7Linked() {
		return nil
	}
	for _, subject := range removal.SubjectsOf(sidecar.RevocationKindSidecar) {
		removedCount, err := RemoveMailDirectMxReceiverInTx(ctx, tx, subject.NetworkId, subject.SubjectId)
		if err != nil {
			return err
		}
		if removedCount == 0 || !dnsCustomHostLinked() {
			continue
		}
		for _, root := range getNetworkDnsRootsInTx(ctx, tx, subject.NetworkId) {
			if err := mailDirectMxRepointInTx(ctx, tx, subject.NetworkId, root); err == nil || IsDnsZoneWaiting(err) {
				continue
			}
			if _, clearErr := DnsClearDirectMxInTx(ctx, tx, subject.NetworkId, root, nil); clearErr != nil &&
				!errors.Is(clearErr, ErrDnsUnlinked) && !IsDnsZoneWaiting(clearErr) {
				return clearErr
			}
		}
	}
	return nil
}

func init() {
	RegisterRemovalEffect(RemovalEffectMailDirectMx, applyMailDirectMxRemovalEffectInTx)
}

// What mxedge's direct-MX store records each probe's outcome through: api's internal route on the block's edge
// bearer, server.PlatformClientSource's RecordMailDirectMxProbe (QD-1103, owner decision 639). Safe for concurrent
// use.
type MxDirectMxProbeGate interface {
	RecordMailDirectMxProbe(ctx context.Context, args *server.MailDirectMxProbeGateArgs) (*server.MailDirectMxProbeGateResult, error)
}

// mxedgeservice.MxDirectMxStore for mxedge's prober in one dns root (the file header's prober row): the rows read
// from Postgres in a read-only transaction, each outcome posted to api through the gate, so nothing it does writes
// Postgres. Safe for concurrent use.
type mxDirectMxStore struct {
	root     string
	gate     MxDirectMxProbeGate
	settings *MailDirectMxSettings
}

var _ mxedgeservice.MxDirectMxStore = (*mxDirectMxStore)(nil)

// The prober's store for root, recording through gate: the block's platform client source. gate nil records
// nothing, each outcome then ErrMailDirectMxProbeNoGate, which the prober logs.
func NewMxDirectMxStore(root string, gate MxDirectMxProbeGate) mxedgeservice.MxDirectMxStore {
	return &mxDirectMxStore{root: dnsLabelRoot(root), gate: gate, settings: DefaultMailDirectMxSettings()}
}

// The network's published keys for mx, newest first, as ListMailHostKeys orders them.
func mailDirectMxPublishedKeysWithQuery(ctx context.Context, query server.PgCanQuery, networkId server.Id) []*MailHostKey {
	keys := []*MailHostKey{}
	result, err := query.Query(
		ctx,
		`
		SELECT network_id, host, receiver_sidecar_id, spki_sha256, state, publish_time, retire_time
		FROM mail_host_key
		WHERE network_id = $1 AND host = $2 AND state = $3
		ORDER BY publish_time DESC, receiver_sidecar_id, spki_sha256
		`,
		networkId,
		mailRouteHost,
		string(MailHostKeyStatePublished),
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			keys = append(keys, scanMailHostKey(result))
		}
	})
	return keys
}

// See mxedgeservice.MxDirectMxStore: every row, with its network's mail host, the receiver's published keys for mx
// and the network's published receivers, all read in one read-only transaction, so Postgres refuses any write in it
// and the reads share one snapshot.
func (self *mxDirectMxStore) ListDirectMx(ctx context.Context) ([]*mxedgeservice.MxDirectMxRow, error) {
	if !server.SidecarMg7Linked() {
		return nil, ErrMailUnlinked
	}
	var rows []*mxedgeservice.MxDirectMxRow
	server.Tx(ctx, func(tx server.PgTx) {
		// a rerun starts over
		rows = []*mxedgeservice.MxDirectMxRow{}
		result, err := tx.Query(
			ctx,
			`
			SELECT network_id, receiver_sidecar_id, address, state, verify_time
			FROM mail_direct_mx
			ORDER BY network_id, receiver_sidecar_id, address
			LIMIT $1
			`,
			self.settings.ListMaxCount,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var networkId, receiverId server.Id
				var address, state string
				var verifyTime *time.Time
				server.Raise(result.Scan(&networkId, &receiverId, &address, &state, &verifyTime))
				parsed, parseErr := netip.ParseAddr(address)
				if parseErr != nil {
					continue
				}
				row := &mxedgeservice.MxDirectMxRow{
					NetworkId: sidecar.Id(networkId), ReceiverSidecarId: sidecar.Id(receiverId), Address: parsed, State: state,
				}
				if verifyTime != nil {
					row.VerifyTime = *verifyTime
				}
				rows = append(rows, row)
			}
		})
		// each network's label and keys, read once for its rows
		networkLabels := map[server.Id]string{}
		networkKeys := map[server.Id][]*MailHostKey{}
		for _, row := range rows {
			networkId := server.Id(row.NetworkId)
			label, ok := networkLabels[networkId]
			if !ok {
				// GetNetworkLabel's read, in this transaction
				if server.SidecarMg2Linked() {
					label = getNetworkLabelInTx(ctx, tx, networkId, self.root)
				}
				networkLabels[networkId] = label
			}
			if label != "" {
				row.Host = mailRouteHost + "." + label + "." + self.root
			}
			keys, ok := networkKeys[networkId]
			if !ok {
				keys = mailDirectMxPublishedKeysWithQuery(ctx, tx, networkId)
				networkKeys[networkId] = keys
			}
			for _, key := range keys {
				if key.ReceiverSidecarId == server.Id(row.ReceiverSidecarId) {
					row.SpkiSha256s = append(row.SpkiSha256s, slices.Clone(key.SpkiSha256))
				}
				if !slices.Contains(row.HintSidecarIds, sidecar.Id(key.ReceiverSidecarId)) {
					row.HintSidecarIds = append(row.HintSidecarIds, sidecar.Id(key.ReceiverSidecarId))
				}
			}
		}
	}, pgx.ReadOnly)
	return rows, nil
}

// See mxedgeservice.MxDirectMxStore: the outcome posted to api's route through the gate, whose handler records it
// (RecordMailDirectMxProbe). Reaches no Postgres. An error, the route's refusal among them, leaves the row as it was,
// for the next probe; a row gone in between answers recorded false, a no-op success.
func (self *mxDirectMxStore) RecordDirectMxProbe(ctx context.Context, row *mxedgeservice.MxDirectMxRow, passed bool) error {
	if self.gate == nil {
		return ErrMailDirectMxProbeNoGate
	}
	if row == nil {
		return ErrMailDirectMxInvalid
	}
	_, err := self.gate.RecordMailDirectMxProbe(ctx, &server.MailDirectMxProbeGateArgs{
		NetworkId:         server.Id(row.NetworkId),
		ReceiverSidecarId: server.Id(row.ReceiverSidecarId),
		Address:           row.Address.Unmap().String(),
		Passed:            passed,
	})
	return err
}
