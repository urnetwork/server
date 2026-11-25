package model

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
)

// Counting sidecar clients (DESIGN.md 20.8, X10): §20.8's table, row by row.
//
//	client                                   concurrent_clients   100 create cap, peer valve   sidecar_clients
//	app device client                        counts               counts                       -
//	sidecar child of a device client         exempt               -                            counts
//	a sidecar's forwarding client and its    exempt               exempt                       - (its sidecar
//	  window clients (§18.6)                                                                     counts once)
//	standalone, declared no-egress           exempt               exempt                       counts
//	standalone, with ur proxy or public exits counts              counts                       counts
//
// The sidecar mark is a sidecar's own client: network_client_owner.strict_exp (§7.4), on a
// client that is no child of a strict_exp client and not forward_only. App device clients
// are minted admin_refused and carry no strict_exp, so they never count here. A client's
// children inherit its marks at mint (clientMarks.union), so a sidecar's links' window
// clients (§17.5), its forwarding client F (owner decision 197) and F's own window clients
// are strict_exp too, but each is the child of a strict_exp client, S's or F, and F and its
// children are also forward_only: none is marked, and the sidecar counts once, by its own
// client. That client is top-level, or the child of its device's client, which has no
// strict_exp (§5.6's sdk host and pairing; owner decision 4), as newClientIsOwnAclSubject
// reads it too. The children of either kind are not top-level, so the other two columns
// exempt them already.
//
// A sidecar child is in no peer registry, since only top-level clients register
// (peer_model.go), so every marked client, child or top-level, whatever its category, joins
// one zset of its own, {sq_<network_id>}connected_sidecar, scored by its expiry in ms as
// the other connected zsets are, the key living 48 h past its last write
// (plan-QUOTA-sidecar-clients-store; §2.8 api-13's {sq_} family, which §19.7 keeps out of
// the rdb bundles). An entry is live while its expiry is past now, and sidecar_clients
// counts the live entries.
//
// The limit is a hard cap at connect (sidecar-clients-quota-race-connect-admission): the
// nomination's gate (AdmitSidecarClient) counts the live entries and adds the client's in
// one script under the network's hash tag, so nominations racing for a network's last
// slots cannot each pass, as they did while the gate only read the count and the entry was
// written later, at the announce or the resident's first poll. A client whose live entry a
// connection already holds is no new connection: the gate keeps its entry and passes it.
// From the gate on, the entry is its connection's:
//
//   - a nomination that fails after its gate added the entry releases it
//     (ReleaseSidecarClientAdmission);
//   - the resident's poll refreshes it; puts it back when it was lost while the resident's
//     last write was still live (Redis lost it, or a removal raced), whatever the count, so
//     connected clients stay; and admits the client again when the entry lapsed, the
//     resident having had no transport for a whole ttl, a refusal closing the resident
//     (RefreshNetworkSidecarClient);
//   - the resident's close removes it (RemoveNetworkSidecarClient).
//
// A release and a removal each remove the entry only while it still holds the expiry their
// own connection last wrote, so a newer connection of the client, a replacing resident's or
// a concurrent nomination's, keeps the entry it wrote. What stays soft: once Redis loses
// the set, until every connected resident's next poll puts its entry back
// (ExchangeResidentTtl / 4), connects are judged against the entries put back so far.
//
// Enrollment asks SidecarClientsExceeded, one ZCOUNT, as an early refusal before a client
// is minted. It reserves nothing, so enrollments racing below the limit may each mint a
// client, and the gate admits the limit's worth of them.
//
// A declared no-egress top-level client takes NetworkPeerCategorySidecar: unlisted, no
// peer subscription, in no {np_} zset, so GetNetworkEnforceableConnectedCount leaves it
// out, and the create cap's and the peer valve's counts skip it (the NOT EXISTS on
// declares_no_egress, run only where MG0 is linked). A standalone sidecar with egress
// stays an ordinary client and joins the zset too, so it counts in all three columns.
//
// The gate and the resident's construction (CountsAsSidecarClient) each read the mark from
// getSidecarClientMark, so they cannot disagree.
//
// The limit is dark behind enforce_sidecar_limits, with no I/O while off: no mark is read
// at a gate or a resident, no zset is written, and both checks pass. Turned on, a client
// counts from its resident's next construction, and clients already connected stay
// connected (§20.7's "connected clients stay").

// zset: every admitted marked client of the network, scored by its expiry in unix ms.
func networkSidecarConnectedKey(networkId server.Id) string {
	return fmt.Sprintf("{sq_%s}connected_sidecar", networkId)
}

