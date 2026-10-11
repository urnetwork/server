// Remembers definitive client-state refusals per process, so a credential the
// database already refused does not repeat the recursive state query on every
// retry. A client that cannot tell an in-band refusal from a dropped connection
// (every WebSocket build before H1+) redials a dead credential every few
// seconds for as long as it runs.
//
// A refusal is permanent for its exact credential. No production path sets
// network_client.active back to true; client, device, network and user ids are
// minted fresh and never reused; and none of the columns the state query binds
// (network_client.network_id, device_id and source_client_id, and
// network.admin_user_id) is rewritten after creation. A client the query found
// missing, inactive, or cut off from an active root therefore stays refused.
// The query reads the primary after the mint that issued the credential
// committed, so a refusal never precedes its client's own row. The key is the
// client id with every other claim the query binds: another credential of the
// same client, such as a renewed one that carries a root claim, gets a query of
// its own.
//
// Only a completed query that found no active row is remembered. A dependency
// failure, deadline, cancellation or credential rotation always reaches the
// database, and nothing is ever remembered as valid. A future path that
// reactivates a client or rebinds one of those columns must forget the
// refusals it affects in every process; the ttl only bounds the cost of a
// mistake.
//
// Safe for concurrent use.
package session

import (
	"context"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/server"
)

const (
	// a remembered refusal answers for at most this long
	stateRefusalTtl = time.Hour
	// the bound of each of the two generations, so at most twice this many
	// refusals are held
	stateRefusalGenerationCount = 32 * 1024
	// model.ProvideModePublic, the persisted provide_key.provide_mode value
	stateRefusalPublicProvideMode = 3
	// the attribution reads one in this many refused credentials
	stateRefusalClassSample = 16
)

// The client id and every other claim the client state query binds. An absent
// optional claim is marked absent, never read as a zero id.
type stateRefusalKey struct {
	clientId       server.Id
	userId         server.Id
	networkId      server.Id
	deviceId       server.Id
	parentClientId server.Id
	sessionId      server.Id
	rootClientId   server.Id
	present        uint8
}

const (
	stateRefusalParentPresent  uint8 = 1
	stateRefusalSessionPresent uint8 = 2
	stateRefusalRootPresent    uint8 = 4
)

// Only a client credential has a client row to refuse.
func newStateRefusalKey(byJwt *ByJwt, parentClientId *server.Id) (stateRefusalKey, bool) {
	if byJwt.ClientId == nil || byJwt.DeviceId == nil {
		return stateRefusalKey{}, false
	}
	key := stateRefusalKey{
		clientId:  *byJwt.ClientId,
		userId:    byJwt.UserId,
		networkId: byJwt.NetworkId,
		deviceId:  *byJwt.DeviceId,
	}
	if parentClientId != nil {
		key.parentClientId = *parentClientId
		key.present |= stateRefusalParentPresent
	}
	if byJwt.SessionId != nil {
		key.sessionId = *byJwt.SessionId
		key.present |= stateRefusalSessionPresent
	}
	if byJwt.RootClientId != nil {
		key.rootClientId = *byJwt.RootClientId
		key.present |= stateRefusalRootPresent
	}
	return key, true
}

// Two bounded generations. A refusal enters the current generation; a full
// current generation becomes the previous one, and the old previous one is
// dropped whole. A refusal served from the previous generation moves back to
// the current one with its original expiry, so a credential that keeps
// retrying survives the turnover while one that stopped ages out. An expired
// refusal is never served.
type stateRefusalCache struct {
	stateLock           sync.Mutex
	currentKeyExpiries  map[stateRefusalKey]time.Time
	previousKeyExpiries map[stateRefusalKey]time.Time
	generationCount     int
	ttl                 time.Duration
}

func newStateRefusalCache(generationCount int, ttl time.Duration) *stateRefusalCache {
	return &stateRefusalCache{
		currentKeyExpiries:  map[stateRefusalKey]time.Time{},
		previousKeyExpiries: map[stateRefusalKey]time.Time{},
		generationCount:     max(1, generationCount),
		ttl:                 ttl,
	}
}

// Reports whether the credential was refused less than the ttl before now.
func (self *stateRefusalCache) refused(key stateRefusalKey, now time.Time) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if expiry, ok := self.currentKeyExpiries[key]; ok {
		if now.Before(expiry) {
			return true
		}
		delete(self.currentKeyExpiries, key)
		return false
	}
	expiry, ok := self.previousKeyExpiries[key]
	if !ok {
		return false
	}
	delete(self.previousKeyExpiries, key)
	if !now.Before(expiry) {
		return false
	}
	self.putWithLock(key, expiry)
	return true
}

// Records a refusal the database answered at now.
func (self *stateRefusalCache) remember(key stateRefusalKey, now time.Time) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	delete(self.previousKeyExpiries, key)
	self.putWithLock(key, now.Add(self.ttl))
}

func (self *stateRefusalCache) putWithLock(key stateRefusalKey, expiry time.Time) {
	if _, ok := self.currentKeyExpiries[key]; !ok && self.generationCount <= len(self.currentKeyExpiries) {
		self.previousKeyExpiries = self.currentKeyExpiries
		self.currentKeyExpiries = map[stateRefusalKey]time.Time{}
	}
	self.currentKeyExpiries[key] = expiry
}

