package model

// The sockets engine's store on Redis Cluster (§9.8, D-SOCK-7; SOCK-M5-04): sidecar's
// socketsservice.SocketStore on §2.8's sock-1, sock-3, sock-4 and sock-5, and nothing in Postgres
// (I-12). Keys:
//
//	{sg_<network_id>_<h16>}      a group's hash: per listener m, r, i and e, and v (socket_store_scripts.go)
//	{sg_<network_id>_<h16>}ix    the group's leases, a ZSET of listener ids scored by lease end in ms
//	{svc_<listener_client_id>}   §6.10.1's pairs: field the dialer, value the grant's expiry (sock-3)
//	{sn_<network_id>}names       the listing: field the name key, value kind;lease_ms;sealed (sock-4)
//	{sc_<client_id>}set          the client's set, a ZSET of 0x01 || name key and one meta member (sock-5)
//
// h16 is the first 16 hex digits of SHA-256 of the name key, an edge name's bytes or a token path. Hash
// tags are per group, never per network, so a network's groups spread over the cluster's slots. A Find is
// one script on one group (§22.5). A page crosses slots, so it runs a script per group, pipelined, then
// commits the client's set and the listing, in the order that leaves a lease to expire rather than a set
// entry naming a lease never written. Hash-field ttls are cleanup only: what a lease, share or report
// means comes from the clock this store passes its scripts, which a test injects, and no keyspace event
// of a field's expiry is relied on.
//
// These families stay out of the rdb bundles (D-OPS-16): they are named to the OPS track for its filter.
//
// Every call takes the deadline pool with no retry and no preflight, so a Find costs its one script and
// a call that fails is answered unavailable and healed by the next. Safe for concurrent use.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/socketsservice"

	"github.com/urnetwork/server"
)

var _ socketsservice.SocketStore = (*SocketStore)(nil)

// The Redis store's own tunables, beside the engine's store settings it shares with the fake.
type SocketStoreSettings struct {
	// What every store keeps alike: the lease, the set's ttl, the sample and the scheduler's values.
	Store *socketsservice.SocketStoreSettings
	// The longest one call may take when its caller gave no deadline: 2 s, the engine's StoreTimeout.
	CallTimeout time.Duration
}

// The defaults.
func DefaultSocketStoreSettings() *SocketStoreSettings {
	return &SocketStoreSettings{
		Store:       socketsservice.DefaultSocketStoreSettings(),
		CallTimeout: 2 * time.Second,
	}
}

// The store. NewSocketStore makes one.
type SocketStore struct {
	clock    sidecar.Clock
	settings *SocketStoreSettings
	// Sees each script's name and keys as it runs, for the slot check and op counts. Nil in production.
	scriptCallback func(name string, keys []string)
}

// A store on clock, nil being real time, with settings, nil being the defaults.
func NewSocketStore(clock sidecar.Clock, settings *SocketStoreSettings) *SocketStore {
	if clock == nil {
		clock = sidecar.RealClock{}
	}
	if settings == nil {
		settings = DefaultSocketStoreSettings()
	}
	return &SocketStore{
		clock:    clock,
		settings: settings,
	}
}

// An id as the store's fields and scores name it.
func socketHexId(id connect.Id) string {
	return hex.EncodeToString(id[:])
}

// The id a field names, or an error for a field this store did not write.
func socketParseHexId(text string) (connect.Id, error) {
	var id connect.Id
	value, err := hex.DecodeString(text)
	if err != nil || len(value) != len(id) {
		return id, fmt.Errorf("socket store: a malformed id")
	}
	copy(id[:], value)
	return id, nil
}

// {sg_<network_id>_<h16>} and its ix.
func socketGroupKeys(networkId connect.Id, nameKey []byte) []string {
	digest := sha256.Sum256(nameKey)
	tag := fmt.Sprintf("{sg_%s_%s}", server.Id(networkId), hex.EncodeToString(digest[:8]))
	return []string{tag, tag + "ix"}
}

// {svc_<listener_client_id>}.
func socketPairKey(listenerClientId connect.Id) string {
	return fmt.Sprintf("{svc_%s}", server.Id(listenerClientId))
}

// {sn_<network_id>}names.
func socketNamesKey(networkId connect.Id) string {
	return fmt.Sprintf("{sn_%s}names", server.Id(networkId))
}

