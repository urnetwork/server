package model

// §18.8's lease and §7.7's ephemeral clients (D-SDK-17, R-TEAM-6, I-15): an
// ephemeral client is reaped once it has no open connection and both its
// newest disconnect and its lease's seen_time are over ReapAfter old, or at
// once by POST /sidecar/close with the close capability its mint returned.
//
//	lease    network_client_lease(client_id PK, seen_time, close_sha256 unique),
//	         one row per ephemeral client, inserted at its mint
//	         (InsertClientLeaseInTx) and deleted with its client (the removal
//	         chain's "lease" effect)
//	poll     GET /sidecar/sync/poll writes seen_time (TouchClientLease) only when
//	         its marker {scp_<C>}hb sets or errs, at most once per 300 s while
//	         Redis answers (pushservice.LongPoll); no poll writes network_client,
//	         whose auth_time keeps ConnectNetworkClient's hourly refresh
//	reap     taskworker's ephemeral job every ReapInterval: lease rows past the
//	         threshold in seen_time order, each probed by
//	         network_client_connection's (client_id, connect_time) index, so a
//	         client connected for hours survives a short disconnect; each reap
//	         runs §7.10's removal chain (deactivateNetworkClientsInTx) with the
//	         reason deactivated
//	close    a 128-bit capability, returned once and kept only as its SHA-256;
//	         POST /sidecar/close reaps its client at once and answers the same
//	         whether or not anything matched
//
// A reap locks the client's network_client row before its lease row, the
// order a removal takes them in (the client, then its removal effects), and a
// poll's touch locks the lease row alone, so none of them wait in a cycle.
//
// Nothing here reads or writes network_client_lease unless MG1 is linked
// (server.SidecarMg1Linked).

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/glog"
	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
)

// Tunables of the lease and the reap.
type NetworkClientLeaseSettings struct {
	// How long both an ephemeral client's lease seen_time and its newest
	// disconnect must be past before it is reaped: 35 min (§7.7), so 30-36 min
	// after its last poll, given the poll's 300 s marker, and 35-36 min after
	// its last disconnect, given ReapInterval. Shorter reaps a page between two
	// polls.
	ReapAfter time.Duration
	// How often taskworker's ephemeral job runs: 60 s (§18.8).
	ReapInterval time.Duration
	// The most lease rows one run of the job looks at, so a run's work stays
	// bounded as the table grows; a run that stops short hands its cursor to the
	// next, which runs at once. Non-positive looks at none.
	ReapPassCount int
	// The longest one run may take before it hands its cursor on.
	ReapPassTimeout time.Duration
}

func DefaultNetworkClientLeaseSettings() *NetworkClientLeaseSettings {
	return &NetworkClientLeaseSettings{
		ReapAfter:       35 * time.Minute,
		ReapInterval:    60 * time.Second,
		ReapPassCount:   1000,
		ReapPassTimeout: 50 * time.Second,
	}
}

// A close capability's length, 128 bits (§7.7), as base64url without padding.
const (
	clientCloseCapabilityByteCount  = 16
	clientCloseCapabilityTextLength = 22
)

var clientCloseCapabilityEncoding = base64.RawURLEncoding

// The capability's bytes from its text, strictly: exactly its encoding of
// exactly 16 bytes, so one capability has one text.
func parseClientCloseCapability(capability string) ([]byte, bool) {
	if len(capability) != clientCloseCapabilityTextLength {
		return nil, false
	}
	capabilityBytes, err := clientCloseCapabilityEncoding.DecodeString(capability)
	if err != nil || len(capabilityBytes) != clientCloseCapabilityByteCount ||
		clientCloseCapabilityEncoding.EncodeToString(capabilityBytes) != capability {
		return nil, false
	}
	return capabilityBytes, true
}

// What api keeps of a capability.
func clientCloseCapabilitySha256(capabilityBytes []byte) []byte {
	sum := sha256.Sum256(capabilityBytes)
	return sum[:]
}

