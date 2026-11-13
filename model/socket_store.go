package model

// The sockets engine's store on Redis Cluster (§9.8, D-SOCK-7; SOCK-M5-04): sidecar's
// socketsservice.SocketStore on §2.8's sock-1, sock-3, sock-4 and sock-5, and nothing in Postgres
// (I-12). Keys:
//
//	{sg_<network_id>_<h16>}      a group's hash: per listener m, r, i, e and its tombstone x, and v
//	                             (socket_store_scripts.go)
//	{sg_<network_id>_<h16>}ix    the group's leases, a ZSET of listener ids scored by lease end in ms
//	{svc_<listener_client_id>}   §6.10.1's pairs: field the dialer, value the grant's expiry, and the floor
//	                             ~f a removal left (sock-3); keyed by the end that is not the edge, so a ur/
//	                             Find's pair is under its sidecar dialer, field the edge listener (QD-728)
//	{sn_<network_id>}names       the listing: field the name key, value kind;lease_ms;sealed, and the
//	                             sweep's claim in field 0x00 (sock-4)
//	{sc_<client_id>}set          the client's set, a ZSET of 0x01 || name key and one meta member (sock-5)
//
// The fences are DESIGN-DETAILS 13.2.3's (SOCK-M5-18): a lease's position and set version in m, a removal's
// tombstone x, a pair floor ~f and the sweep's claim, each kept to the clock this store passes its scripts.
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
	"sync/atomic"
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
	// When set, every script runs through it, one call each, rather than on the deadline pool: a test's counted
	// Redis (§22.5's hot-path gate). Nil in production.
	scriptRedis atomic.Pointer[socketScriptRunner]
}

// Scripts run through a sidecar.Redis, each one call.
type socketScriptRunner struct {
	redis sidecar.Redis
}

// Every script as a sidecar.RedisScript, named sockets.<name>, for a runner.
var socketStoreSidecarScripts = func() map[string]*sidecar.RedisScript {
	scripts := map[string]*sidecar.RedisScript{}
	for name, source := range socketStoreScriptSources {
		scripts[name] = &sidecar.RedisScript{Name: "sockets." + name, Source: source}
	}
	return scripts
}()

// One script through the runner's Redis, its arguments as go-redis would write them and its reply as go-redis would
// read it, a nil reply being redis.Nil.
func (self *socketScriptRunner) run(ctx context.Context, name string, keys []string, args []any) (any, error) {
	argv := make([]string, len(args))
	for index, arg := range args {
		switch arg := arg.(type) {
		case string:
			argv[index] = arg
		case []byte:
			argv[index] = string(arg)
		case int:
			argv[index] = strconv.Itoa(arg)
		case int64:
			argv[index] = strconv.FormatInt(arg, 10)
		case uint32:
			argv[index] = strconv.FormatUint(uint64(arg), 10)
		case uint64:
			argv[index] = strconv.FormatUint(arg, 10)
		default:
			return nil, fmt.Errorf("socket store: a script argument of type %T", arg)
		}
	}
	reply, err := self.redis.RunScript(ctx, socketStoreSidecarScripts[name], keys, argv)
	if err != nil {
		return nil, err
	}
	var value func(reply *sidecar.RedisReply) any
	value = func(reply *sidecar.RedisReply) any {
		switch reply.Kind {
		case sidecar.RedisReplyInteger:
			return reply.Integer
		case sidecar.RedisReplyBulk:
			return string(reply.Bulk)
		case sidecar.RedisReplyArray:
			values := make([]any, len(reply.Replies))
			for index, element := range reply.Replies {
				values[index] = value(element)
			}
			return values
		}
		return nil
	}
	if reply == nil || reply.Kind == sidecar.RedisReplyNil {
		return nil, redis.Nil
	}
	return value(reply), nil
}

// Runs every later script through redis, one call each, and nil goes back to the deadline pool: what §22.5's hot-path
// gate counts a composed path's calls with (sidecart3.InlinePath). A test's alone.
func (self *SocketStore) Testing_RunScriptsThrough(redis sidecar.Redis) {
	if redis == nil {
		self.scriptRedis.Store(nil)
		return
	}
	self.scriptRedis.Store(&socketScriptRunner{redis: redis})
}