// {sc_<client_id>}set.
func socketClientSetKey(clientId connect.Id) string {
	return fmt.Sprintf("{sc_%s}set", server.Id(clientId))
}

// Runs callback on the deadline pool with no retry and no preflight, bounded by ctx's deadline or
// CallTimeout, and turns the wrapper's panics into errors.
func (self *SocketStore) redis(ctx context.Context, callback func(client server.RedisClient) error) (returnErr error) {
	if _, bounded := ctx.Deadline(); !bounded {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, self.settings.CallTimeout)
		defer cancel()
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			if err, ok := recovered.(error); ok {
				returnErr = err
			} else {
				returnErr = fmt.Errorf("socket store: %v", recovered)
			}
		}
	}()
	return server.RedisWithDeadline(ctx, func(client server.RedisClient) error {
		return callback(client)
	}, server.OptRedisNoPreflight())
}

// Notes one script for the callback.
func (self *SocketStore) noteScript(name string, keys []string) {
	if self.scriptCallback != nil {
		self.scriptCallback(name, slices.Clone(keys))
	}
}

// One script, by SHA-1 with a fallback to its source.
func (self *SocketStore) runScript(ctx context.Context, client server.RedisClient, name string, keys []string, args ...any) (any, error) {
	self.noteScript(name, keys)
	return socketStoreScripts[name].Run(ctx, client, keys, args...).Result()
}

// One script call of a pipelined batch.
type socketScriptCall struct {
	name string
	keys []string
	args []any
}

// Runs calls pipelined, each by SHA-1, then reruns by source any a node had not loaded, and returns
// each reply in order.
func (self *SocketStore) runScripts(ctx context.Context, client server.RedisClient, calls []*socketScriptCall) ([]any, error) {
	if len(calls) == 0 {
		return nil, nil
	}
	pipe := client.Pipeline()
	cmds := make([]*redis.Cmd, len(calls))
	for index, call := range calls {
		self.noteScript(call.name, call.keys)
		cmds[index] = socketStoreScripts[call.name].EvalSha(ctx, pipe, call.keys, call.args...)
	}
	// errors are per command, read below
	pipe.Exec(ctx)
	replies := make([]any, len(calls))
	for index, cmd := range cmds {
		reply, err := cmd.Result()
		if err != nil && strings.HasPrefix(err.Error(), "NOSCRIPT") {
			call := calls[index]
			reply, err = socketStoreScripts[call.name].Run(ctx, client, call.keys, call.args...).Result()
		}
		if err != nil {
			return nil, err
		}
		replies[index] = reply
	}
	return replies, nil
}

// See socketsservice.SocketStore.
func (self *SocketStore) Ping(ctx context.Context) error {
	return self.redis(ctx, func(client server.RedisClient) error {
		return client.Ping(ctx).Err()
	})
}

// m:<L>'s record for registration.
func socketRegistrationRecord(registration *sidecar.SocketRegistration) string {
	flags := 0
	if registration.Exclusive {
		flags |= 1
	}
	if registration.Affinity {
		flags |= 2
	}
	if registration.Serving {
		flags |= 4
	}
	if registration.Draining {
		flags |= 8
	}
	alpns := make([]string, 0, len(registration.Alpn))
	for _, alpn := range registration.Alpn {
		alpns = append(alpns, hex.EncodeToString([]byte(alpn)))
	}
	listenId := ""
	if registration.ListenId != nil {
		listenId = socketHexId(*registration.ListenId)
	}
	return strings.Join([]string{
		listenId,
		hex.EncodeToString([]byte(registration.Protocol)),
		strconv.Itoa(registration.Version),
		strconv.Itoa(registration.Weight),
		strconv.Itoa(registration.MaxStreams),
		strconv.Itoa(flags),
		strings.Join(alpns, ","),
		hex.EncodeToString([]byte(registration.AppMode)),
		hex.EncodeToString([]byte(registration.Name)),
		hex.EncodeToString(registration.NameToken),
		base64.StdEncoding.EncodeToString(registration.SealedName),
		base64.StdEncoding.EncodeToString(registration.Claim),
	}, ";")
}