// How the connected sidecar set is kept.
type SidecarClientCountSettings struct {
	// The key's ttl past its last write, which §2.8 gives every {sq_} key (48 h). A network
	// with no connected sidecar clears within it.
	KeyTtl time.Duration
	// The most expired entries one write removes, so a write's work stays bounded.
	PruneLimit int64
}

func DefaultSidecarClientCountSettings() *SidecarClientCountSettings {
	return &SidecarClientCountSettings{
		KeyTtl:     48 * time.Hour,
		PruneLimit: 64,
	}
}

var sidecarClientCountSettings = DefaultSidecarClientCountSettings()

// Whether sidecar clients are counted now: the limit is enforced and this binary links
// MG0, whose owner row holds the mark. False means no mark is read and nothing is written.
func SidecarClientCountingOn() bool {
	return Pro().EnforceSidecarLimits && server.SidecarMg0Linked()
}

// Ages out at most ARGV[2] entries whose expiry is at or before ARGV[1], now in unix ms,
// from the set KEYS[1]: what the admission and the refresh run first, so a write's work
// stays bounded.
const sidecarClientPruneLua = `
local expired = redis.call('ZRANGEBYSCORE', KEYS[1], '-inf', ARGV[1], 'LIMIT', 0, tonumber(ARGV[2]))
for _, expired_member in ipairs(expired) do
	redis.call('ZREM', KEYS[1], expired_member)
end
`

// The gate's one step. KEYS[1] is the network's set; ARGV[1] now and ARGV[2] the prune
// bound, as the prune reads them; ARGV[3] the client's member, ARGV[4] the entry's expiry
// in unix ms, ARGV[5] the network's limit, -1 for none, and ARGV[6] the key's ttl in
// seconds. A live entry of the client is a connection's already: it is kept at the later
// of its expiry and ARGV[4], and the client passes. Otherwise the client's entry is added
// while the live entries are fewer than the limit, and a refusal changes nothing. Returns
// {admitted, live entries after, added, the entry's expiry}. A live entry holding exactly
// ARGV[4] is this admission's own write, run again after a lost reply, and reads as added.
var sidecarClientAdmitScript = redis.NewScript(sidecarClientPruneLua + `
local key = KEYS[1]
local now_ms = tonumber(ARGV[1])
local member = ARGV[3]
local expiry_ms = tonumber(ARGV[4])
local limit = tonumber(ARGV[5])
local count = redis.call('ZCOUNT', key, '(' .. ARGV[1], '+inf')
local score = redis.call('ZSCORE', key, member)
if score and now_ms < tonumber(score) then
	local held_ms = tonumber(score)
	local added = 0
	if held_ms == expiry_ms then
		added = 1
	elseif held_ms < expiry_ms then
		redis.call('ZADD', key, ARGV[4], member)
		held_ms = expiry_ms
	end
	redis.call('EXPIRE', key, ARGV[6])
	return {1, count, added, held_ms}
end
if 0 <= limit and limit <= count then
	return {0, count, 0, 0}
end
redis.call('ZADD', key, ARGV[4], member)
redis.call('EXPIRE', key, ARGV[6])
return {1, count + 1, 1, expiry_ms}
`)

// A resident's poll. KEYS[1] is the network's set; ARGV[1] now and ARGV[2] the prune bound;
// ARGV[3] the client's member, ARGV[4] the entry's new expiry, ARGV[5] the expiry the
// resident's own last write left, 0 for none, and ARGV[6] the key's ttl in seconds. A live
// entry is kept at the later of its expiry and ARGV[4]. An entry gone or expired while the
// resident's last write is still live was lost, not lapsed, and is put back. Otherwise the
// entry lapsed, and nothing is written. Returns {state, the entry's expiry}, state 1 for
// kept, 2 for put back and 0 for lapsed.
var sidecarClientRefreshScript = redis.NewScript(sidecarClientPruneLua + `
local key = KEYS[1]
local now_ms = tonumber(ARGV[1])
local member = ARGV[3]
local expiry_ms = tonumber(ARGV[4])
local score = redis.call('ZSCORE', key, member)
if score and now_ms < tonumber(score) then
	local held_ms = tonumber(score)
	if held_ms < expiry_ms then
		redis.call('ZADD', key, ARGV[4], member)
		held_ms = expiry_ms
	end
	redis.call('EXPIRE', key, ARGV[6])
	return {1, held_ms}
end
if now_ms < tonumber(ARGV[5]) then
	redis.call('ZADD', key, ARGV[4], member)
	redis.call('EXPIRE', key, ARGV[6])
	return {2, expiry_ms}
end
return {0, 0}
`)