// Drops every remembered refusal.
func (self *stateRefusalCache) forget() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.currentKeyExpiries = map[stateRefusalKey]time.Time{}
	self.previousKeyExpiries = map[stateRefusalKey]time.Time{}
}

func (self *stateRefusalCache) count() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return len(self.currentKeyExpiries) + len(self.previousKeyExpiries)
}

var stateRefusals = newStateRefusalCache(stateRefusalGenerationCount, stateRefusalTtl)

// Testing_ForgetStateRefusals drops every remembered refusal, for a fixture
// that reactivates a client by hand. Production never reactivates one.
func Testing_ForgetStateRefusals() {
	stateRefusals.forget()
}

type stateRefusalEvent uint8

const (
	stateRefusalStored stateRefusalEvent = iota
	stateRefusalServed
	stateRefusalEventCount
)

var stateRefusalEventLabels = [stateRefusalEventCount]string{"stored", "served"}

var stateRefusalCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork", Subsystem: "jwt", Name: "state_refusals_total",
	Help: "Definitive no-active-row client refusals by trusted caller and operation: stored after a completed state query, or served from memory without one.",
}, []string{"caller", "operation", "event"})

// Resolved once, like the state query counters.
var stateRefusalCounters [stateQuerySourceCount][stateRefusalEventCount]prometheus.Counter

var stateRefusalClassCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork", Subsystem: "jwt", Name: "state_refusal_classes_total",
	Help: "Client credentials refused for no active row, sampled one in 16 by client id and counted when a process first stores the refusal, by trusted caller and operation and by the refused client's own row: missing, inactive, or active with a refused ancestor or binding; its level; and whether it provides publicly.",
}, []string{"caller", "operation", "row", "level", "provide"})

var stateRefusalEntries = prometheus.NewGaugeFunc(prometheus.GaugeOpts{
	Namespace: "urnetwork", Subsystem: "jwt", Name: "state_refusals_remembered",
	Help: "Client credentials whose definitive refusal this process remembers.",
}, func() float64 {
	return float64(stateRefusals.count())
})

func init() {
	for source, labels := range stateQuerySourceLabels {
		for event, label := range stateRefusalEventLabels {
			stateRefusalCounters[source][event] = stateRefusalCounter.WithLabelValues(labels[0], labels[1], label)
		}
	}
	prometheus.MustRegister(stateRefusalCounter, stateRefusalClassCounter, stateRefusalEntries)
	// a test environment starts from a fresh database, where remembered ids
	// mean nothing
	server.OnReset(stateRefusals.forget)
}

// Answers a remembered refusal without a query, and remembers the definitive
// no-active-row verdict of a completed one. The rejection counter and detail
// are the same either way.
func validateByJwtStateRemembered(ctx context.Context, byJwt *ByJwt, parentClientId *server.Id) (returnErr error) {
	key, rememberable := newStateRefusalKey(byJwt, parentClientId)
	source := stateQuerySourceFromContext(ctx)
	if rememberable && stateRefusals.refused(key, time.Now()) {
		stateRefusalCounters[source][stateRefusalServed].Inc()
		return rejectNoActiveRow(byJwt)
	}
	noActiveRow := false
	server.Db(ctx, func(conn server.PgConn) {
		noActiveRow, returnErr = validateByJwtStateQuery(ctx, conn, byJwt, parentClientId)
		if rememberable && noActiveRow && stateRefusalClassified(*byJwt.ClientId) {
			row, level, provide := classifyStateRefusal(ctx, conn, *byJwt.ClientId)
			labels := stateQuerySourceLabels[source]
			stateRefusalClassCounter.WithLabelValues(labels[0], labels[1], row, level, provide).Inc()
		}
	})
	if rememberable && noActiveRow {
		stateRefusals.remember(key, time.Now())
		stateRefusalCounters[source][stateRefusalStored].Inc()
	}
	return
}

// The attribution samples credentials by the random tail of the client id, so
// on a reconnect wave, when every process first stores the same refusals, it
// adds a sliver of their cost. Scale its counts by stateRefusalClassSample.
func stateRefusalClassified(clientId server.Id) bool {
	return clientId[len(clientId)-1]%stateRefusalClassSample == 0
}

// Reads the refused client's own row once, when this process first stores its
// refusal, so the refused population is attributable without ids. It never
// changes the verdict: a failed read is unclassified.
func classifyStateRefusal(ctx context.Context, conn server.PgCanQuery, clientId server.Id) (row string, level string, provide string) {
	row, level, provide = "unclassified", "unknown", "unknown"
	result, err := conn.Query(ctx, `
		SELECT
			network_client.active,
			network_client.source_client_id IS NULL,
			EXISTS (
				SELECT 1 FROM provide_key
				WHERE
					provide_key.client_id = network_client.client_id AND
					provide_key.provide_mode = $2
			)
		FROM network_client
		WHERE network_client.client_id = $1
	`, clientId, stateRefusalPublicProvideMode)
	if err != nil {
		return
	}
	defer result.Close()
	if !result.Next() {
		if result.Err() == nil {
			row = "missing"
		}
		return
	}
	var active, topLevel, public bool
	if result.Scan(&active, &topLevel, &public) != nil {
		return
	}
	row, level, provide = "inactive", "derived", "none"
	if active {
		row = "active"
	}
	if topLevel {
		level = "top_level"
	}
	if public {
		provide = "public"
	}
	return
}
