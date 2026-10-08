// A positive held PostgreSQL owner defines the exclusion interval. Callback
// release ordering alone is deliberately never treated as a locking oracle.
package model

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"sync"
	"testing"

	"github.com/urnetwork/server"
)

type parallelCloseCommonOwnership struct {
	stateLock                sync.Mutex
	hot                      server.PgOwnershipKey
	grants                   map[server.PgOwnershipKey]bool
	accounts                 map[server.PgOwnershipKey]bool
	grantAdmitted            map[server.PgOwnershipKey]bool
	accountAdmitted          map[server.PgOwnershipKey]bool
	hotAdmittedBackends      map[uint32]bool
	events                   map[string]int64
	transactionEvents        map[string]int64
	sessionEvents            map[string]int64
	holding                  bool
	backendEnded             bool
	windowOpened             bool
	windowClosed             bool
	backendEndedDuringWindow bool
	holderAdmitted           bool
	hotAttemptsWhileHeld     int64
	hotWaitsWhileHeld        int64
	hotRefusalsWhileHeld     int64
	hotAdmissionsWhileHeld   int64
	windowEndedBeforeOpened  bool
}

func newParallelCloseCommonOwnership(fixture parallelPublicCloseFixture) *parallelCloseCommonOwnership {
	observation := &parallelCloseCommonOwnership{
		hot:    server.NewPgOwnershipKey("transfer_balance", fixture.payers[0].balanceId),
		grants: map[server.PgOwnershipKey]bool{}, accounts: map[server.PgOwnershipKey]bool{},
		grantAdmitted: map[server.PgOwnershipKey]bool{}, accountAdmitted: map[server.PgOwnershipKey]bool{},
		hotAdmittedBackends: map[uint32]bool{}, events: map[string]int64{},
	}
	for _, payer := range fixture.payers {
		observation.grants[server.NewPgOwnershipKey("transfer_balance", payer.balanceId)] = true
	}
	for _, provider := range fixture.finance.providers {
		observation.accounts[server.NewPgOwnershipKey("account_balance", provider.destinationNetworkId)] = true
	}
	return observation
}

// The source-pinned adapters acquire these keys before shared business SQL.
// This hook neither blocks a worker nor decides financial admission. Keeping
// cumulative positive witnesses avoids confusing a delayed Released callback
// with the physical ownership of a newer operation on the same backend.
func (self *parallelCloseCommonOwnership) observe(event server.PgOwnershipEvent) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	name := "unknown"
	switch event.Kind {
	case server.PgOwnershipWaiting:
		name = "waiting"
	case server.PgOwnershipAdmitted:
		name = "admitted"
	case server.PgOwnershipReleased:
		name = "released"
	case server.PgOwnershipRefused:
		name = "refused"
	case server.PgOwnershipUncertain:
		name = "uncertain"
	}
	self.events[name]++
	if event.TransactionScoped {
		if self.transactionEvents == nil {
			self.transactionEvents = map[string]int64{}
		}
		self.transactionEvents[name]++
	} else {
		if self.sessionEvents == nil {
			self.sessionEvents = map[string]int64{}
		}
		self.sessionEvents[name]++
	}
	if event.BackendPid == 0 {
		self.events["missing_backend_identity"]++
	}
	hot := false
	for _, key := range event.Keys {
		hot = hot || key == self.hot
		if event.Kind == server.PgOwnershipAdmitted {
			if self.grants[key] {
				self.grantAdmitted[key] = true
			}
			if self.accounts[key] {
				self.accountAdmitted[key] = true
			}
		}
	}
	if !hot {
		return
	}
	if event.Kind == server.PgOwnershipAdmitted {
		self.hotAdmittedBackends[event.BackendPid] = true
		if self.holding {
			self.hotAdmissionsWhileHeld++
		}
	}
	if self.holding && (event.Kind == server.PgOwnershipWaiting || event.Kind == server.PgOwnershipRefused) {
		self.hotAttemptsWhileHeld++
		if event.Kind == server.PgOwnershipWaiting {
			self.hotWaitsWhileHeld++
		} else {
			self.hotRefusalsWhileHeld++
		}
	}
}

// Open only after the real grant row/idle-transaction witness. No public peer
// calls have launched yet. The exact advisory key is independently inspected
// on that same backend before the window is accepted at the final verdict.
func (self *parallelCloseCommonOwnership) beginHeldWindow(pid uint32) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.holderAdmitted = self.hotAdmittedBackends[pid]
	self.windowEndedBeforeOpened = self.backendEnded
	self.windowOpened = true
	self.holding = !self.backendEnded
}

