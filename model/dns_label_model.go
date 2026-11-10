package model

// §12.5's network labels (DNS-M5-10; R-DNS-5, R-DNS-6, D-DNS-2, D-DNS-10, X5,
// I-14; owner decisions 14 and 97): dns_label, MG2's table by
// plan-DNS-labels-before-m5.
//
//   - A label is 10 characters drawn by sidecar.NewDnsLabel from crypto/rand,
//     never derived from the network id, allocated on first need, immutable and
//     never reassigned. A draw that hits a row or fails DnsNameScreen is drawn
//     again (owner decision 97), up to DnsLabelSettings.DrawCount times.
//   - Labels and aliases share the key (root, label) in every state, and a row
//     is never deleted, so no name a row holds, live, retiring or tombstoned,
//     can be claimed again: by an alias, a rename (DnsNameHeldInTx, which
//     RenameNetwork asks) or a new network.
//   - ResolveNetworkLabel follows D-DNS-2: label, then live alias, then a
//     retiring alias, and the legacy network_name only for interactive input.
//     A tombstone or an ownerless row resolves to nothing.
//   - Deleting a network withdraws every record of its zones and tombstones its
//     label and alias in the deletion's own transaction, the dns lock taken last
//     (DnsWithdrawNetworkInTx).
//
// Nothing here reads or writes dns_label unless the binary links MG2
// (server.SidecarMg2Linked), and nothing touches §12.14's other tables unless it
// links MG5.

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/dnsservice"

	"github.com/urnetwork/server"
)

// dns_label.kind.
const (
	DnsLabelKindLabel = "label"
	DnsLabelKindAlias = "alias"
)

// dns_label.state (D-DNS-10).
const (
	DnsLabelStateLive       = "live"
	DnsLabelStateRetiring   = "retiring"
	DnsLabelStateTombstoned = "tombstoned"
)

// The binary does not link MG2, so dns_label does not exist for it.
var ErrDnsLabelUnlinked = errors.New("dns_label is not linked in this binary")

// DnsLabelSettings.DrawCount draws in a row hit a held name or the screen.
var ErrDnsLabelDrawsSpent = errors.New("dns label draws spent")

// Tunables of label allocation.
type DnsLabelSettings struct {
	// The most draws one allocation makes before it gives up. A draw collides with
	// probability about n / 2^51 for n labels, and the screen refuses few, so a
	// handful never runs out by chance.
	DrawCount int
}

func DefaultDnsLabelSettings() *DnsLabelSettings {
	return &DnsLabelSettings{
		DrawCount: 16,
	}
}

// §21.6's screen of a label draw, behind which ABUSE's ScreenName sits: true
// lets the label be allocated. nil-facts ScreenName is a label draw's rule.
type DnsNameScreen func(label string) bool

// sidecar.ScreenName on a draw, which allows only what it allows outright.
func DefaultDnsNameScreen(label string) bool {
	return sidecar.ScreenName(label, nil) == sidecar.ScreenVerdictAllow
}

// The process's screen and random source, which tests replace.
var dnsLabelDeps = struct {
	stateLock sync.Mutex
	screen    DnsNameScreen
	random    io.Reader
}{
	screen: DefaultDnsNameScreen,
	random: rand.Reader,
}

func currentDnsLabelDeps() (DnsNameScreen, io.Reader) {
	dnsLabelDeps.stateLock.Lock()
	defer dnsLabelDeps.stateLock.Unlock()
	return dnsLabelDeps.screen, dnsLabelDeps.random
}

// Installs a screen, returning what puts the previous one back. nil restores
// the default.
func SetDnsNameScreen(screen DnsNameScreen) (restore func()) {
	if screen == nil {
		screen = DefaultDnsNameScreen
	}
	dnsLabelDeps.stateLock.Lock()
	defer dnsLabelDeps.stateLock.Unlock()
	previous := dnsLabelDeps.screen
	dnsLabelDeps.screen = screen
	return func() {
		dnsLabelDeps.stateLock.Lock()
		defer dnsLabelDeps.stateLock.Unlock()
		dnsLabelDeps.screen = previous
	}
}