// KEYS[1] is the network's set. Removes the member ARGV[1] while its expiry is still
// ARGV[2], the one its connection last wrote, so that what a newer connection of the client
// wrote stays. Returns 1 when it removed the entry.
var sidecarClientRemoveScript = redis.NewScript(`
local score = redis.call('ZSCORE', KEYS[1], ARGV[1])
if score and tonumber(score) == tonumber(ARGV[2]) then
	redis.call('ZREM', KEYS[1], ARGV[1])
	return 1
end
return 0
`)

// One connection's admission against its network's sidecar_clients, at its nomination.
type SidecarClientAdmission struct {
	ClientId server.Id
	// Whether the connection may proceed.
	Admitted bool
	// The network whose set holds the client's entry; nil for a client the gate did not
	// count, since sidecar clients are not counted now or it is no sidecar's own client.
	NetworkId *server.Id
	// The entry's expiry after the admission, in unix ms; 0 when there is none.
	ExpiryMs int64
	// Whether this admission added the entry, rather than finding it held by a connection
	// of the client already: a nomination failing after it releases what it added.
	Added bool
	// The network's limit, -1 for none, and the live entries the gate found: a refusal's
	// numbers.
	Limit     int64
	UsedCount int64
}

// The gate at a resident's nomination, beside CanConnectNetworkPeer: whether the client may
// connect, its entry in its network's connected sidecar set counted and added in one step
// when it is a sidecar's own client. A refusal closes the connection with 4001 as the
// concurrent limit's does. A client with a live entry, a re-nomination, passes and keeps
// it, so connected clients stay. Admitted, with no I/O, while the limit is dark; admitted,
// with no entry, for a client with no sidecar mark, a sidecar's window and forwarding
// clients among them. ttl is the resident's, ExchangeResidentTtl, which its poll writes.
func AdmitSidecarClient(ctx context.Context, clientId server.Id, ttl time.Duration) *SidecarClientAdmission {
	if !SidecarClientCountingOn() {
		return &SidecarClientAdmission{ClientId: clientId, Admitted: true}
	}
	networkId, marked := getSidecarClientMark(ctx, clientId)
	if !marked {
		return &SidecarClientAdmission{ClientId: clientId, Admitted: true}
	}
	return admitNetworkSidecarClient(ctx, networkId, clientId, ttl)
}

// The gate's script for a marked client of networkId, against the network's limit while
// sidecar clients are counted, and against none while they are not, so a resident built
// while they were keeps its entry as before.
func admitNetworkSidecarClient(ctx context.Context, networkId server.Id, clientId server.Id, ttl time.Duration) *SidecarClientAdmission {
	limit := proSidecarLimitNone
	if SidecarClientCountingOn() {
		_, limits := networkSidecarLimitsWithTier(ctx, nil, networkId)
		limit = limits.Limit("sidecar_clients")
	}
	now := server.NowUtc()
	expiryMs := now.Add(ttl).UnixMilli()
	settings := sidecarClientCountSettings
	var values []int64
	server.Redis(ctx, func(r server.RedisClient) {
		var err error
		values, err = sidecarClientAdmitScript.Run(
			ctx,
			r,
			[]string{networkSidecarConnectedKey(networkId)},
			now.UnixMilli(),
			settings.PruneLimit,
			string(clientId.Bytes()),
			expiryMs,
			limit,
			int64(settings.KeyTtl/time.Second),
		).Int64Slice()
		if err != nil {
			panic(err)
		}
	})
	if len(values) != 4 {
		panic(fmt.Errorf("the sidecar client admission answered %v", values))
	}
	admission := &SidecarClientAdmission{
		ClientId:  clientId,
		Admitted:  values[0] == 1,
		Limit:     limit,
		UsedCount: values[1],
	}
	if admission.Admitted {
		admission.NetworkId = &networkId
		admission.Added = values[2] == 1
		admission.ExpiryMs = values[3]
	}
	return admission
}

// Removes the entry an admission added, for a nomination that failed after its gate, while
// the entry still holds the expiry the admission wrote. Nothing for an admission that added
// nothing.
func ReleaseSidecarClientAdmission(ctx context.Context, admission *SidecarClientAdmission) {
	if admission == nil || !admission.Added || admission.NetworkId == nil {
		return
	}
	RemoveNetworkSidecarClient(ctx, *admission.NetworkId, admission.ClientId, admission.ExpiryMs)
}

// What a resident's poll left of its client's entry.
type SidecarClientRefresh struct {
	// Whether the resident still holds an entry. False only for one whose entry lapsed and
	// whose network's limit then refused it as a new connection: the resident closes.
	Held bool
	// The entry's expiry after the poll, in unix ms, while held.
	ExpiryMs int64
	// The entry had been lost while the resident's last write was live, and was put back.
	Restored bool
	// The entry had lapsed, and the client was admitted again, or refused.
	Lapsed bool
}