// Close after a second positive row/key witness and before releasing Ready(T).
// The backend cannot make progress through the paused client in this interval.
// A later commit notification is not used to order admissions across sessions.
func (self *parallelCloseCommonOwnership) closeHeldWindow() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	closedWhileHeld := self.holding && !self.backendEnded
	self.holding = false
	self.windowClosed = true
	return closedWhileHeld
}

// Terminal reply/loss is an independent audit. Its receipt can lag actual
// PostgreSQL release, so it never extends the asserted exclusion interval.
func (self *parallelCloseCommonOwnership) backendEnd() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.backendEndedDuringWindow = self.backendEndedDuringWindow || self.holding
	self.holding = false
	self.backendEnded = true
}

func (self *parallelCloseCommonOwnership) observedContender() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.hotAttemptsWhileHeld > 0
}

func (self *parallelCloseCommonOwnership) snapshot() map[string]any {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	events := map[string]int64{}
	for name, count := range self.events {
		events[name] = count
	}
	transactionEvents, sessionEvents := map[string]int64{}, map[string]int64{}
	for name, count := range self.transactionEvents {
		transactionEvents[name] = count
	}
	for name, count := range self.sessionEvents {
		sessionEvents[name] = count
	}
	return map[string]any{"events_total": events, "transaction_events": transactionEvents, "session_events": sessionEvents,
		"expected_grant_keys": len(self.grants), "admitted_grant_keys": len(self.grantAdmitted),
		"expected_account_keys": len(self.accounts), "admitted_account_keys": len(self.accountAdmitted),
		"holder_admitted_on_actual_backend": self.holderAdmitted, "backend_end_observed": self.backendEnded,
		"held_window_opened": self.windowOpened, "held_window_closed_before_release": self.windowClosed,
		"backend_ended_during_asserted_window":            self.backendEndedDuringWindow,
		"hot_key_refusal_or_wait_observations_while_held": self.hotAttemptsWhileHeld,
		"hot_key_session_waits_while_held":                self.hotWaitsWhileHeld,
		"hot_key_transaction_refusals_while_held":         self.hotRefusalsWhileHeld,
		"hot_key_admissions_while_actual_backend_held":    self.hotAdmissionsWhileHeld,
		"window_ended_before_opened":                      self.windowEndedBeforeOpened}
}

func (self *parallelCloseCommonOwnership) requireComplete() error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if !self.windowOpened || !self.windowClosed || !self.backendEnded || !self.holderAdmitted ||
		self.windowEndedBeforeOpened || self.backendEndedDuringWindow {
		return errors.New("common owner lacked a complete positively held backend window")
	}
	if self.hotAttemptsWhileHeld == 0 || self.hotAdmissionsWhileHeld != 0 {
		return errors.New("common owner did not exclude an actual competing hot-key admission")
	}
	if len(self.grantAdmitted) != len(self.grants) || len(self.accountAdmitted) != len(self.accounts) {
		return errors.New("public-close work lacked a positive admission for every expected grant/account key")
	}
	if self.events["unknown"] != 0 || self.events["uncertain"] != 0 || self.events["missing_backend_identity"] != 0 {
		return errors.New("common ownership observation contained unknown or uncertain custody")
	}
	return nil
}