// Draws from random in place of crypto/rand: a test's fixed reader forces a
// collision. Returns what puts the previous source back.
func Testing_SetDnsLabelRandom(random io.Reader) (restore func()) {
	dnsLabelDeps.stateLock.Lock()
	defer dnsLabelDeps.stateLock.Unlock()
	previous := dnsLabelDeps.random
	dnsLabelDeps.random = random
	return func() {
		dnsLabelDeps.stateLock.Lock()
		defer dnsLabelDeps.stateLock.Unlock()
		dnsLabelDeps.random = previous
	}
}

// A root as dns_label keys it: lowercase and fully qualified.
func dnsLabelRoot(root string) string {
	root = strings.ToLower(root)
	if !strings.HasSuffix(root, ".") {
		root += "."
	}
	return root
}

// Called with the network just before EnsureNetworkLabelInTx takes the label's
// lock, so a test sees which locks its caller already holds there: a zone
// writer must hold none of the zone's (dns_zone_model.go). nil in production.
var Testing_DnsLabelLockBarrier func(networkId server.Id)

// The network's label in root, allocating one on first need (§12.5): read,
// first publish or order, or /mail/enable. Under the network's label lock, so
// two first needs at once allocate one label.
func EnsureNetworkLabelInTx(ctx context.Context, tx server.PgTx, networkId server.Id, root string) (string, error) {
	if !server.SidecarMg2Linked() {
		return "", ErrDnsLabelUnlinked
	}
	root = dnsLabelRoot(root)
	if barrier := Testing_DnsLabelLockBarrier; barrier != nil {
		barrier(networkId)
	}
	server.RaisePgResult(tx.Exec(
		ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended('dns_label:' || $1::uuid::text, 0))`,
		networkId,
	))
	if label := getNetworkLabelInTx(ctx, tx, networkId, root); label != "" {
		return label, nil
	}
	screen, random := currentDnsLabelDeps()
	now := server.NowUtc()
	for range DefaultDnsLabelSettings().DrawCount {
		label, err := sidecar.NewDnsLabel(random)
		if errors.Is(err, sidecar.ErrDnsLabelDrawRefused) {
			continue
		}
		if err != nil {
			return "", err
		}
		if !screen(label) {
			continue
		}
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO dns_label (root, label, network_id, kind, state, state_time)
				VALUES ($1, $2, $3, $4, $5, $6)
				ON CONFLICT (root, label) DO NOTHING
			`,
			root,
			label,
			networkId,
			DnsLabelKindLabel,
			DnsLabelStateLive,
			now,
		))
		if tag.RowsAffected() == 1 {
			return label, nil
		}
	}
	return "", ErrDnsLabelDrawsSpent
}

// The network's label in root, allocating one on first need, in a transaction
// of its own.
func EnsureNetworkLabel(ctx context.Context, networkId server.Id, root string) (label string, returnErr error) {
	server.Tx(ctx, func(tx server.PgTx) {
		label, returnErr = EnsureNetworkLabelInTx(ctx, tx, networkId, root)
	}, server.TxReadCommitted)
	return
}

// The network's label in root, or "" while it has none. Any state: a deleted
// network's label is tombstoned with its id kept.
func getNetworkLabelInTx(ctx context.Context, query server.PgCanQuery, networkId server.Id, root string) string {
	label := ""
	result, err := query.Query(
		ctx,
		`
			SELECT label FROM dns_label
			WHERE network_id = $1 AND root = $2 AND kind = $3
			ORDER BY label
			LIMIT 1
		`,
		networkId,
		root,
		DnsLabelKindLabel,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&label))
		}
	})
	return label
}

// The network's label in root, or "" while it has none or in a binary that
// does not link MG2. Allocates nothing.
func GetNetworkLabel(ctx context.Context, networkId server.Id, root string) (label string) {
	if !server.SidecarMg2Linked() {
		return ""
	}
	server.Db(ctx, func(conn server.PgConn) {
		label = getNetworkLabelInTx(ctx, conn, networkId, dnsLabelRoot(root))
	})
	return
}