// The resident's poll of a marked client's entry, as it heartbeats the peer registry: a
// live entry is kept until ttl from now; one lost while heldExpiryMs, the expiry the
// resident's last write left, is still live is put back, whatever the count; and one that
// lapsed is admitted again through the gate's script, which refuses it when the network is
// full.
func RefreshNetworkSidecarClient(ctx context.Context, networkId server.Id, clientId server.Id, ttl time.Duration, heldExpiryMs int64) *SidecarClientRefresh {
	now := server.NowUtc()
	expiryMs := now.Add(ttl).UnixMilli()
	settings := sidecarClientCountSettings
	var values []int64
	server.Redis(ctx, func(r server.RedisClient) {
		var err error
		values, err = sidecarClientRefreshScript.Run(
			ctx,
			r,
			[]string{networkSidecarConnectedKey(networkId)},
			now.UnixMilli(),
			settings.PruneLimit,
			string(clientId.Bytes()),
			expiryMs,
			heldExpiryMs,
			int64(settings.KeyTtl/time.Second),
		).Int64Slice()
		if err != nil {
			panic(err)
		}
	})
	if len(values) != 2 {
		panic(fmt.Errorf("the sidecar client refresh answered %v", values))
	}
	switch values[0] {
	case 1:
		return &SidecarClientRefresh{Held: true, ExpiryMs: values[1]}
	case 2:
		return &SidecarClientRefresh{Held: true, ExpiryMs: values[1], Restored: true}
	}
	admission := admitNetworkSidecarClient(ctx, networkId, clientId, ttl)
	return &SidecarClientRefresh{
		Held:     admission.Admitted,
		ExpiryMs: admission.ExpiryMs,
		Lapsed:   true,
	}
}

// Removes a marked client's entry, as its resident closes, while the entry still holds
// heldExpiryMs, the expiry that resident's last write left, so an entry a newer connection
// of the client wrote stays. Nothing for heldExpiryMs 0, a resident that held no entry.
func RemoveNetworkSidecarClient(ctx context.Context, networkId server.Id, clientId server.Id, heldExpiryMs int64) {
	if heldExpiryMs == 0 {
		return
	}
	server.Redis(ctx, func(r server.RedisClient) {
		err := sidecarClientRemoveScript.Run(
			ctx,
			r,
			[]string{networkSidecarConnectedKey(networkId)},
			string(clientId.Bytes()),
			heldExpiryMs,
		).Err()
		if err != nil && err != server.RedisNil {
			panic(err)
		}
	})
}

// Whether the client counts toward its network's sidecar_clients, a sidecar's own client:
// what a resident asks as it is built, of a client its profile calls strict_exp, so that
// its poll never keeps a sidecar's window or forwarding client in the zset. The gate asked
// just before (AdmitSidecarClient) and the answer is kept, so this costs no query there.
// False, with no I/O, while sidecar clients are not counted.
func CountsAsSidecarClient(ctx context.Context, clientId server.Id) bool {
	if !SidecarClientCountingOn() {
		return false
	}
	_, marked := getSidecarClientMark(ctx, clientId)
	return marked
}

// The network's connected sidecar clients: the zset's entries whose expiry has not
// passed. One ZCOUNT.
func GetNetworkSidecarConnectedCount(ctx context.Context, networkId server.Id) (count int64) {
	liveMin := fmt.Sprintf("(%d", server.NowUtc().UnixMilli())
	server.Redis(ctx, func(r server.RedisClient) {
		var err error
		count, err = r.ZCount(ctx, networkSidecarConnectedKey(networkId), liveMin, "+inf").Result()
		if err != nil && err != server.RedisNil {
			panic(err)
		}
	})
	return
}

// The refusal for one more connected sidecar client of the network, or nil when it has
// room: what TEAM-M1-04 calls at enrollment, an early refusal before a client is minted,
// which reserves nothing (the header). Nil, with no I/O, while the limit is dark. A limit of
// -1 passes, and the refusal's Upgrade says whether a higher tier lifts it.
func SidecarClientsExceeded(ctx context.Context, networkId server.Id) *sidecar.QuotaRefusal {
	return sidecarClientsExceeded(ctx, nil, networkId)
}

// SidecarClientsExceeded for a caller holding a transaction, such as an enrollment that
// mints the client in it: the limits read through tx where their caches miss.
func SidecarClientsExceededInTx(ctx context.Context, tx server.PgTx, networkId server.Id) *sidecar.QuotaRefusal {
	return sidecarClientsExceeded(ctx, tx, networkId)
}