// Calls observe with each script's name and keys as it runs: §22.4's slot check over a whole composed path. A test's
// alone, set before the store is shared.
func (self *SocketStore) Testing_ObserveScripts(observe func(name string, keys []string)) {
	self.scriptCallback = observe
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
	if runner := self.scriptRedis.Load(); runner != nil {
		return runner.run(ctx, name, keys, args)
	}
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
	if runner := self.scriptRedis.Load(); runner != nil {
		replies := make([]any, len(calls))
		for index, call := range calls {
			self.noteScript(call.name, call.keys)
			reply, err := runner.run(ctx, call.name, call.keys, call.args)
			if err != nil {
				return nil, err
			}
			replies[index] = reply
		}
		return replies, nil
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

// What m:<L>'s last three fields hold: the listener's sidecar, the lease's position and its set
// version. An m of the first twelve fields alone reads as nil, 0 and 0.
func socketParseLeaseRecord(record string) (*connect.Id, uint64, uint64, error) {
	fields := strings.Split(record, ";")
	if len(fields) == 12 {
		return nil, 0, 0, nil
	}
	if len(fields) != 15 {
		return nil, 0, 0, errors.New("socket store: a malformed registration record")
	}
	var sidecarId *connect.Id
	if fields[12] != "" {
		id, err := socketParseHexId(fields[12])
		if err != nil {
			return nil, 0, 0, err
		}
		sidecarId = &id
	}
	policyVersion, err := strconv.ParseUint(fields[13], 10, 64)
	if err != nil {
		return nil, 0, 0, errors.New("socket store: a malformed lease position")
	}
	setVersion, err := strconv.ParseUint(fields[14], 10, 64)
	if err != nil {
		return nil, 0, 0, errors.New("socket store: a malformed lease set version")
	}
	return sidecarId, policyVersion, setVersion, nil
}

// What m:<L> and r:<L> hold, back as a registration.
func socketParseRegistrationRecord(record string, inflightRecord string) (*sidecar.SocketRegistration, int64, error) {
	malformed := errors.New("socket store: a malformed registration record")
	fields := strings.Split(record, ";")
	if len(fields) != 12 && len(fields) != 15 {
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
	sidecarId, policyVersion, setVersion, err := socketParseLeaseRecord(values[2])
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
		Affinity:             registration.Affinity,
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
	return &socketsservice.SocketStoreListener{
		Registration:  registration,
		Candidate:     candidate,
		SidecarId:     sidecarId,
		PolicyVersion: policyVersion,
		SetVersion:    setVersion,
	}, nil
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

// A uint64 as the scripts compare it: decimal, no leading zeros.
func socketDecimal(value uint64) string {
	return strconv.FormatUint(value, 10)
}

// How long a tombstone of cause fences, in milliseconds.
func (self *SocketStore) tombstoneTtlMillis(cause socketsservice.SocketRemoveCause) int64 {
	ttl := self.settings.Store.TombstoneTtl
	if cause == socketsservice.SocketRemoveUnlisten {
		ttl = self.settings.Store.UnlistenFenceTtl
	}
	return (ttl + time.Millisecond - 1).Milliseconds()
}

// One group's del for cause at (setVersion, policyVersion), pipelined with the others. An empty
// setVersion is the lease's own, as a removal no page carries passes.
func (self *SocketStore) delCall(networkId connect.Id, nameKey []byte, listenerClientId connect.Id, cause socketsservice.SocketRemoveCause, setVersion string, policyVersion uint64, nowMillis int64) *socketScriptCall {
	return &socketScriptCall{
		name: "group",
		keys: socketGroupKeys(networkId, nameKey),
		args: []any{"del", nowMillis, socketHexId(listenerClientId), int(cause), setVersion, socketDecimal(policyVersion), self.tombstoneTtlMillis(cause)},
	}
}

// What a group's put or del answered: its status, its flag, the newest lease and a position, with put's
// fence cause.
type socketGroupAnswer struct {
	status         string
	flag           bool
	maxLeaseMillis int64
	position       uint64
	cause          socketsservice.SocketRemoveCause
}

// A put's reply, six values, or a del's, five.
func socketParseGroupReply(reply any) (*socketGroupAnswer, error) {
	values, err := socketReplyStrings(reply)
	if err != nil {
		return nil, err
	}
	if len(values) != 5 && len(values) != 6 {
		return nil, errors.New("socket store: a malformed group reply")
	}
	answer := &socketGroupAnswer{status: values[0], flag: values[1] == "1"}
	if answer.maxLeaseMillis, err = strconv.ParseInt(values[3], 10, 64); err != nil {
		return nil, err
	}
	if answer.position, err = strconv.ParseUint(values[4], 10, 64); err != nil {
		return nil, err
	}
	if len(values) == 6 {
		cause, err := strconv.Atoi(values[5])
		if err != nil {
			return nil, err
		}
		answer.cause = socketsservice.SocketRemoveCause(cause)
	}
	return answer, nil
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

// Whether a page is one the engine could send: entries and refused keys each sorted, inside the range,
// non-empty, and no name key both allowed and refused.
func socketPageWellFormed(page *socketsservice.SocketStorePage) error {
	if page == nil {
		return fmt.Errorf("%w: no page", socketsservice.ErrSocketStoreRequest)
	}
	var previousNameKey []byte
	allowedNameKeys := map[string]bool{}
	for index, registration := range page.Registrations {
		nameKey := registration.NameKey()
		if len(nameKey) == 0 || !socketInRange(nameKey, page.KeyStart, page.KeyEnd) ||
			(0 < index && bytes.Compare(nameKey, previousNameKey) <= 0) {
			return fmt.Errorf("%w: entry %d", socketsservice.ErrSocketStoreRequest, index)
		}
		previousNameKey = nameKey
		allowedNameKeys[string(nameKey)] = true
	}
	for index, nameKey := range page.RefusedNameKeys {
		if len(nameKey) == 0 || !socketInRange(nameKey, page.KeyStart, page.KeyEnd) || allowedNameKeys[string(nameKey)] ||
			(0 < index && bytes.Compare(nameKey, page.RefusedNameKeys[index-1]) <= 0) {
			return fmt.Errorf("%w: refused entry %d", socketsservice.ErrSocketStoreRequest, index)
		}
	}
	return nil
}

// See socketsservice.SocketStore. The page's puts, its refused entries' removals and its unlistens run
// as one pipeline of group scripts between the set's begin and its commit, each group touched once.
func (self *SocketStore) ApplyPage(ctx context.Context, page *socketsservice.SocketStorePage) (*socketsservice.SocketStorePageResult, error) {
	if err := socketPageWellFormed(page); err != nil {
		return nil, err
	}
	nowMillis := self.clock.Now().UnixMilli()
	leaseMillis := self.settings.Store.Lease.Milliseconds()
	setKey := socketClientSetKey(page.ListenerClientId)
	setVersion := socketDecimal(page.SetVersion)
	policyVersion := socketDecimal(page.PolicyVersion)
	sidecarHex := ""
	if page.SidecarId != nil {
		sidecarHex = socketHexId(*page.SidecarId)
	}

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
		previousNameKeys := map[string]bool{}
		for _, nameKey := range begin[2:] {
			if socketInRange([]byte(nameKey), page.KeyStart, page.KeyEnd) {
				previousNameKeys[nameKey] = true
			}
		}

		calls := []*socketScriptCall{}
		pageNameKeys := map[string]bool{}
		for _, registration := range page.Registrations {
			exclusive := "0"
			if registration.Exclusive {
				exclusive = "1"
			}
			pageNameKeys[string(registration.NameKey())] = true
			calls = append(calls, &socketScriptCall{
				name: "group",
				keys: socketGroupKeys(page.NetworkId, registration.NameKey()),
				args: []any{
					"put", nowMillis, socketHexId(page.ListenerClientId), leaseMillis,
					socketRegistrationRecord(registration), registration.Inflight,
					hex.EncodeToString([]byte(registration.Protocol)), exclusive,
					sidecarHex, setVersion, policyVersion,
				},
			})
		}
		for _, nameKey := range page.RefusedNameKeys {
			pageNameKeys[string(nameKey)] = true
			calls = append(calls, self.delCall(page.NetworkId, nameKey, page.ListenerClientId, socketsservice.SocketRemoveRefused, setVersion, page.PolicyVersion, nowMillis))
		}
		unlistenNameKeys := []string{}
		for nameKey := range previousNameKeys {
			if !pageNameKeys[nameKey] {
				unlistenNameKeys = append(unlistenNameKeys, nameKey)
			}
		}
		slices.Sort(unlistenNameKeys)
		for _, nameKey := range unlistenNameKeys {
			calls = append(calls, self.delCall(page.NetworkId, []byte(nameKey), page.ListenerClientId, socketsservice.SocketRemoveUnlisten, setVersion, 0, nowMillis))
		}
		replies, err := self.runScripts(ctx, client, calls)
		if err != nil {
			return err
		}
		answers := make([]*socketGroupAnswer, len(replies))
		for index, reply := range replies {
			if answers[index], err = socketParseGroupReply(reply); err != nil {
				return err
			}
		}

		result = &socketsservice.SocketStorePageResult{SetVersion: page.SetVersion}
		heldNameKeys := map[string]bool{}
		// a lease a newer page or position wrote, which stays the set's
		standingNameKeys := map[string]bool{}
		lastListenerNameKeys := map[string]bool{}
		refusedRemovedNameKeys := map[string]bool{}
		nameUpdates := []*socketNameUpdate{}
		for index, registration := range page.Registrations {
			answer := answers[index]
			outcome := &socketsservice.SocketStoreEntryOutcome{}
			result.Outcomes = append(result.Outcomes, outcome)
			switch answer.status {
			case "fenced":
				outcome.Fenced = true
				outcome.FencePolicyVersion = answer.position
				outcome.FenceCause = answer.cause
			case "stale":
				outcome.Stale = true
				outcome.PriorPolicyVersion = answer.position
				standingNameKeys[string(registration.NameKey())] = true
			case "taken":
				outcome.Taken = true
				outcome.PriorPolicyVersion = answer.position
			case "ok":
				outcome.FirstListener = answer.flag
				outcome.PriorPolicyVersion = answer.position
				heldNameKeys[string(registration.NameKey())] = true
				update := &socketNameUpdate{nameKey: registration.NameKey(), maxLeaseMillis: answer.maxLeaseMillis, kind: "e"}
				if len(registration.NameToken) != 0 {
					update.kind = "b"
					update.sealed = registration.SealedName
				}
				nameUpdates = append(nameUpdates, update)
			default:
				return fmt.Errorf("socket store: a put answered %q", answer.status)
			}
		}
		delAnswers := answers[len(page.Registrations):]
		for index, nameKey := range page.RefusedNameKeys {
			answer := delAnswers[index]
			removal := &socketsservice.SocketStoreRemoval{}
			switch answer.status {
			case "removed":
				removal.Removed = &socketsservice.SocketStoreRemoved{
					NetworkId:        page.NetworkId,
					ListenerClientId: page.ListenerClientId,
					NameKey:          bytes.Clone(nameKey),
					LastListener:     answer.flag,
				}
				refusedRemovedNameKeys[string(nameKey)] = true
				lastListenerNameKeys[string(nameKey)] = answer.flag
				nameUpdates = append(nameUpdates, &socketNameUpdate{nameKey: nameKey, maxLeaseMillis: answer.maxLeaseMillis})
			case "kept":
				removal.Kept = true
				removal.PolicyVersion = answer.position
				standingNameKeys[string(nameKey)] = true
			}
			result.RefusedOutcomes = append(result.RefusedOutcomes, removal)
		}
		for index, nameKey := range unlistenNameKeys {
			answer := delAnswers[len(page.RefusedNameKeys)+index]
			switch answer.status {
			case "removed":
				lastListenerNameKeys[nameKey] = answer.flag
				nameUpdates = append(nameUpdates, &socketNameUpdate{nameKey: []byte(nameKey), maxLeaseMillis: answer.maxLeaseMillis})
			case "kept":
				standingNameKeys[nameKey] = true
			}
		}

		removedNameKeys := []string{}
		for nameKey := range previousNameKeys {
			if !heldNameKeys[nameKey] && !standingNameKeys[nameKey] {
				removedNameKeys = append(removedNameKeys, nameKey)
			}
		}
		for nameKey := range refusedRemovedNameKeys {
			if !previousNameKeys[nameKey] {
				removedNameKeys = append(removedNameKeys, nameKey)
			}
		}
		slices.Sort(removedNameKeys)
		for _, nameKey := range removedNameKeys {
			result.Removed = append(result.Removed, &socketsservice.SocketStoreRemoved{
				NetworkId:        page.NetworkId,
				ListenerClientId: page.ListenerClientId,
				NameKey:          []byte(nameKey),
				LastListener:     lastListenerNameKeys[nameKey],
			})
		}

		commitArgs := []any{page.NetworkId[:], setVersion, page.KeyStart, page.KeyEnd, self.settings.Store.ClientSetTtl.Milliseconds()}
		committedNameKeys := []string{}
		for nameKey := range heldNameKeys {
			committedNameKeys = append(committedNameKeys, nameKey)
		}
		for nameKey := range standingNameKeys {
			committedNameKeys = append(committedNameKeys, nameKey)
		}
		slices.Sort(committedNameKeys)
		for _, nameKey := range committedNameKeys {
			commitArgs = append(commitArgs, nameKey)
		}
		reply, err = self.runScript(ctx, client, "set_commit", []string{setKey}, commitArgs...)
		if err != nil {
			return err
		}
		commit, err := socketReplyStrings(reply)
		if err != nil || len(commit) != 3 {
			return fmt.Errorf("socket store: a malformed commit reply: %v", err)
		}
		if commit[0] == "stale" {
			stored, err := strconv.ParseUint(commit[1], 10, 64)
			if err != nil {
				return err
			}
			result.Stale = true
			result.SetVersion = stored
		}
		result.EdgeEntriesRemain = commit[2] == "1"
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
		// the key rides ARGV alone and is never written (sock-2)
		string(request.AffinityKey),
		scheduler.AffinityLoadPercent,
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
			if nameKey == socketSweepClaimField {
				continue
			}
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

// The listing's field that holds the sweep's claim, which no name key is.
const socketSweepClaimField = "\x00"

// A ttl in whole milliseconds, rounded up, so a positive ttl never reaches Redis as zero (host_redis.go).
func socketTtlMillis(ttl time.Duration) int64 {
	return (ttl + time.Millisecond - 1).Milliseconds()
}

// See socketsservice.SocketStore.
func (self *SocketStore) SetServicePair(ctx context.Context, listenerClientId connect.Id, dialerClientId connect.Id, grantExpireMillis uint64, ttl time.Duration, position uint64) (bool, uint64, error) {
	if ttl <= 0 {
		return false, 0, fmt.Errorf("%w: a ttl of %v", socketsservice.ErrSocketStoreRequest, ttl)
	}
	nowMillis := self.clock.Now().UnixMilli()
	wrote := false
	floor := uint64(0)
	err := self.redis(ctx, func(client server.RedisClient) error {
		reply, err := self.runScript(ctx, client, "pair", []string{socketPairKey(listenerClientId)},
			socketHexId(dialerClientId), socketDecimal(grantExpireMillis), socketTtlMillis(ttl), socketDecimal(position), nowMillis)
		if err != nil {
			return err
		}
		values, err := socketReplyStrings(reply)
		if err != nil || len(values) != 2 {
			return fmt.Errorf("socket store: a malformed pair reply: %v", err)
		}
		wrote = values[0] == "1"
		floor, err = strconv.ParseUint(values[1], 10, 64)
		return err
	})
	if err != nil {
		return false, 0, err
	}
	return wrote, floor, nil
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

// pair_remove on one listener's key.
func (self *SocketStore) removePairs(ctx context.Context, client server.RedisClient, listenerClientId connect.Id, position uint64, nowMillis int64) error {
	_, err := self.runScript(ctx, client, "pair_remove", []string{socketPairKey(listenerClientId)},
		socketDecimal(position), nowMillis, socketTtlMillis(self.settings.Store.TombstoneTtl))
	return err
}

// See socketsservice.SocketStore.
func (self *SocketStore) RemoveServicePairs(ctx context.Context, listenerClientId connect.Id, position uint64) error {
	nowMillis := self.clock.Now().UnixMilli()
	return self.redis(ctx, func(client server.RedisClient) error {
		return self.removePairs(ctx, client, listenerClientId, position, nowMillis)
	})
}

// See socketsservice.SocketStore.
func (self *SocketStore) TouchServicePairs(ctx context.Context, listenerClientId connect.Id) error {
	nowMillis := self.clock.Now().UnixMilli()
	return self.redis(ctx, func(client server.RedisClient) error {
		_, err := self.runScript(ctx, client, "pair_touch", []string{socketPairKey(listenerClientId)},
			socketTtlMillis(self.settings.Store.EdgePairTtl), nowMillis)
		return err
	})
}

// Removes refs' leases for cause at position, then tells the listing, and answers each ref in order.
func (self *SocketStore) removeLeases(ctx context.Context, client server.RedisClient, networkId connect.Id, cause socketsservice.SocketRemoveCause, position uint64, refs []*socketsservice.SocketStoreEntryRef, nowMillis int64) ([]*socketsservice.SocketStoreRemoval, error) {
	calls := make([]*socketScriptCall, 0, len(refs))
	for _, ref := range refs {
		calls = append(calls, self.delCall(networkId, ref.NameKey, ref.ListenerClientId, cause, "", position, nowMillis))
	}
	replies, err := self.runScripts(ctx, client, calls)
	if err != nil {
		return nil, err
	}
	removals := make([]*socketsservice.SocketStoreRemoval, 0, len(refs))
	nameUpdates := []*socketNameUpdate{}
	for index, reply := range replies {
		answer, err := socketParseGroupReply(reply)
		if err != nil {
			return nil, err
		}
		removal := &socketsservice.SocketStoreRemoval{}
		switch answer.status {
		case "removed":
			removal.Removed = &socketsservice.SocketStoreRemoved{
				NetworkId:        networkId,
				ListenerClientId: refs[index].ListenerClientId,
				NameKey:          bytes.Clone(refs[index].NameKey),
				LastListener:     answer.flag,
			}
			nameUpdates = append(nameUpdates, &socketNameUpdate{nameKey: refs[index].NameKey, maxLeaseMillis: answer.maxLeaseMillis})
		case "kept":
			removal.Kept = true
			removal.PolicyVersion = answer.position
		}
		removals = append(removals, removal)
	}
	if err := self.updateNames(ctx, client, networkId, nowMillis, nameUpdates); err != nil {
		return nil, err
	}
	return removals, nil
}

// See socketsservice.SocketStore. Each batch's leases go first and then the batch leaves the set, so a
// failure leaves the set naming leases still to remove, never leases it has forgotten.
func (self *SocketStore) RemoveClient(ctx context.Context, clientId connect.Id) ([]*socketsservice.SocketStoreRemoved, error) {
	nowMillis := self.clock.Now().UnixMilli()
	batchCount := self.settings.Store.RemoveClientBatchCount
	removed := []*socketsservice.SocketStoreRemoved{}
	err := self.redis(ctx, func(client server.RedisClient) error {
		setKey := socketClientSetKey(clientId)
		meta, err := client.ZRangeByLex(ctx, setKey, &redis.ZRangeBy{Min: "-", Max: "(\x01", Count: 1}).Result()
		if err != nil {
			return err
		}
		if len(meta) == 1 && 17 <= len(meta[0]) {
			var networkId connect.Id
			copy(networkId[:], meta[0][1:17])
			setCount, err := client.ZCard(ctx, setKey).Result()
			if err != nil {
				return err
			}
			// a removed client's pages are refused, so the set only shrinks; the bound stops a loop on
			// pages already in flight
			for round := int64(0); round <= setCount/int64(batchCount)+1; round += 1 {
				members, err := client.ZRangeByLex(ctx, setKey, &redis.ZRangeBy{Min: "[\x01", Max: "+", Count: int64(batchCount)}).Result()
				if err != nil {
					return err
				}
				if len(members) == 0 {
					break
				}
				refs := make([]*socketsservice.SocketStoreEntryRef, 0, len(members))
				for _, member := range members {
					refs = append(refs, &socketsservice.SocketStoreEntryRef{ListenerClientId: clientId, NameKey: []byte(member[1:])})
				}
				removals, err := self.removeLeases(ctx, client, networkId, socketsservice.SocketRemoveClientRemoved, socketsservice.SocketRemovedClientPosition, refs, nowMillis)
				if err != nil {
					return err
				}
				for _, removal := range removals {
					if removal.Removed != nil {
						removed = append(removed, removal.Removed)
					}
				}
				memberValues := make([]any, 0, len(members))
				for _, member := range members {
					memberValues = append(memberValues, member)
				}
				if err := client.ZRem(ctx, setKey, memberValues...).Err(); err != nil {
					return err
				}
			}
		}
		if err := client.Del(ctx, setKey).Err(); err != nil {
			return err
		}
		return self.removePairs(ctx, client, clientId, socketsservice.SocketRemovedClientPosition, nowMillis)
	})
	if err != nil {
		return nil, err
	}
	return removed, nil
}

// See socketsservice.SocketStore.
func (self *SocketStore) RemoveEntries(ctx context.Context, networkId connect.Id, cause socketsservice.SocketRemoveCause, position uint64, refs []*socketsservice.SocketStoreEntryRef) ([]*socketsservice.SocketStoreRemoval, error) {
	if !socketsservice.SocketRemovalAllowed(cause, position) {
		return nil, fmt.Errorf("%w: a removal of cause %d at %d", socketsservice.ErrSocketStoreRequest, cause, position)
	}
	for _, ref := range refs {
		if len(ref.NameKey) == 0 {
			return nil, fmt.Errorf("%w: no name key", socketsservice.ErrSocketStoreRequest)
		}
	}
	nowMillis := self.clock.Now().UnixMilli()
	var removals []*socketsservice.SocketStoreRemoval
	err := self.redis(ctx, func(client server.RedisClient) error {
		var err error
		if removals, err = self.removeLeases(ctx, client, networkId, cause, position, refs, nowMillis); err != nil {
			return err
		}
		// each client's set forgets the leases that are gone and says whether it still holds an edge name: one
		// script a client, pipelined, since each set is its own slot
		listenerClientIds := []connect.Id{}
		listenerClientIdNameKeys := map[connect.Id][]any{}
		for index, ref := range refs {
			nameKeys, found := listenerClientIdNameKeys[ref.ListenerClientId]
			if !found {
				listenerClientIds = append(listenerClientIds, ref.ListenerClientId)
				nameKeys = []any{}
			}
			if !removals[index].Kept {
				nameKeys = append(nameKeys, ref.NameKey)
			}
			listenerClientIdNameKeys[ref.ListenerClientId] = nameKeys
		}
		calls := make([]*socketScriptCall, 0, len(listenerClientIds))
		for _, listenerClientId := range listenerClientIds {
			calls = append(calls, &socketScriptCall{
				name: "set_forget",
				keys: []string{socketClientSetKey(listenerClientId)},
				args: listenerClientIdNameKeys[listenerClientId],
			})
		}
		replies, err := self.runScripts(ctx, client, calls)
		if err != nil {
			return err
		}
		listenerClientIdEdges := map[connect.Id]bool{}
		for index, reply := range replies {
			values, err := socketReplyStrings(reply)
			if err != nil || len(values) != 1 {
				return fmt.Errorf("socket store: a malformed set reply: %v", err)
			}
			listenerClientIdEdges[listenerClientIds[index]] = values[0] == "1"
		}
		for index, ref := range refs {
			removals[index].EdgeEntriesRemain = listenerClientIdEdges[ref.ListenerClientId]
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return removals, nil
}

// See socketsservice.SocketStore.
func (self *SocketStore) ClaimSweep(ctx context.Context, networkId connect.Id, position uint64, ttl time.Duration, held *socketsservice.SocketSweepClaim) (*socketsservice.SocketSweepClaim, bool, error) {
	if ttl <= 0 {
		return nil, false, fmt.Errorf("%w: a ttl of %v", socketsservice.ErrSocketStoreRequest, ttl)
	}
	nowMillis := self.clock.Now().UnixMilli()
	heldPosition, heldUntil := "", ""
	if held != nil {
		heldPosition = socketDecimal(held.PolicyVersion)
		heldUntil = strconv.FormatInt(held.UntilMillis, 10)
	}
	var claim *socketsservice.SocketSweepClaim
	claimed := false
	err := self.redis(ctx, func(client server.RedisClient) error {
		reply, err := self.runScript(ctx, client, "claim", []string{socketNamesKey(networkId)},
			socketDecimal(position), nowMillis, socketTtlMillis(ttl), heldPosition, heldUntil)
		if err != nil {
			return err
		}
		values, err := socketReplyStrings(reply)
		if err != nil || len(values) != 3 {
			return fmt.Errorf("socket store: a malformed claim reply: %v", err)
		}
		claim = &socketsservice.SocketSweepClaim{}
		if claim.PolicyVersion, err = strconv.ParseUint(values[1], 10, 64); err != nil {
			return err
		}
		if claim.UntilMillis, err = strconv.ParseInt(values[2], 10, 64); err != nil {
			return err
		}
		claimed = values[0] == "1"
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return claim, claimed, nil
}