// The text a caller names a network by, as dns_label keys it: lowercase, with
// any .<root> and final dot taken off.
func dnsLabelText(root string, text string) string {
	text = strings.ToLower(strings.TrimSuffix(text, "."))
	return strings.TrimSuffix(text, "."+strings.TrimSuffix(root, "."))
}

// §12.14's ResolveNetworkLabel (D-DNS-2): the network a label, a live alias, a
// retiring alias or, only when interactive, a legacy network_name names in
// root, with what it matched. A tombstone, an ownerless row and a text nothing
// holds answer sidecar.ErrNetworkLabelUnknown, never a guess.
func ResolveNetworkLabel(ctx context.Context, root string, text string, interactive bool) (networkId server.Id, kind sidecar.NetworkLabelKind, returnErr error) {
	if !server.SidecarMg2Linked() {
		returnErr = ErrDnsLabelUnlinked
		return
	}
	root = dnsLabelRoot(root)
	text = dnsLabelText(root, text)
	if text == "" || strings.Contains(text, ".") {
		returnErr = sidecar.ErrNetworkLabelUnknown
		return
	}
	found := false
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT network_id, kind, state FROM dns_label WHERE root = $1 AND label = $2`,
			root,
			text,
		)
		server.WithPgResult(result, err, func() {
			if !result.Next() {
				return
			}
			var holderId *server.Id
			var rowKind string
			var state string
			server.Raise(result.Scan(&holderId, &rowKind, &state))
			if holderId == nil {
				return
			}
			switch {
			case rowKind == DnsLabelKindLabel && state == DnsLabelStateLive:
				networkId, kind, found = *holderId, sidecar.NetworkLabelKindLabel, true
			case rowKind == DnsLabelKindAlias && state == DnsLabelStateLive:
				networkId, kind, found = *holderId, sidecar.NetworkLabelKindAlias, true
			case rowKind == DnsLabelKindAlias && state == DnsLabelStateRetiring:
				networkId, kind, found = *holderId, sidecar.NetworkLabelKindRetiringAlias, true
			}
		})
		if found || !interactive {
			return
		}
		// a label row of any state holds its name from every legacy match too
		held := false
		result, err = conn.Query(ctx, `SELECT EXISTS (SELECT 1 FROM dns_label WHERE root = $1 AND label = $2)`, root, text)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&held))
			}
		})
		if held {
			return
		}
		result, err = conn.Query(ctx, `SELECT network_id FROM network WHERE network_name = $1`, text)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&networkId))
				kind = sidecar.NetworkLabelKindLegacyName
				found = true
			}
		})
	})
	if !found {
		return server.Id{}, "", sidecar.ErrNetworkLabelUnknown
	}
	return
}

// Whether any dns_label row holds name, as a label or an alias, live, retiring
// or tombstoned, in any root (§12.5): what RenameNetwork, alias claims and a new
// network's name refuse. False in a binary that does not link MG2, which holds
// no row.
func DnsNameHeldInTx(ctx context.Context, query server.PgCanQuery, name string) bool {
	if !server.SidecarMg2Linked() {
		return false
	}
	held := false
	result, err := query.Query(
		ctx,
		`SELECT EXISTS (SELECT 1 FROM dns_label WHERE label = $1)`,
		strings.ToLower(name),
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&held))
		}
	})
	return held
}

// The NetworkLabelHolder RenameNetwork and network creation ask (TEAM-M1-09):
// dns_label itself.
type dnsLabelHolder struct{}

func (dnsLabelHolder) HoldsNameInTx(ctx context.Context, query server.PgCanQuery, name string) bool {
	return DnsNameHeldInTx(ctx, query, name)
}

// Every root the network has a zone or a name in.
func getNetworkDnsRootsInTx(ctx context.Context, query server.PgCanQuery, networkId server.Id) []string {
	roots := map[string]bool{}
	if server.SidecarMg2Linked() {
		result, err := query.Query(ctx, `SELECT DISTINCT root FROM dns_label WHERE network_id = $1`, networkId)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var root string
				server.Raise(result.Scan(&root))
				roots[root] = true
			}
		})
	}
	if server.SidecarMg5Linked() {
		result, err := query.Query(ctx, `SELECT root FROM network_dns_zone WHERE network_id = $1`, networkId)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var root string
				server.Raise(result.Scan(&root))
				roots[root] = true
			}
		})
	}
	ordered := make([]string, 0, len(roots))
	for root := range roots {
		ordered = append(ordered, root)
	}
	return ordered
}

// Moves the label map's cursor for the network's names in root to serial, in
// the transaction that took it (DnsLabelMapSince).
func touchDnsLabelMapInTx(ctx context.Context, tx server.PgTx, root string, networkId server.Id, serial uint64) {
	if !server.SidecarMg2Linked() {
		return
	}
	server.RaisePgResult(tx.Exec(
		ctx,
		`UPDATE dns_label SET change_serial = $3 WHERE network_id = $1 AND root = $2`,
		networkId,
		root,
		dnsSerialColumn(serial),
	))
}

// A network's deletion in its own transaction (I-14, D-DNS-10): every record
// of each of its zones withdrawn in one journal write a zone, its zone rows
// gone, and its label and alias tombstoned, never freed. The dns lock is taken
// last: call this after the deletion's other locks. A root that is restoring
// refuses with dnsservice.ErrDnsRestoring, which fails the deletion for a
// retry, since no write may take a serial then.
func DnsWithdrawNetworkInTx(ctx context.Context, tx server.PgTx, networkId server.Id) error {
	now := server.NowUtc()
	for _, root := range getNetworkDnsRootsInTx(ctx, tx, networkId) {
		var serial uint64
		if server.SidecarMg5Linked() {
			lockDnsZoneInTx(ctx, tx, root, networkId)
			deletes := []dnsservice.DnsRecordKey{}
			for _, row := range getDnsZoneRowsInTx(ctx, tx, root, networkId) {
				deletes = append(deletes, row.set.Key())
			}
			if 0 < len(deletes) {
				var err error
				serial, err = dnsCommitInTx(ctx, tx, &dnsservice.DnsZoneWrite{
					Root:      root,
					NetworkId: dnsSidecarId(networkId),
					Deletes:   deletes,
				}, now, nil)
				if err != nil {
					return err
				}
				root := root
				server.AddTxPostCommit(tx, "dns_write:"+root, func() any {
					// the network is gone, so its sidecars need no hint
					DnsWritePost(context.Background(), root, serial, server.Id{})
					return nil
				})
			}
			server.RaisePgResult(tx.Exec(
				ctx,
				`DELETE FROM network_dns_zone WHERE root = $1 AND network_id = $2`,
				root,
				networkId,
			))
		}
		if server.SidecarMg2Linked() {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					UPDATE dns_label
					SET state = $3, state_time = $4, change_serial = GREATEST(change_serial, $5)
					WHERE network_id = $1 AND root = $2 AND state <> $3
				`,
				networkId,
				root,
				DnsLabelStateTombstoned,
				now,
				dnsSerialColumn(serial),
			))
		}
	}
	return nil
}