func sidecarClientsExceeded(ctx context.Context, tx server.PgTx, networkId server.Id) *sidecar.QuotaRefusal {
	if !SidecarClientCountingOn() {
		return nil
	}
	tier, limits := networkSidecarLimitsWithTier(ctx, tx, networkId)
	limit := limits.Limit("sidecar_clients")
	if limit == proSidecarLimitNone {
		return nil
	}
	usedCount := GetNetworkSidecarConnectedCount(ctx, networkId)
	if usedCount < limit {
		return nil
	}
	return &sidecar.QuotaRefusal{
		LimitKey: "sidecar_clients",
		Limit:    limit,
		Used:     usedCount,
		Upgrade:  quotaRefusalUpgrade(tier, "sidecar_clients", usedCount, networkSidecarLimitHasRow(ctx, tx, networkId, "sidecar_clients")),
	}
}

// A client's network and sidecar mark. Each fact the mark reads, the client's network,
// its marks and its source's strict_exp, is written at a mint and never changes, so a
// process keeps each answer it read, bounded as ClientRefusesAdmin's are, and a
// re-nomination asks no query. A source row the child reap removes later leaves a kept
// answer as it was read.
type sidecarClientMark struct {
	networkId server.Id
	marked    bool
}

var sidecarClientMarkAnswers = newBoundedClientMap[sidecarClientMark](DefaultClientAdminSettings().CachedClientMaxCount)

// The client's network and whether it is a sidecar's own client (the header): its owner
// row marks it strict_exp, its source client's row, if it has a source, does not, and it is
// not forward_only, MG8's column, which a binary without MG8 reads as false since it mints
// no forwarding client. An inactive or unknown client is unmarked, and that answer is not
// kept. Only while MG0 is linked.
//
// A child whose source row is gone, which only the child reap leaves (it removes a stale
// child with no connection, such as an app's sidecar client while the app is closed), reads
// as an unmarked client's child: F, which its VPN component keeps connected, stays exempt
// by forward_only, and any other strict_exp child counts.
func getSidecarClientMark(ctx context.Context, clientId server.Id) (networkId server.Id, marked bool) {
	if answer, ok := sidecarClientMarkAnswers.Load(clientId); ok {
		return answer.networkId, answer.marked
	}
	found := false
	defer func() {
		if found {
			sidecarClientMarkAnswers.Store(clientId, sidecarClientMark{networkId: networkId, marked: marked})
		}
	}()
	forwardOnlyColumn := "false"
	if server.SidecarMg8Linked() {
		forwardOnlyColumn = "COALESCE(network_client_owner.forward_only, false)"
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
					network_client.network_id,
					COALESCE(network_client_owner.strict_exp, false) AND
						NOT COALESCE(source_owner.strict_exp, false) AND
						NOT `+forwardOnlyColumn+`
				FROM network_client
				LEFT JOIN network_client_owner ON
					network_client_owner.client_id = network_client.client_id
				LEFT JOIN network_client_owner source_owner ON
					source_owner.client_id = network_client.source_client_id
				WHERE
					network_client.client_id = $1 AND
					network_client.active = true
			`,
			clientId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&networkId, &marked))
				found = true
			}
		})
	})
	return
}

// Drops this process's kept marks, for a test that writes a client's owner row after the
// client was asked about.
func Testing_ForgetSidecarClientMarks() {
	sidecarClientMarkAnswers.Clear()
}

// Replaces the zset's settings, returning a restore function, for tests.
func Testing_SetSidecarClientCountSettings(settings *SidecarClientCountSettings) func() {
	prev := sidecarClientCountSettings
	sidecarClientCountSettings = settings
	return func() { sidecarClientCountSettings = prev }
}

// Adds or refreshes one client's entry until ttl from now, unconditionally, as no
// production path does: for a test that seeds a network's connected sidecars.
func Testing_AddNetworkSidecarClient(ctx context.Context, networkId server.Id, clientId server.Id, ttl time.Duration) {
	key := networkSidecarConnectedKey(networkId)
	server.Redis(ctx, func(r server.RedisClient) {
		pipe := r.TxPipeline()
		pipe.ZAdd(ctx, key, redis.Z{
			Score:  float64(server.NowUtc().Add(ttl).UnixMilli()),
			Member: string(clientId.Bytes()),
		})
		pipe.Expire(ctx, key, sidecarClientCountSettings.KeyTtl)
		if _, err := pipe.Exec(ctx); err != nil {
			panic(err)
		}
	})
}