// What m:<L> and r:<L> hold, back as a registration.
func socketParseRegistrationRecord(record string, inflightRecord string) (*sidecar.SocketRegistration, int64, error) {
	malformed := errors.New("socket store: a malformed registration record")
	fields := strings.Split(record, ";")
	if len(fields) != 12 {
		return nil, 0, malformed
	}
	hexText := func(text string) (string, error) {
		value, err := hex.DecodeString(text)
		return string(value), err
	}
	registration := &sidecar.SocketRegistration{}
	listenId, err := socketParseHexId(fields[0])
	if err != nil {
		return nil, 0, malformed
	}
	registration.ListenId = &listenId
	numbers := make([]int, 4)
	for index := range numbers {
		if numbers[index], err = strconv.Atoi(fields[2+index]); err != nil {
			return nil, 0, malformed
		}
	}
	registration.Version, registration.Weight, registration.MaxStreams = numbers[0], numbers[1], numbers[2]
	flags := numbers[3]
	registration.Exclusive = flags&1 != 0
	registration.Affinity = flags&2 != 0
	registration.Serving = flags&4 != 0
	registration.Draining = flags&8 != 0
	if registration.Protocol, err = hexText(fields[1]); err != nil {
		return nil, 0, malformed
	}
	if fields[6] != "" {
		for _, alpnHex := range strings.Split(fields[6], ",") {
			alpn, err := hexText(alpnHex)
			if err != nil {
				return nil, 0, malformed
			}
			registration.Alpn = append(registration.Alpn, alpn)
		}
	}
	if registration.AppMode, err = hexText(fields[7]); err != nil {
		return nil, 0, malformed
	}
	if registration.Name, err = hexText(fields[8]); err != nil {
		return nil, 0, malformed
	}
	if registration.NameToken, err = hex.DecodeString(fields[9]); err != nil {
		return nil, 0, malformed
	}
	if len(registration.NameToken) == 0 {
		registration.NameToken = nil
	}
	if registration.SealedName, err = base64.StdEncoding.DecodeString(fields[10]); err != nil {
		return nil, 0, malformed
	}
	if len(registration.SealedName) == 0 {
		registration.SealedName = nil
	}
	if registration.Claim, err = base64.StdEncoding.DecodeString(fields[11]); err != nil {
		return nil, 0, malformed
	}
	reportMillis := int64(0)
	if inflightRecord != "" {
		inflightText, reportText, found := strings.Cut(inflightRecord, ";")
		if !found {
			return nil, 0, malformed
		}
		if registration.Inflight, err = strconv.Atoi(inflightText); err != nil {
			return nil, 0, malformed
		}
		if reportMillis, err = strconv.ParseInt(reportText, 10, 64); err != nil {
			return nil, 0, malformed
		}
	}
	return registration, reportMillis, nil
}

// A listener from a script's six values: listener, lease_ms, m, r, i and e.
func socketParseListener(values []string) (*socketsservice.SocketStoreListener, error) {
	malformed := errors.New("socket store: a malformed listener")
	if len(values) != 6 {
		return nil, malformed
	}
	listenerClientId, err := socketParseHexId(values[0])
	if err != nil {
		return nil, err
	}
	leaseExpireMillis, err := strconv.ParseInt(values[1], 10, 64)
	if err != nil {
		return nil, malformed
	}
	registration, reportMillis, err := socketParseRegistrationRecord(values[2], values[3])
	if err != nil {
		return nil, err
	}
	candidate := &socketsservice.SocketCandidate{
		ListenerClientId:     listenerClientId,
		ListenId:             *registration.ListenId,
		Protocol:             registration.Protocol,
		Version:              registration.Version,
		Alpns:                slices.Clone(registration.Alpn),
		Weight:               registration.Weight,
		MaxStreams:           registration.MaxStreams,
		Serving:              registration.Serving,
		Draining:             registration.Draining,
		LeaseExpireMillis:    leaseExpireMillis,
		ReportedInflight:     registration.Inflight,
		ReportMillis:         reportMillis,
		DialerClientIdShares: map[connect.Id]*socketsservice.SocketDialerShare{},
	}
	if values[4] != "" {
		for _, entry := range strings.Split(values[4], "|") {
			fields := strings.Split(entry, ";")
			if len(fields) != 4 {
				return nil, malformed
			}
			dialerClientId, err := socketParseHexId(fields[0])
			if err != nil {
				return nil, err
			}
			share := &socketsservice.SocketDialerShare{}
			if share.Exclusion.ConsecutiveCount, err = strconv.Atoi(fields[1]); err != nil {
				return nil, malformed
			}
			if share.Exclusion.StartMillis, err = strconv.ParseInt(fields[2], 10, 64); err != nil {
				return nil, malformed
			}
			if fields[3] != "" {
				for _, issueText := range strings.Split(fields[3], ",") {
					issueMillis, err := strconv.ParseInt(issueText, 10, 64)
					if err != nil {
						return nil, malformed
					}
					share.PrimaryIssueMillis = append(share.PrimaryIssueMillis, issueMillis)
				}
			}
			candidate.DialerClientIdShares[dialerClientId] = share
		}
	}
	if values[5] != "" {
		fields := strings.SplitN(values[5], ";", 3)
		if len(fields) != 3 {
			return nil, malformed
		}
		if candidate.Ejection.ConsecutiveCount, err = strconv.Atoi(fields[0]); err != nil {
			return nil, malformed
		}
		if candidate.Ejection.StartMillis, err = strconv.ParseInt(fields[1], 10, 64); err != nil {
			return nil, malformed
		}
	}
	return &socketsservice.SocketStoreListener{Registration: registration, Candidate: candidate}, nil
}