// One name of the label map: what tlsedge and mxedge route by (§12.9,
// plan-TLS-edge-data-source), with the network's suspensions.
type DnsLabelMapEntry struct {
	Root  string
	Label string
	// nil for a restore's ownerless name, which routes nothing (§12.5)
	NetworkId *server.Id
	Kind      string
	State     string
	// the serial of the last change the map shows
	ChangeSerial uint64
	// §12.9's zone suspension: set while it holds, with its end if it has one
	SuspendReason string
	SuspendUntil  *time.Time
	// §12.9's suspended hosts in the network's zone, as owners
	WithheldHosts []string
}

// The label map at one moment, or its changes since a serial.
type DnsLabelMap struct {
	Root string
	// The highest change serial read: the next DnsLabelMapSince's after. A read
	// that found nothing new answers its own after.
	Serial uint64
	// The root's last restore_serial (§12.6). A reader that sees it move takes a
	// new snapshot, since a Postgres restore can leave its cursor above every
	// change serial the restored rows hold.
	RestoreSerial uint64
	Entries       []*DnsLabelMapEntry
}

// Every name of root with its suspensions, and the serial to follow from.
func DnsLabelMapSnapshot(ctx context.Context, root string) (*DnsLabelMap, error) {
	return readDnsLabelMap(ctx, root, nil)
}

