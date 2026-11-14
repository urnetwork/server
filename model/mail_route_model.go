package model

// §15.15's route projection (MAIL-M7-22; D-MAIL-1, I-13, I-14, QD-480): Redis
// {mxr_<net>}, what mxedge's router routes an SNI by, so a connection never
// queries Postgres (I-12), and POST /mail/enable's write.
//
//   - <net> is the SNI's routable part (MailRouteName): a network's label or
//     its alias, which mx.<net>.<root> names. A Team scope's and a custom MX
//     host's keys are named by the same function and written by nothing yet:
//     scope routes wait on team_mail's member scopes (MAIL-M10-04) and custom
//     MX hosts on §12.8's custom-host rows, so both route nothing.
//   - The value is one JSON MailRoute: the network, the host its receivers
//     listen at (mx), flags, and the receivers whose key for that host is
//     published, newest first, at most 64 (plan-MAIL-hints #2), which the
//     router hints on a miss.
//   - A name routes while its network has a mail_network row, the name is the
//     network's live label or a live alias, neither the zone nor mx under the
//     name is withheld (§12.9's SuspendZone and SuspendHost), and no direct MX
//     of the network is verified or failing (§15.4: §12.8 then points mx at
//     the receiver itself). Every other name of the network has its key
//     deleted, so a suspended, direct-MX, tombstoned or mail-off network
//     closes in the handshake.
//   - ProjectMailRoutes writes one network's keys: after POST /mail/enable,
//     a host key's change and a direct MX's change, by their callers. The
//     pass (ProjectMailRoutesPass, work.MailRouteProjection) follows the label
//     map's changes (DnsLabelMapSince), which every suspension, alias change
//     and network deletion moves, and rewrites every mail network's keys a
//     page a run, so a lost Redis is whole again within one round. Each key
//     expires RouteTtl after its last write, which a round renews: a name
//     Postgres no longer lists, or a projection that stopped, routes nothing
//     past it.
//
// Nothing durable lives in a key (§19.7). Nothing here runs unless the binary
// links MG2, MG5 and MG7: a read answers no route and a write ErrMailUnlinked.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/dnsservice"

	"github.com/urnetwork/server"
)

// MailRoute.Flags' bits.
const (
	// The name is the network's alias, not its label.
	MailRouteFlagAlias uint32 = 1 << iota
	// The name is a Team scope's: Host is mx.<scope>.
	MailRouteFlagScope
	// The name is a strict-mode custom MX host's full name.
	MailRouteFlagCustomHost
)

// The host every network's receivers listen at (§15.5), mail_host_key.host's
// and smtp/25/'s.
const mailRouteHost = "mx"

// One {mxr_<net>} value.
type MailRoute struct {
	NetworkId server.Id `json:"network_id"`
	// mx, or mx.<scope>: the router Finds <network_id>:smtp/25/<host>.
	Host  string `json:"host"`
	Flags uint32 `json:"flags"`
	// The receivers whose key for Host is published, newest first.
	ReceiverSidecarIds []server.Id `json:"receiver_sidecar_ids"`
}

// Tunables of the projection.
type MailRouteSettings struct {
	// A key expires this long after its last write. The pass renews every key
	// each round, so this bounds how long a route outlives a projection that
	// stopped. Non-positive keeps keys until they are deleted.
	RouteTtl time.Duration
	// The most receivers a value lists (plan-MAIL-hints #2: 64).
	ReceiverMaxCount int
	// Mail networks one run of the pass rewrites, and how often a round of
	// them starts again from the first.
	PageNetworkCount int
	// One Redis call's bound.
	CallTimeout time.Duration
}

// An hour's expiry over a pass that renews within minutes.
func DefaultMailRouteSettings() *MailRouteSettings {
	return &MailRouteSettings{
		RouteTtl:         time.Hour,
		ReceiverMaxCount: 64,
		PageNetworkCount: 500,
		CallTimeout:      5 * time.Second,
	}
}

// The Redis key of name's route.
func MailRouteKey(name string) string {
	return fmt.Sprintf("{mxr_%s}", name)
}

// The key the pass sets at the end of a run: while it stands, Redis has not
// been lost since the pass last wrote, and a run that finds it gone starts a
// round at once.
const mailRouteStandingKey = "{mxr_}standing"