// A script's array reply as strings, integers written in decimal.
func socketReplyStrings(reply any) ([]string, error) {
	values, ok := reply.([]any)
	if !ok {
		return nil, errors.New("socket store: a reply that is no array")
	}
	strings := make([]string, len(values))
	for index, value := range values {
		switch value := value.(type) {
		case string:
			strings[index] = value
		case int64:
			strings[index] = strconv.FormatInt(value, 10)
		case nil:
			strings[index] = ""
		default:
			return nil, fmt.Errorf("socket store: a reply value of type %T", value)
		}
	}
	return strings, nil
}

// Whether nameKey lies in [keyStart, keyEnd), an empty keyEnd being unbounded.
func socketInRange(nameKey []byte, keyStart []byte, keyEnd []byte) bool {
	return 0 <= bytes.Compare(nameKey, keyStart) && (len(keyEnd) == 0 || bytes.Compare(nameKey, keyEnd) < 0)
}

// One group's del, pipelined with the others.
func socketDelCall(networkId connect.Id, nameKey []byte, listenerClientId connect.Id, nowMillis int64) *socketScriptCall {
	return &socketScriptCall{
		name: "group",
		keys: socketGroupKeys(networkId, nameKey),
		args: []any{"del", nowMillis, socketHexId(listenerClientId)},
	}
}

// What a group's put or del answered: its two flags, the listeners left and the newest lease.
func socketGroupReply(reply any) (bool, bool, int64, int64, error) {
	values, err := socketReplyStrings(reply)
	if err != nil {
		return false, false, 0, 0, err
	}
	if len(values) != 4 {
		return false, false, 0, 0, errors.New("socket store: a malformed group reply")
	}
	remaining, err := strconv.ParseInt(values[2], 10, 64)
	if err != nil {
		return false, false, 0, 0, err
	}
	maxLeaseMillis, err := strconv.ParseInt(values[3], 10, 64)
	if err != nil {
		return false, false, 0, 0, err
	}
	return values[0] == "1", values[1] == "1", remaining, maxLeaseMillis, nil
}

// One name's update to the listing.
type socketNameUpdate struct {
	nameKey        []byte
	maxLeaseMillis int64
	// "e", "b", or "" to keep the field's own.
	kind   string
	sealed []byte
}

// Writes updates to the network's listing in one script.
func (self *SocketStore) updateNames(ctx context.Context, client server.RedisClient, networkId connect.Id, nowMillis int64, updates []*socketNameUpdate) error {
	if len(updates) == 0 {
		return nil
	}
	args := []any{nowMillis}
	for _, update := range updates {
		args = append(args, update.nameKey, update.maxLeaseMillis, update.kind, update.sealed)
	}
	_, err := self.runScript(ctx, client, "names", []string{socketNamesKey(networkId)}, args...)
	return err
}