// Read the exact two-integer advisory key on the actual row-owning backend.
// This diagnostic mapping is bound to NewPgOwnershipKey's source hash. Logical
// identity and money authority remain in production; no CTID or lease replaces
// either. The observer performs no attempted acquisition and cannot add a wait.
func parallelPublicCloseCommonKeyHeld(ctx context.Context, observer server.PgConn, pid int32, domain string, id server.Id) bool {
	hash := sha256.New()
	_, _ = hash.Write([]byte("urnetwork:business-owner:v1\x00"))
	_, _ = hash.Write([]byte(domain))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write(id[:])
	sum := hash.Sum(nil)
	first, second := binary.BigEndian.Uint32(sum[:4]), binary.BigEndian.Uint32(sum[4:8])
	var held bool
	server.Raise(observer.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks
      WHERE pid=$1 AND locktype='advisory' AND mode='ExclusiveLock' AND granted
        AND classid=$2::oid AND objid=$3::oid AND objsubid=2
        AND database=(SELECT oid FROM pg_database WHERE datname=current_database()))`, pid, first, second).Scan(&held))
	return held
}

func TestParallelCloseCommonOwnerObservationUsesActualHeldWindow(t *testing.T) {
	hot := server.NewPgOwnershipKey("transfer_balance", server.NewId())
	other := server.NewPgOwnershipKey("account_balance", server.NewId())
	observation := &parallelCloseCommonOwnership{hot: hot,
		grants: map[server.PgOwnershipKey]bool{hot: true}, accounts: map[server.PgOwnershipKey]bool{other: true},
		grantAdmitted: map[server.PgOwnershipKey]bool{}, accountAdmitted: map[server.PgOwnershipKey]bool{},
		hotAdmittedBackends: map[uint32]bool{}, events: map[string]int64{}}
	observation.observe(server.PgOwnershipEvent{Kind: server.PgOwnershipAdmitted, BackendPid: 11, Keys: []server.PgOwnershipKey{hot}})
	observation.beginHeldWindow(11)
	observation.observe(server.PgOwnershipEvent{Kind: server.PgOwnershipAdmitted, BackendPid: 12, Keys: []server.PgOwnershipKey{other}})
	observation.observe(server.PgOwnershipEvent{Kind: server.PgOwnershipRefused, BackendPid: 13, Keys: []server.PgOwnershipKey{hot}, TransactionScoped: true})
	if !observation.holderAdmitted || !observation.observedContender() || observation.hotAdmissionsWhileHeld != 0 {
		t.Fatal("positive holder/contender or independent-key evidence was misclassified")
	}
	observation.observe(server.PgOwnershipEvent{Kind: server.PgOwnershipAdmitted, BackendPid: 14, Keys: []server.PgOwnershipKey{hot}})
	if observation.hotAdmissionsWhileHeld != 1 {
		t.Fatal("a conflicting admitted writer was silently accepted while the backend was held")
	}
	if !observation.closeHeldWindow() {
		t.Fatal("positive window did not close before the physical barrier release")
	}
	observation.observe(server.PgOwnershipEvent{Kind: server.PgOwnershipAdmitted, BackendPid: 15, Keys: []server.PgOwnershipKey{hot}})
	observation.backendEnd()
	observation.observe(server.PgOwnershipEvent{Kind: server.PgOwnershipReleased, BackendPid: 11, Keys: []server.PgOwnershipKey{hot}})
	if observation.hotAdmissionsWhileHeld != 1 || !observation.backendEnded || observation.backendEndedDuringWindow {
		t.Fatal("a delayed release callback manufactured a conflicting business interval")
	}
	if observation.requireComplete() == nil {
		t.Fatal("the earlier conflicting admission was lost at final validation")
	}
}

func TestParallelCloseCommonOwnerObservationRequiresCompleteEvidence(t *testing.T) {
	for _, mode := range []string{"healthy", "early_backend_end", "no_contender", "missing_account", "uncertain"} {
		hot := server.NewPgOwnershipKey("transfer_balance", server.NewId())
		account := server.NewPgOwnershipKey("account_balance", server.NewId())
		observation := &parallelCloseCommonOwnership{hot: hot,
			grants: map[server.PgOwnershipKey]bool{hot: true}, accounts: map[server.PgOwnershipKey]bool{account: true},
			grantAdmitted: map[server.PgOwnershipKey]bool{}, accountAdmitted: map[server.PgOwnershipKey]bool{},
			hotAdmittedBackends: map[uint32]bool{}, events: map[string]int64{}}
		observation.observe(server.PgOwnershipEvent{Kind: server.PgOwnershipAdmitted, BackendPid: 11, Keys: []server.PgOwnershipKey{hot}})
		observation.beginHeldWindow(11)
		if mode != "no_contender" {
			observation.observe(server.PgOwnershipEvent{Kind: server.PgOwnershipWaiting, BackendPid: 12, Keys: []server.PgOwnershipKey{hot}})
		}
		if mode != "missing_account" {
			observation.observe(server.PgOwnershipEvent{Kind: server.PgOwnershipAdmitted, BackendPid: 13, Keys: []server.PgOwnershipKey{account}})
		}
		if mode == "early_backend_end" {
			observation.backendEnd()
		}
		observation.closeHeldWindow()
		// A later admission can precede receipt of the older owner's
		// COMMIT and Released observations without violating this window.
		observation.observe(server.PgOwnershipEvent{Kind: server.PgOwnershipAdmitted, BackendPid: 14, Keys: []server.PgOwnershipKey{hot}})
		observation.backendEnd()
		kind := server.PgOwnershipReleased
		if mode == "uncertain" {
			kind = server.PgOwnershipUncertain
		}
		observation.observe(server.PgOwnershipEvent{Kind: kind, BackendPid: 11, Keys: []server.PgOwnershipKey{hot}})
		if (observation.requireComplete() == nil) != (mode == "healthy") {
			t.Fatal("ownership evidence gate accepted missing custody or refused a complete positive window", mode)
		}
	}
}