// The routable part of an SNI under root: <net> of mx.<net>.<root>, which is a
// label or an alias, or <scope>.<net> of a scope's mx.<scope>.<net>.<root>;
// any other name is a custom MX host and routes by its full name. Lowercase,
// with no final dot. "" for an empty name.
func MailRouteName(root string, sni string) string {
	sni = strings.TrimSuffix(strings.ToLower(sni), ".")
	root = strings.TrimSuffix(dnsLabelRoot(root), ".")
	if within, found := strings.CutSuffix(sni, "."+root); found && root != "" {
		if name, found := strings.CutPrefix(within, mailRouteHost+"."); found && name != "" {
			return name
		}
	}
	return sni
}

func mailRouteLinked() bool {
	return server.SidecarMg2Linked() && server.SidecarMg5Linked() && server.SidecarMg7Linked()
}

// Runs callback on Redis within settings' bound, a failure returned.
func mailRouteRedis(ctx context.Context, settings *MailRouteSettings, callback func(client server.RedisClient) error) error {
	callCtx, cancel := context.WithTimeout(ctx, settings.CallTimeout)
	defer cancel()
	return server.RedisWithDeadline(callCtx, callback)
}

// The route name stands at, nil when it routes nothing: the router's read,
// behind its in-process cache (MAIL-M7-19).
func GetMailRoute(ctx context.Context, name string) (*MailRoute, error) {
	if name == "" {
		return nil, nil
	}
	var route *MailRoute
	err := mailRouteRedis(ctx, DefaultMailRouteSettings(), func(client server.RedisClient) error {
		value, err := client.Get(ctx, MailRouteKey(name)).Bytes()
		if errors.Is(err, redis.Nil) {
			return nil
		}
		if err != nil {
			return err
		}
		decoded := &MailRoute{}
		if err := json.Unmarshal(value, decoded); err != nil || decoded.NetworkId == (server.Id{}) || decoded.Host == "" {
			// not a value this writes: no route, and the next projection rewrites it
			return nil
		}
		route = decoded
		return nil
	})
	return route, err
}

// What one network's projection asks of Redis.
type mailRouteChange struct {
	// name to its route
	routes map[string]*MailRoute
	// the network's names that route nothing
	removed []string
}

// One network's routes as Postgres has them now, in one read-only snapshot.
func readMailRouteChange(ctx context.Context, networkId server.Id, root string, settings *MailRouteSettings) *mailRouteChange {
	change := &mailRouteChange{routes: map[string]*MailRoute{}}
	server.Tx(ctx, func(tx server.PgTx) {
		change.routes = map[string]*MailRoute{}
		change.removed = nil

		type labelRow struct {
			label string
			kind  string
			state string
		}
		labels := []*labelRow{}
		result, err := tx.Query(
			ctx,
			`SELECT label, kind, state FROM dns_label WHERE network_id = $1 AND root = $2 ORDER BY label`,
			networkId,
			root,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				row := &labelRow{}
				server.Raise(result.Scan(&row.label, &row.kind, &row.state))
				labels = append(labels, row)
			}
		})
		for _, row := range labels {
			change.removed = append(change.removed, row.label)
		}
		if getMailNetworkInQuerier(ctx, tx, networkId) == nil {
			return
		}

		directMx := false
		server.Raise(tx.QueryRow(
			ctx,
			`SELECT EXISTS (SELECT 1 FROM mail_direct_mx WHERE network_id = $1 AND state IN ($2, $3))`,
			networkId,
			string(MailDirectMxStateVerified),
			string(MailDirectMxStateFailing),
		).Scan(&directMx))
		if directMx {
			return
		}

		// suspended while the reason is set, a passed until included, as a write
		// to the zone is refused until the lift (getDnsZoneSuspendedInTx)
		if getDnsZoneSuspendedInTx(ctx, tx, root, networkId) {
			return
		}
		withheld := []string{}
		result, err = tx.Query(
			ctx,
			`SELECT owner FROM network_dns_record WHERE network_id = $1 AND root = $2 AND kind = $3`,
			networkId,
			root,
			string(dnsservice.DnsRecordKindWithheld),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var owner string
				server.Raise(result.Scan(&owner))
				withheld = append(withheld, owner)
			}
		})

		receiverSidecarIds := []server.Id{}
		result, err = tx.Query(
			ctx,
			`
				SELECT receiver_sidecar_id FROM mail_host_key
				WHERE network_id = $1 AND host = $2 AND state = $3
				ORDER BY publish_time DESC, receiver_sidecar_id, spki_sha256
			`,
			networkId,
			mailRouteHost,
			string(MailHostKeyStatePublished),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var receiverSidecarId server.Id
				server.Raise(result.Scan(&receiverSidecarId))
				if !slices.Contains(receiverSidecarIds, receiverSidecarId) && len(receiverSidecarIds) < settings.ReceiverMaxCount {
					receiverSidecarIds = append(receiverSidecarIds, receiverSidecarId)
				}
			}
		})

		change.removed = nil
		for _, row := range labels {
			mailHost := mailRouteHost + "." + row.label + "." + root
			routes := row.state == DnsLabelStateLive &&
				!slices.ContainsFunc(withheld, func(owner string) bool { return dnsOwnerAtOrBelow(mailHost, owner) })
			if !routes {
				change.removed = append(change.removed, row.label)
				continue
			}
			route := &MailRoute{NetworkId: networkId, Host: mailRouteHost, ReceiverSidecarIds: receiverSidecarIds}
			if row.kind == DnsLabelKindAlias {
				route.Flags |= MailRouteFlagAlias
			}
			change.routes[row.label] = route
		}
	}, pgx.ReadOnly, pgx.RepeatableRead)
	return change
}