// See socketsservice.SocketStore.
func (self *SocketStore) ApplyPage(ctx context.Context, page *socketsservice.SocketStorePage) (*socketsservice.SocketStorePageResult, error) {
	if page == nil {
		return nil, fmt.Errorf("%w: no page", socketsservice.ErrSocketStoreRequest)
	}
	var previousNameKey []byte
	for index, registration := range page.Registrations {
		nameKey := registration.NameKey()
		if len(nameKey) == 0 || !socketInRange(nameKey, page.KeyStart, page.KeyEnd) ||
			(0 < index && bytes.Compare(nameKey, previousNameKey) <= 0) {
			return nil, fmt.Errorf("%w: entry %d", socketsservice.ErrSocketStoreRequest, index)
		}
		previousNameKey = nameKey
	}
	nowMillis := self.clock.Now().UnixMilli()
	leaseMillis := self.settings.Store.Lease.Milliseconds()
	setKey := socketClientSetKey(page.ListenerClientId)
	setVersion := strconv.FormatUint(page.SetVersion, 10)

	var result *socketsservice.SocketStorePageResult
	err := self.redis(ctx, func(client server.RedisClient) error {
		reply, err := self.runScript(ctx, client, "set_begin", []string{setKey}, setVersion, page.KeyStart, page.KeyEnd)
		if err != nil {
			return err
		}
		begin, err := socketReplyStrings(reply)
		if err != nil || len(begin) < 2 {
			return fmt.Errorf("socket store: a malformed set reply: %v", err)
		}
		if begin[0] == "stale" {
			stored, err := strconv.ParseUint(begin[1], 10, 64)
			if err != nil {
				return err
			}
			result = &socketsservice.SocketStorePageResult{Stale: true, SetVersion: stored}
			return nil
		}
		previousNameKeys := begin[2:]

		putCalls := make([]*socketScriptCall, 0, len(page.Registrations))
		for _, registration := range page.Registrations {
			exclusive := "0"
			if registration.Exclusive {
				exclusive = "1"
			}
			putCalls = append(putCalls, &socketScriptCall{
				name: "group",
				keys: socketGroupKeys(page.NetworkId, registration.NameKey()),
				args: []any{
					"put", nowMillis, socketHexId(page.ListenerClientId), leaseMillis,
					socketRegistrationRecord(registration), registration.Inflight,
					hex.EncodeToString([]byte(registration.Protocol)), exclusive,
				},
			})
		}
		putReplies, err := self.runScripts(ctx, client, putCalls)
		if err != nil {
			return err
		}
		result = &socketsservice.SocketStorePageResult{SetVersion: page.SetVersion}
		heldNameKeys := map[string]bool{}
		heldList := []any{}
		nameUpdates := []*socketNameUpdate{}
		for index, reply := range putReplies {
			taken, first, _, maxLeaseMillis, err := socketGroupReply(reply)
			if err != nil {
				return err
			}
			result.Outcomes = append(result.Outcomes, &socketsservice.SocketStoreEntryOutcome{Taken: taken, FirstListener: first && !taken})
			if taken {
				continue
			}
			registration := page.Registrations[index]
			heldNameKeys[string(registration.NameKey())] = true
			heldList = append(heldList, registration.NameKey())
			update := &socketNameUpdate{nameKey: registration.NameKey(), maxLeaseMillis: maxLeaseMillis, kind: "e"}
			if len(registration.NameToken) != 0 {
				update.kind = "b"
				update.sealed = registration.SealedName
			}
			nameUpdates = append(nameUpdates, update)
		}

		removedNameKeys := []string{}
		delCalls := []*socketScriptCall{}
		for _, nameKey := range previousNameKeys {
			if !heldNameKeys[nameKey] && socketInRange([]byte(nameKey), page.KeyStart, page.KeyEnd) {
				removedNameKeys = append(removedNameKeys, nameKey)
				delCalls = append(delCalls, socketDelCall(page.NetworkId, []byte(nameKey), page.ListenerClientId, nowMillis))
			}
		}
		delReplies, err := self.runScripts(ctx, client, delCalls)
		if err != nil {
			return err
		}
		for index, reply := range delReplies {
			found, last, _, maxLeaseMillis, err := socketGroupReply(reply)
			if err != nil {
				return err
			}
			result.Removed = append(result.Removed, &socketsservice.SocketStoreRemoved{
				NetworkId:        page.NetworkId,
				ListenerClientId: page.ListenerClientId,
				NameKey:          []byte(removedNameKeys[index]),
				LastListener:     found && last,
			})
			if found {
				nameUpdates = append(nameUpdates, &socketNameUpdate{nameKey: []byte(removedNameKeys[index]), maxLeaseMillis: maxLeaseMillis})
			}
		}

		commitArgs := []any{page.NetworkId[:], setVersion, page.KeyStart, page.KeyEnd, self.settings.Store.ClientSetTtl.Milliseconds()}
		commitArgs = append(commitArgs, heldList...)
		if _, err := self.runScript(ctx, client, "set_commit", []string{setKey}, commitArgs...); err != nil {
			return err
		}
		return self.updateNames(ctx, client, page.NetworkId, nowMillis, nameUpdates)
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// The arguments a pick passes its script, after its two keys.
func (self *SocketStore) pickArgs(request *socketsservice.SocketStorePickRequest, nowMillis int64) []any {
	scheduler := self.settings.Store.Scheduler
	alpns := make([]string, 0, len(request.Alpns))
	for _, alpn := range request.Alpns {
		alpns = append(alpns, hex.EncodeToString([]byte(alpn)))
	}
	reports := make([]string, 0, len(request.Reports))
	for _, report := range request.Reports {
		reports = append(reports, socketHexId(report.ListenerClientId)+":"+socketHexId(report.ListenId))
	}
	return []any{
		nowMillis,
		socketHexId(request.DialerClientId),
		socketHexId(request.DialerNetworkId),
		hex.EncodeToString([]byte(request.Protocol)),
		request.MinVersion,
		strings.Join(alpns, ","),
		request.FirstDraw,
		request.SecondDraw,
		self.settings.Store.SampleCount,
		scheduler.TicketLifetime.Milliseconds(),
		scheduler.ClampFloorCount,
		scheduler.ClampPercent,
		scheduler.EjectionStep.Milliseconds(),
		scheduler.EjectionMax.Milliseconds(),
		scheduler.EjectionGroupPercent,
		scheduler.ReporterWindow.Milliseconds(),
		scheduler.ReporterNetworkCount,
		max(scheduler.EjectionMax, scheduler.TicketLifetime).Milliseconds(),
		strings.Join(reports, ","),
	}
}

// See socketsservice.SocketStore.
func (self *SocketStore) Pick(ctx context.Context, request *socketsservice.SocketStorePickRequest) (*socketsservice.SocketStorePickResult, error) {
	if request == nil || len(request.NameKey) == 0 {
		return nil, fmt.Errorf("%w: no group", socketsservice.ErrSocketStoreRequest)
	}
	nowMillis := self.clock.Now().UnixMilli()
	var result *socketsservice.SocketStorePickResult
	err := self.redis(ctx, func(client server.RedisClient) error {
		reply, err := self.runScript(ctx, client, "pick", socketGroupKeys(request.NetworkId, request.NameKey), self.pickArgs(request, nowMillis)...)
		if err != nil {
			return err
		}
		values, err := socketReplyStrings(reply)
		if err != nil {
			return err
		}
		if len(values) < 3 {
			return errors.New("socket store: a malformed pick reply")
		}
		outcome, err := strconv.Atoi(values[0])
		if err != nil {
			return err
		}
		exclusionCount, err := strconv.Atoi(values[1])
		if err != nil {
			return err
		}
		ejectionCount, err := strconv.Atoi(values[2])
		if err != nil {
			return err
		}
		result = &socketsservice.SocketStorePickResult{
			Outcome:        socketsservice.SocketPickOutcome(outcome),
			ExclusionCount: exclusionCount,
			EjectionCount:  ejectionCount,
		}
		listenerValues := values[3:]
		if result.Outcome == socketsservice.SocketPickFound {
			if len(listenerValues) != 6 && len(listenerValues) != 12 {
				return errors.New("socket store: a pick named no listener")
			}
			if result.Primary, err = socketParseListener(listenerValues[:6]); err != nil {
				return err
			}
			if len(listenerValues) == 12 {
				if result.Backup, err = socketParseListener(listenerValues[6:]); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// See socketsservice.SocketStore.
func (self *SocketStore) ListListeners(ctx context.Context, networkId connect.Id, nameKey []byte) ([]*socketsservice.SocketStoreListener, error) {
	nowMillis := self.clock.Now().UnixMilli()
	listeners := []*socketsservice.SocketStoreListener{}
	err := self.redis(ctx, func(client server.RedisClient) error {
		reply, err := self.runScript(ctx, client, "read", socketGroupKeys(networkId, nameKey), nowMillis)
		if err != nil {
			return err
		}
		values, err := socketReplyStrings(reply)
		if err != nil {
			return err
		}
		if len(values)%6 != 0 {
			return errors.New("socket store: a malformed read reply")
		}
		for index := 0; index < len(values); index += 6 {
			listener, err := socketParseListener(values[index : index+6])
			if err != nil {
				return err
			}
			listeners = append(listeners, listener)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(listeners, func(a *socketsservice.SocketStoreListener, b *socketsservice.SocketStoreListener) int {
		return a.Candidate.ListenerClientId.Cmp(b.Candidate.ListenerClientId)
	})
	return listeners, nil
}

// See socketsservice.SocketStore. The cursor is HSCAN's with how many of its batch earlier pages took,
// so a batch larger than count, as a small hash's whole listpack is, still pages; a name changed between
// pages may show twice, as the interface allows.
func (self *SocketStore) ListNames(ctx context.Context, networkId connect.Id, cursor string, count int) ([]*sidecar.SocketListName, string, error) {
	if count <= 0 {
		return nil, "", fmt.Errorf("%w: a count of %d", socketsservice.ErrSocketStoreRequest, count)
	}
	scanCursor := uint64(0)
	skip := 0
	if cursor != "" {
		scanText, skipText, found := strings.Cut(cursor, ":")
		var err error
		if scanCursor, err = strconv.ParseUint(scanText, 10, 64); !found || err != nil {
			return nil, "", fmt.Errorf("%w: a cursor no page gave", socketsservice.ErrSocketStoreRequest)
		}
		if skip, err = strconv.Atoi(skipText); err != nil || skip < 0 {
			return nil, "", fmt.Errorf("%w: a cursor no page gave", socketsservice.ErrSocketStoreRequest)
		}
	}
	nowMillis := self.clock.Now().UnixMilli()
	names := []*sidecar.SocketListName{}
	nextCursor := ""
	err := self.redis(ctx, func(client server.RedisClient) error {
		fieldValues, scanNext, err := client.HScan(ctx, socketNamesKey(networkId), scanCursor, "", int64(count)).Result()
		if err != nil {
			return err
		}
		// pairs this page takes from the batch, and whether the batch has more
		pairCount := len(fieldValues) / 2
		end := min(pairCount, skip+count)
		for index := skip; index < end; index += 1 {
			nameKey := fieldValues[2*index]
			value := fieldValues[2*index+1]
			kind, rest, found := strings.Cut(value, ";")
			if !found {
				continue
			}
			leaseText, sealed, found := strings.Cut(rest, ";")
			if !found {
				continue
			}
			leaseMillis, err := strconv.ParseInt(leaseText, 10, 64)
			if err != nil || leaseMillis <= nowMillis {
				continue
			}
			if kind == "e" {
				names = append(names, &sidecar.SocketListName{Name: nameKey})
			} else {
				names = append(names, &sidecar.SocketListName{NameToken: []byte(nameKey), SealedName: []byte(sealed)})
			}
		}
		switch {
		case end < pairCount:
			nextCursor = fmt.Sprintf("%d:%d", scanCursor, end)
		case scanNext != 0:
			nextCursor = fmt.Sprintf("%d:0", scanNext)
		}
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	return names, nextCursor, nil
}

// See socketsservice.SocketStore.
func (self *SocketStore) SetServicePair(ctx context.Context, listenerClientId connect.Id, dialerClientId connect.Id, grantExpireMillis uint64, ttl time.Duration) error {
	if ttl <= 0 {
		return fmt.Errorf("%w: a ttl of %v", socketsservice.ErrSocketStoreRequest, ttl)
	}
	// rounded up, so a positive ttl never reaches Redis as zero (host_redis.go)
	ttlMillis := (ttl + time.Millisecond - 1).Milliseconds()
	return self.redis(ctx, func(client server.RedisClient) error {
		_, err := self.runScript(ctx, client, "pair", []string{socketPairKey(listenerClientId)},
			socketHexId(dialerClientId), strconv.FormatUint(grantExpireMillis, 10), ttlMillis)
		return err
	})
}

// See socketsservice.SocketStore.
func (self *SocketStore) GetServicePair(ctx context.Context, listenerClientId connect.Id, dialerClientId connect.Id) (uint64, bool, error) {
	grantExpireMillis := uint64(0)
	found := false
	err := self.redis(ctx, func(client server.RedisClient) error {
		var err error
		grantExpireMillis, found, err = getSocketServicePair(ctx, client, listenerClientId, dialerClientId)
		return err
	})
	if err != nil {
		return 0, false, err
	}
	return grantExpireMillis, found, nil
}

// See socketsservice.SocketStore.
func (self *SocketStore) RemoveServicePairs(ctx context.Context, listenerClientId connect.Id) error {
	return self.redis(ctx, func(client server.RedisClient) error {
		return client.Del(ctx, socketPairKey(listenerClientId)).Err()
	})
}

// Ends refs' leases, then tells the listing, and returns what each found.
func (self *SocketStore) removeLeases(ctx context.Context, client server.RedisClient, networkId connect.Id, refs []*socketsservice.SocketStoreEntryRef, nowMillis int64) ([]*socketsservice.SocketStoreRemoved, error) {
	calls := make([]*socketScriptCall, 0, len(refs))
	for _, ref := range refs {
		calls = append(calls, socketDelCall(networkId, ref.NameKey, ref.ListenerClientId, nowMillis))
	}
	replies, err := self.runScripts(ctx, client, calls)
	if err != nil {
		return nil, err
	}
	removed := []*socketsservice.SocketStoreRemoved{}
	nameUpdates := []*socketNameUpdate{}
	for index, reply := range replies {
		found, last, _, maxLeaseMillis, err := socketGroupReply(reply)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		removed = append(removed, &socketsservice.SocketStoreRemoved{
			NetworkId:        networkId,
			ListenerClientId: refs[index].ListenerClientId,
			NameKey:          bytes.Clone(refs[index].NameKey),
			LastListener:     last,
		})
		nameUpdates = append(nameUpdates, &socketNameUpdate{nameKey: refs[index].NameKey, maxLeaseMillis: maxLeaseMillis})
	}
	if err := self.updateNames(ctx, client, networkId, nowMillis, nameUpdates); err != nil {
		return nil, err
	}
	return removed, nil
}

// See socketsservice.SocketStore.
func (self *SocketStore) RemoveClient(ctx context.Context, clientId connect.Id) ([]*socketsservice.SocketStoreRemoved, error) {
	nowMillis := self.clock.Now().UnixMilli()
	removed := []*socketsservice.SocketStoreRemoved{}
	err := self.redis(ctx, func(client server.RedisClient) error {
		if err := client.Del(ctx, socketPairKey(clientId)).Err(); err != nil {
			return err
		}
		setKey := socketClientSetKey(clientId)
		members, err := client.ZRange(ctx, setKey, 0, -1).Result()
		if err != nil {
			return err
		}
		var networkId connect.Id
		haveNetwork := false
		refs := []*socketsservice.SocketStoreEntryRef{}
		for _, member := range members {
			switch {
			case strings.HasPrefix(member, "\x00") && 17 <= len(member):
				copy(networkId[:], member[1:17])
				haveNetwork = true
			case strings.HasPrefix(member, "\x01") && 1 < len(member):
				refs = append(refs, &socketsservice.SocketStoreEntryRef{ListenerClientId: clientId, NameKey: []byte(member[1:])})
			}
		}
		if !haveNetwork {
			return client.Del(ctx, setKey).Err()
		}
		// the leases first, then the set, so a failure leaves leases to expire and not a set naming
		// leases already gone
		if removed, err = self.removeLeases(ctx, client, networkId, refs, nowMillis); err != nil {
			return err
		}
		return client.Del(ctx, setKey).Err()
	})
	if err != nil {
		return nil, err
	}
	return removed, nil
}

// See socketsservice.SocketStore.
func (self *SocketStore) RemoveEntries(ctx context.Context, networkId connect.Id, refs []*socketsservice.SocketStoreEntryRef) ([]*socketsservice.SocketStoreRemoved, error) {
	for _, ref := range refs {
		if len(ref.NameKey) == 0 {
			return nil, fmt.Errorf("%w: no name key", socketsservice.ErrSocketStoreRequest)
		}
	}
	nowMillis := self.clock.Now().UnixMilli()
	removed := []*socketsservice.SocketStoreRemoved{}
	err := self.redis(ctx, func(client server.RedisClient) error {
		var err error
		if removed, err = self.removeLeases(ctx, client, networkId, refs, nowMillis); err != nil {
			return err
		}
		// each client's set forgets the entry, one key at a time since each set is its own slot
		pipe := client.Pipeline()
		for _, ref := range refs {
			pipe.ZRem(ctx, socketClientSetKey(ref.ListenerClientId), "\x01"+string(ref.NameKey))
		}
		_, err = pipe.Exec(ctx)
		return err
	})
	if err != nil {
		return nil, err
	}
	return removed, nil
}