// The names of root whose map entry changed above after, in change serial
// order, with their suspensions as they stand now (plan-TLS-edge-data-source:
// tlsedge reads it every 5 s). Serial order is commit order, since a change
// takes its serial under dns_root's lock, so a reader that follows Serial misses
// nothing.
func DnsLabelMapSince(ctx context.Context, root string, after uint64) (*DnsLabelMap, error) {
	return readDnsLabelMap(ctx, root, &after)
}

func readDnsLabelMap(ctx context.Context, root string, after *uint64) (*DnsLabelMap, error) {
	if !server.SidecarMg2Linked() {
		return nil, ErrDnsLabelUnlinked
	}
	root = dnsLabelRoot(root)
	labelMap := &DnsLabelMap{Root: root, Entries: []*DnsLabelMapEntry{}}
	if after != nil {
		labelMap.Serial = *after
	}
	server.Tx(ctx, func(tx server.PgTx) {
		labelMap.Entries = []*DnsLabelMapEntry{}
		afterColumn := int64(-1)
		if after != nil {
			afterColumn = dnsSerialColumn(*after)
		}
		result, err := tx.Query(
			ctx,
			`
				SELECT label, network_id, kind, state, change_serial
				FROM dns_label
				WHERE root = $1 AND $2 < change_serial
				ORDER BY change_serial, label
			`,
			root,
			afterColumn,
		)
		networkEntries := map[server.Id][]*DnsLabelMapEntry{}
		server.WithPgResult(result, err, func() {
			for result.Next() {
				entry := &DnsLabelMapEntry{Root: root}
				var changeSerial int64
				server.Raise(result.Scan(&entry.Label, &entry.NetworkId, &entry.Kind, &entry.State, &changeSerial))
				entry.ChangeSerial = dnsSerialFromColumn(changeSerial)
				labelMap.Serial = max(labelMap.Serial, entry.ChangeSerial)
				labelMap.Entries = append(labelMap.Entries, entry)
				if entry.NetworkId != nil {
					networkEntries[*entry.NetworkId] = append(networkEntries[*entry.NetworkId], entry)
				}
			}
		})
		if !server.SidecarMg5Linked() {
			return
		}
		if state := getDnsRootStateInTx(ctx, tx, root); state != nil {
			labelMap.RestoreSerial = state.RestoreSerial
		}
		now := server.NowUtc()
		for networkId, entries := range networkEntries {
			var suspendReason *string
			var suspendUntil *time.Time
			result, err := tx.Query(
				ctx,
				`SELECT suspend_reason, suspend_until FROM network_dns_zone WHERE root = $1 AND network_id = $2`,
				root,
				networkId,
			)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&suspendReason, &suspendUntil))
				}
			})
			withheldHosts := []string{}
			result, err = tx.Query(
				ctx,
				`
					SELECT owner FROM network_dns_record
					WHERE network_id = $1 AND root = $2 AND kind = $3
					ORDER BY owner
				`,
				networkId,
				root,
				string(dnsservice.DnsRecordKindWithheld),
			)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var owner string
					server.Raise(result.Scan(&owner))
					withheldHosts = append(withheldHosts, owner)
				}
			})
			for _, entry := range entries {
				if suspendReason != nil && (suspendUntil == nil || now.Before(*suspendUntil)) {
					entry.SuspendReason = *suspendReason
					entry.SuspendUntil = suspendUntil
				}
				entry.WithheldHosts = withheldHosts
			}
		}
	}, pgx.ReadOnly, pgx.RepeatableRead)
	return labelMap, nil
}