// Applies one network's change in one pipeline: each route set, each other
// name deleted. The keys sit in different slots, so the pipeline is no
// transaction: a failure leaves some written, and the next projection writes
// them all again.
func writeMailRouteChange(ctx context.Context, change *mailRouteChange, settings *MailRouteSettings) error {
	if len(change.routes) == 0 && len(change.removed) == 0 {
		return nil
	}
	values := map[string][]byte{}
	for name, route := range change.routes {
		value, err := json.Marshal(route)
		if err != nil {
			return err
		}
		values[name] = value
	}
	return mailRouteRedis(ctx, settings, func(client server.RedisClient) error {
		pipe := client.Pipeline()
		for name, value := range values {
			pipe.Set(ctx, MailRouteKey(name), value, max(settings.RouteTtl, 0))
		}
		for _, name := range change.removed {
			pipe.Del(ctx, MailRouteKey(name))
		}
		_, err := pipe.Exec(ctx)
		return err
	})
}

// What one network's projection did.
type MailRouteProjection struct {
	// The names that route, and the network's names whose key was deleted.
	Routed  []string
	Removed []string
}

// Writes networkId's routes under root as Postgres has them now (see the file
// header): called after every change that moves one, never inside the change's
// transaction, since Redis cannot join it. A failure leaves the keys as they
// were or partly written, and the pass rewrites them within its round, so a
// caller logs it and goes on.
func ProjectMailRoutes(ctx context.Context, networkId server.Id, root string) (*MailRouteProjection, error) {
	return projectMailRoutes(ctx, networkId, root, DefaultMailRouteSettings())
}

func projectMailRoutes(ctx context.Context, networkId server.Id, root string, settings *MailRouteSettings) (*MailRouteProjection, error) {
	if !mailRouteLinked() {
		return nil, ErrMailUnlinked
	}
	if root == "" {
		return nil, ErrDnsUnlinked
	}
	root = dnsLabelRoot(root)
	change := readMailRouteChange(ctx, networkId, root, settings)
	if err := writeMailRouteChange(ctx, change, settings); err != nil {
		return nil, err
	}
	projection := &MailRouteProjection{Routed: []string{}, Removed: change.removed}
	for name := range change.routes {
		projection.Routed = append(projection.Routed, name)
	}
	slices.Sort(projection.Routed)
	return projection, nil
}

// Where the pass stands between runs: carried in its task's arguments.
type MailRouteCursor struct {
	// The label map's serial the pass followed through, and the root's restore
	// serial it last saw (DnsLabelMap).
	Serial        uint64 `json:"serial"`
	RestoreSerial uint64 `json:"restore_serial"`
	// The last mail network this round rewrote; nil at a round's start.
	After *server.Id `json:"after,omitempty"`
	// Whether a cursor was ever taken: false reads the label map from its
	// start.
	Started bool `json:"started"`
}