// Inserts an ephemeral client's lease at its mint, in the mint's transaction,
// and returns the close capability the mint returns once. Only its SHA-256 is
// stored.
func InsertClientLeaseInTx(ctx context.Context, tx server.PgTx, clientId server.Id, now time.Time) (capability string) {
	if !server.SidecarMg1Linked() {
		panic(errors.New("an ephemeral client's lease needs MG1"))
	}
	capabilityBytes := make([]byte, clientCloseCapabilityByteCount)
	if _, err := rand.Read(capabilityBytes); err != nil {
		panic(err)
	}
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			INSERT INTO network_client_lease (
				client_id,
				seen_time,
				close_sha256
			)
			VALUES ($1, $2, $3)
		`,
		clientId,
		now,
		clientCloseCapabilitySha256(capabilityBytes),
	))
	return clientCloseCapabilityEncoding.EncodeToString(capabilityBytes)
}

// Writes the client's lease seen_time, which a poll calls when its marker
// {scp_<C>}hb sets or errs (pushservice.LeaseTouchFunction). A client with no
// lease, every client that is not ephemeral, writes nothing. Locks the lease row
// alone.
func TouchClientLease(ctx context.Context, clientId connect.Id) (returnErr error) {
	return touchClientLeaseAt(ctx, server.Id(clientId), server.NowUtc())
}

func touchClientLeaseAt(ctx context.Context, clientId server.Id, now time.Time) (returnErr error) {
	if !server.SidecarMg1Linked() {
		return nil
	}
	if r := server.HandleError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(
				ctx,
				`
					UPDATE network_client_lease
					SET seen_time = $2
					WHERE client_id = $1 AND seen_time < $2
				`,
				clientId,
				now,
			))
		})
	}); r != nil {
		return fmt.Errorf("lease touch: %v", r)
	}
	return nil
}

// The context's key for the reason a deactivation records.
type removalReasonContextKey struct{}

// ctx for a deactivation whose removal effects record reason rather than
// removed: the reap's, deactivated (§18.8).
func withRemovalReason(ctx context.Context, reason sidecar.RevocationReason) context.Context {
	return context.WithValue(ctx, removalReasonContextKey{}, reason)
}

// The reason a deactivation in ctx records: removed unless withRemovalReason
// says otherwise.
func removalReasonOf(ctx context.Context) sidecar.RevocationReason {
	if reason, ok := ctx.Value(removalReasonContextKey{}).(sidecar.RevocationReason); ok {
		return reason
	}
	return sidecar.RevocationReasonRemoved
}

// The removal chain's lease effect: a removed client's lease goes with it.
const RemovalEffectLease = "lease"

func init() {
	RegisterRemovalEffect(RemovalEffectLease, deleteClientLeasesInTx)
}

func deleteClientLeasesInTx(ctx context.Context, tx server.PgTx, removal *Removal) error {
	if !server.SidecarMg1Linked() {
		return nil
	}
	clients := removal.SubjectsOf(sidecar.RevocationKindClient)
	if len(clients) == 0 {
		return nil
	}
	clientIds := make([]server.Id, 0, len(clients))
	for _, client := range clients {
		clientIds = append(clientIds, client.SubjectId)
	}
	_, err := tx.Exec(
		ctx,
		`DELETE FROM network_client_lease WHERE client_id = ANY($1)`,
		clientIds,
	)
	return err
}

// Called in a reap's transaction, after its deactivation, with the client's
// network: §7.9's tick schedule (config_log_tick.go), so the tick that carries
// the reap is owed exactly when the reap commits.
var reapedEphemeralClientCallbacks []func(ctx context.Context, tx server.PgTx, networkId server.Id, clientId server.Id)

// Reaps the ephemeral client in tx: locks its network_client row, then its
// lease, and, when stillReapable is nil or says so of the lease's seen_time and
// the client's connections now, deactivates it through §7.10's removal chain
// with the reason deactivated. False when the client has no lease, is inactive,
// or is reapable no longer.
func reapEphemeralClientInTx(
	ctx context.Context,
	tx server.PgTx,
	clientId server.Id,
	stillReapable func(seenTime time.Time) bool,
) (reaped bool, networkId server.Id, err error) {
	found := false
	result, err := tx.Query(
		ctx,
		`
			SELECT network_id
			FROM network_client
			WHERE client_id = $1 AND active = true
			FOR UPDATE
		`,
		clientId,
	)
	if err != nil {
		return false, networkId, err
	}
	for result.Next() {
		if err := result.Scan(&networkId); err != nil {
			result.Close()
			return false, networkId, err
		}
		found = true
	}
	result.Close()
	if err := result.Err(); err != nil {
		return false, networkId, err
	}
	if !found {
		return false, networkId, nil
	}
	var seenTime time.Time
	err = tx.QueryRow(
		ctx,
		`
			SELECT seen_time
			FROM network_client_lease
			WHERE client_id = $1
			FOR UPDATE
		`,
		clientId,
	).Scan(&seenTime)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, networkId, nil
	}
	if err != nil {
		return false, networkId, err
	}
	if stillReapable != nil && !stillReapable(seenTime) {
		return false, networkId, nil
	}
	count, err := deactivateNetworkClientsInTx(withRemovalReason(ctx, sidecar.RevocationReasonDeactivated), tx, []server.Id{clientId}, networkId)
	if err != nil {
		return false, networkId, err
	}
	if count != 1 {
		return false, networkId, nil
	}
	for _, callback := range reapedEphemeralClientCallbacks {
		callback(ctx, tx, networkId, clientId)
	}
	return true, networkId, nil
}

// Whether the client has an open connection, or one that closed at or after
// since: one probe of network_client_connection's (client_id, connect_time)
// index, whose closed rows live 8 h.
func clientConnectedSinceInTx(ctx context.Context, tx server.PgTx, clientId server.Id, since time.Time) (bool, error) {
	var connected bool
	err := tx.QueryRow(
		ctx,
		`
			SELECT EXISTS (
				SELECT 1
				FROM network_client_connection
				WHERE
					client_id = $1 AND
					(connected OR $2 <= disconnect_time)
			)
		`,
		clientId,
		since,
	).Scan(&connected)
	return connected, err
}

// Reaps the ephemeral client at once, whatever its lease and connections say,
// as POST /sidecar/close does. False when it has no lease or is inactive.
func ReapEphemeralClient(ctx context.Context, clientId server.Id) (reaped bool, returnErr error) {
	if !server.SidecarMg1Linked() {
		return false, nil
	}
	server.Tx(ctx, func(tx server.PgTx) {
		var err error
		reaped, _, err = reapEphemeralClientInTx(ctx, tx, clientId, nil)
		server.Raise(err)
	}, server.TxReadCommitted)
	return reaped, nil
}

// POST /sidecar/close: reaps the client whose close capability this is, at once.
// A capability that is malformed or matches no lease does nothing, and the
// caller answers the same either way, since a beacon reads no answer.
func CloseEphemeralClient(ctx context.Context, capability string) (reaped bool, returnErr error) {
	if !server.SidecarMg1Linked() {
		return false, nil
	}
	capabilityBytes, ok := parseClientCloseCapability(capability)
	if !ok {
		return false, nil
	}
	closeSha256 := clientCloseCapabilitySha256(capabilityBytes)
	server.Tx(ctx, func(tx server.PgTx) {
		var clientId server.Id
		err := tx.QueryRow(
			ctx,
			`SELECT client_id FROM network_client_lease WHERE close_sha256 = $1`,
			closeSha256,
		).Scan(&clientId)
		if errors.Is(err, pgx.ErrNoRows) {
			reaped = false
			return
		}
		server.Raise(err)
		reaped, _, err = reapEphemeralClientInTx(ctx, tx, clientId, nil)
		server.Raise(err)
	}, server.TxReadCommitted)
	return reaped, nil
}

// Where a reap pass stopped: the last lease row it looked at, in (seen_time,
// client_id) order.
type ReapEphemeralClientsCursor struct {
	SeenTime time.Time `json:"seen_time"`
	ClientId server.Id `json:"client_id"`
}

type ReapEphemeralClientsResult struct {
	// lease rows looked at
	LookedCount int `json:"looked_count"`
	// clients reaped
	ReapedCount int `json:"reaped_count"`
	// leases of clients already inactive or gone, deleted
	ClearedCount int `json:"cleared_count"`
	// set when the pass stopped short: where the next starts
	Cursor *ReapEphemeralClientsCursor `json:"cursor,omitempty"`
}

// A lease row a reap pass looks at.
type reapLeaseRow struct {
	clientId server.Id
	seenTime time.Time
}

// The lease rows whose seen_time is before cutoff, after the cursor, in
// (seen_time, client_id) order, at most count: the seen_time index's range.
func readReapLeaseRows(ctx context.Context, conn server.PgConn, cutoff time.Time, after *ReapEphemeralClientsCursor, count int) []reapLeaseRow {
	sql := `
		SELECT client_id, seen_time
		FROM network_client_lease
		WHERE seen_time < $1
		ORDER BY seen_time, client_id
		LIMIT $2
	`
	args := []any{cutoff, count}
	if after != nil {
		sql = `
			SELECT client_id, seen_time
			FROM network_client_lease
			WHERE seen_time < $1 AND (seen_time, client_id) > ($3, $4)
			ORDER BY seen_time, client_id
			LIMIT $2
		`
		args = append(args, after.SeenTime, after.ClientId)
	}
	leaseRows := []reapLeaseRow{}
	rows, err := conn.Query(ctx, sql, args...)
	server.WithPgResult(rows, err, func() {
		for rows.Next() {
			var row reapLeaseRow
			server.Raise(rows.Scan(&row.clientId, &row.seenTime))
			leaseRows = append(leaseRows, row)
		}
	})
	return leaseRows
}

// One pass of taskworker's ephemeral job at now: each ephemeral client with no
// open connection whose lease seen_time and newest disconnect are both over
// ReapAfter before now is reaped, at most ReapPassCount lease rows looked at,
// from after on. Each reap is its own transaction and checks again under its
// locks, so a poll or a connection that lands meanwhile keeps the client. A
// lease whose client is already inactive or gone is deleted.
func ReapEphemeralClients(ctx context.Context, now time.Time, after *ReapEphemeralClientsCursor, settings *NetworkClientLeaseSettings) (*ReapEphemeralClientsResult, error) {
	result := &ReapEphemeralClientsResult{}
	if !server.SidecarMg1Linked() || settings.ReapPassCount <= 0 {
		return result, nil
	}
	cutoff := now.Add(-settings.ReapAfter)
	passCtx, cancel := context.WithTimeout(ctx, settings.ReapPassTimeout)
	defer cancel()

	var leaseRows []reapLeaseRow
	if r := server.HandleError(func() {
		server.Db(passCtx, func(conn server.PgConn) {
			leaseRows = readReapLeaseRows(passCtx, conn, cutoff, after, settings.ReapPassCount)
		})
	}); r != nil {
		return nil, fmt.Errorf("lease reap: %v", r)
	}

	for _, row := range leaseRows {
		if passCtx.Err() != nil {
			break
		}
		result.LookedCount += 1
		result.Cursor = &ReapEphemeralClientsCursor{SeenTime: row.seenTime, ClientId: row.clientId}
		reaped := false
		cleared := false
		if r := server.HandleError(func() {
			server.Tx(passCtx, func(tx server.PgTx) {
				var err error
				reaped, _, err = reapEphemeralClientInTx(passCtx, tx, row.clientId, func(seenTime time.Time) bool {
					if !seenTime.Before(cutoff) {
						return false
					}
					connected, err := clientConnectedSinceInTx(passCtx, tx, row.clientId, cutoff)
					server.Raise(err)
					return !connected
				})
				server.Raise(err)
				if reaped {
					return
				}
				// a lease whose client is inactive or gone outlived it: a binary
				// that did not run the lease effect removed the client
				tag := server.RaisePgResult(tx.Exec(
					passCtx,
					`
						DELETE FROM network_client_lease
						WHERE
							client_id = $1 AND
							NOT EXISTS (
								SELECT 1
								FROM network_client
								WHERE
									network_client.client_id = network_client_lease.client_id AND
									network_client.active = true
							)
					`,
					row.clientId,
				))
				cleared = 0 < tag.RowsAffected()
			}, server.TxReadCommitted)
		}); r != nil {
			if glog.V(1) {
				glog.Infof("[lease]reap of %s failed: %v\n", row.clientId, r)
			}
			continue
		}
		if reaped {
			result.ReapedCount += 1
		}
		if cleared {
			result.ClearedCount += 1
		}
	}
	if result.LookedCount < settings.ReapPassCount && passCtx.Err() == nil {
		// every row past the threshold was looked at: the next pass starts over
		result.Cursor = nil
	}
	return result, nil
}