// What one run of the pass did.
type MailRoutePass struct {
	// Where the next run starts.
	Cursor *MailRouteCursor `json:"cursor"`
	// Networks projected for a label map change, and for the round's page.
	ChangedNetworkCount int `json:"changed_network_count"`
	PageNetworkCount    int `json:"page_network_count"`
	// The round reached its end with this run.
	RoundDone bool `json:"round_done"`
	// Redis held no standing mark, so the round started again from the first
	// network.
	Rebuilt bool `json:"rebuilt"`
}

// One run of the pass under root (see the file header): the networks whose
// label map entry changed since cursor, then one page of the round over every
// mail network. The cursor moves only past what Redis took, so a failed run
// repeats its work. cursor nil starts from nothing.
func ProjectMailRoutesPass(ctx context.Context, root string, cursor *MailRouteCursor) (*MailRoutePass, error) {
	return projectMailRoutesPass(ctx, root, cursor, DefaultMailRouteSettings())
}

func projectMailRoutesPass(ctx context.Context, root string, cursor *MailRouteCursor, settings *MailRouteSettings) (*MailRoutePass, error) {
	if !mailRouteLinked() {
		return nil, ErrMailUnlinked
	}
	if root == "" {
		return nil, ErrDnsUnlinked
	}
	root = dnsLabelRoot(root)
	next := &MailRouteCursor{}
	if cursor != nil {
		*next = *cursor
	}
	pass := &MailRoutePass{Cursor: next}

	standing := false
	err := mailRouteRedis(ctx, settings, func(client server.RedisClient) error {
		count, err := client.Exists(ctx, mailRouteStandingKey).Result()
		standing = 0 < count
		return err
	})
	if err != nil {
		return pass, err
	}
	if !standing && next.After != nil {
		// Redis lost what the round wrote: start it again
		next.After = nil
		pass.Rebuilt = true
	}

	// the label map's changes: suspensions, aliases, tombstones, deletions
	var labelMap *DnsLabelMap
	if next.Started {
		labelMap, err = DnsLabelMapSince(ctx, root, next.Serial)
	} else {
		labelMap, err = DnsLabelMapSnapshot(ctx, root)
	}
	if err != nil {
		return pass, err
	}
	if next.Started && labelMap.RestoreSerial != next.RestoreSerial {
		// a restore can leave the cursor above every restored row's serial
		// (DnsLabelMap): read the map whole
		labelMap, err = DnsLabelMapSnapshot(ctx, root)
		if err != nil {
			return pass, err
		}
	}
	changedNetworkIds := []server.Id{}
	for _, entry := range labelMap.Entries {
		if entry.NetworkId != nil && !slices.Contains(changedNetworkIds, *entry.NetworkId) {
			changedNetworkIds = append(changedNetworkIds, *entry.NetworkId)
		}
		if entry.NetworkId == nil {
			// a restore's ownerless name routes nothing (§12.5)
			if err := writeMailRouteChange(ctx, &mailRouteChange{removed: []string{entry.Label}}, settings); err != nil {
				return pass, err
			}
		}
	}
	for _, networkId := range changedNetworkIds {
		if _, err := projectMailRoutes(ctx, networkId, root, settings); err != nil {
			return pass, err
		}
		pass.ChangedNetworkCount += 1
	}
	next.Serial = labelMap.Serial
	next.RestoreSerial = labelMap.RestoreSerial
	next.Started = true

	// the round's page
	pageNetworkIds := []server.Id{}
	server.Db(ctx, func(conn server.PgConn) {
		pageNetworkIds = []server.Id{}
		args := []any{max(settings.PageNetworkCount, 1)}
		after := ``
		if next.After != nil {
			args = append(args, *next.After)
			after = ` WHERE $2 < network_id`
		}
		result, err := conn.Query(
			ctx,
			`SELECT network_id FROM mail_network`+after+` ORDER BY network_id LIMIT $1`,
			args...,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var networkId server.Id
				server.Raise(result.Scan(&networkId))
				pageNetworkIds = append(pageNetworkIds, networkId)
			}
		})
	})
	for _, networkId := range pageNetworkIds {
		if _, err := projectMailRoutes(ctx, networkId, root, settings); err != nil {
			return pass, err
		}
		pass.PageNetworkCount += 1
		next.After = &networkId
	}
	if len(pageNetworkIds) < max(settings.PageNetworkCount, 1) {
		next.After = nil
		pass.RoundDone = true
	}
	err = mailRouteRedis(ctx, settings, func(client server.RedisClient) error {
		return client.Set(ctx, mailRouteStandingKey, "1", 0).Err()
	})
	return pass, err
}

// POST /mail/enable's write (§15.15; R-MAIL-11, D-MAIL-1): the network's
// mail_network row and §12's mail records in one transaction, so mail is never
// on in one and off in the other, then its routes. network_verified is not
// asked: only MTA-STS and internet mail need it. Enabling again keeps the row
// and its create_time and writes no record. A restoring root answers
// dnsservice.ErrDnsRestoring and a suspended zone ErrDnsZoneSuspended, with
// nothing written; a binary without MG2, MG5 and MG7 answers ErrMailUnlinked.
// The M8 MTA-STS publish joins this transaction with SMTP's task.
func EnableMail(ctx context.Context, networkId server.Id, root string) (*MailNetwork, error) {
	if !mailRouteLinked() {
		return nil, ErrMailUnlinked
	}
	if root == "" {
		return nil, ErrDnsUnlinked
	}
	var mailNetwork *MailNetwork
	err := dnsCatchRefusal(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			enabled, _, err := EnableMailInTx(ctx, tx, networkId)
			if err != nil {
				panic(dnsWriteRefusal{err: err})
			}
			if _, err := DnsSetMailRecordsInTx(ctx, tx, networkId, root, true, nil); err != nil {
				panic(dnsWriteRefusal{err: err})
			}
			mailNetwork = enabled
		}, server.TxReadCommitted)
	})
	if err != nil {
		return nil, err
	}
	if _, err := ProjectMailRoutes(ctx, networkId, root); err != nil {
		// the pass writes it within its round
		glog.Warningf("[mail]routes of a network enabled just now wait for the pass: %v\n", err)
	}
	return mailNetwork, nil
}

// GET /mail/status's body (QD-478): what a receiver reads at most every 30 s
// for RCPT's 452 4.2.2, and what a device that missed a receiver_needed hint
// reads direct_mx from.
type MailStatus struct {
	// The network has a mail_network row.
	Enabled bool `json:"enabled"`
	// The network is at or past its storage limit (§20.7).
	StorageFull bool `json:"storage_full"`
	// The network is spent (§20.6).
	Spent bool `json:"spent"`
	// The network names a direct MX, in any state (§15.4).
	DirectMx bool `json:"direct_mx"`
}

// The network's mail status. A binary that does not link MG7 answers a status
// with mail off.
func GetMailStatus(ctx context.Context, networkId server.Id) (*MailStatus, error) {
	status := &MailStatus{}
	if !server.SidecarMg7Linked() {
		return status, nil
	}
	status.Enabled = GetMailNetwork(ctx, networkId) != nil
	status.DirectMx = 0 < len(ListMailDirectMx(ctx, networkId))
	if server.SidecarMg2Linked() {
		state, err := GetNetworkQuotaState(ctx, networkId)
		if err != nil {
			return nil, err
		}
		if state != nil {
			status.StorageFull = state.StorageOver
			status.Spent = state.SpentTime != nil
		}
	}
	return status, nil
}

// The name the mail host key effect registers under.
const RemovalEffectMailHostKey = "mail_host_key"

// The removal effect (I-15): each removed sidecar's published host keys retire
// in the deactivation's transaction, so no key of a removed receiver stays
// published. A direct MX that names it goes with MAIL-M7-23's effect, which
// also takes back §12.8's address record. Their TLSA goes in the same
// transaction by §12's effect (RemovalEffectDnsTlsa), whichever of the two
// runs first; §19.10 applying a removal again retires nothing. The network's
// routes follow at the pass, since Redis cannot join the transaction and the
// pass rewrites every mail network's keys each round. A binary that does not
// link MG7 has no rows and does nothing.
func applyMailHostKeyRemovalEffectInTx(ctx context.Context, tx server.PgTx, removal *Removal) error {
	if !server.SidecarMg7Linked() {
		return nil
	}
	for _, subject := range removal.SubjectsOf(sidecar.RevocationKindSidecar) {
		if _, err := RetireMailHostKeysInTx(ctx, tx, subject.NetworkId, subject.SubjectId); err != nil {
			return err
		}
	}
	return nil
}

func init() {
	RegisterRemovalEffect(RemovalEffectMailHostKey, applyMailHostKeyRemovalEffectInTx)
}
