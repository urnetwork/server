package model

// §18.7's steps 5 and 6 (SDK-M8-11; D-SDK-16, WK1, WK5): taskworker's pass over the token projection,
// every 60 s (work.PushTokenPass).
//
// Loss (step 5). Each of the 16,384 hash slots has a sentinel, {tokgen_<n>} for the least n whose tag
// hashes to the slot, holding the run_id of the master that served the slot at its last reconcile, with no
// ttl, so volatile-ttl never evicts it. Masters have no replicas and come back empty or from their last
// RDB dump, and a run_id is random per process start, so a sentinel that is absent or names another
// run_id than the master CLUSTER SHARDS shows serving its slot marks the slot found: its tok and tokrev
// may hold an older or removed token. A found slot is reconciled at the first pass at least ReconcileWait
// (90 s) after the pass that found it, if the same master still serves it, by which time every apply
// begun before that master took the slot has expired (the apply window is 60 s and §19.6's chrony holds
// masters within 50 ms): read t0 on that master; list the slot's tok and tokrev keys by CLUSTER
// GETKEYSINSLOT; read the slot's rows by sidecar_push_token.slot and apply each; DEL tok and tokrev of
// each listed client that has no row; then SET the sentinel. Keys are listed before rows are read, since
// a row commits before any key of its client exists: a client whose first POST lands mid-reconcile is not
// listed, or has its row read, so it keeps its token. GETKEYSINSLOT and INFO run on that master's own
// connection, never through the cluster router.
//
// Sweep (step 6). The same pass applies every row changed since SweepOverlap (2 min) before the last
// completed pass began, so an apply lost with a killed api lands within about 2 minutes with no app
// launch, and deletes each row RemovedRowRetention (24 h) after its client's removal, after DELing its tok
// and tokrev.
//
// Work per pass is bounded: the sweep by an (update_time, client_id) cursor and a row count, the
// deletions by the removed_time index and a count, the reconciles by a slot count, all by a run budget,
// and what is left resumes at the next pass from the state the pass returns, which the task carries in
// its args. Found slots ride the state too, so a restart costs at most another ReconcileWait.
//
// A non-cluster Redis, the local and test stacks', is one master serving every slot, whose slot's keys
// are listed by one SCAN a pass.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/pushservice"

	"github.com/urnetwork/server"
)

// The pass's tunables.
type SidecarPushTokenPassSettings struct {
	// How often the pass runs: 60 s (§18.7).
	RunInterval time.Duration
	// How soon a pass that left work behind runs again: 10 s.
	RetryInterval time.Duration
	// How long after the pass that found a slot the slot is reconciled: 90 s (§18.7). It must exceed
	// SidecarPushTokenSettings.ApplyWindow and the masters' clock skew.
	ReconcileWait time.Duration
	// How far before the last completed pass's start the sweep reaches back: 2 min (§18.7).
	SweepOverlap time.Duration
	// How long a removed client's row stays: 24 h (§18.7, api-6).
	RemovedRowRetention time.Duration
	// Slots one pass reconciles at most: all 16,384, which §18.7 sizes at about 50 s for every slot at
	// 20,000 writes/s, so the run budget, not this, normally bounds a pass after a whole cluster restarts.
	ReconcileSlotCount int
	// Rows one page of a slot's reconcile reads: 1,000.
	SlotRowPageCount int
	// Keys one slot may hold for its listing to be whole: 10,000,000. A slot holding more is listed in
	// part, its rows still applied, and only the listed clients without a row lose their keys.
	SlotKeyLimit int
	// Sentinels one pipeline reads: 1,024.
	SentinelPageCount int
	// Rows one page of the sweep reads: 1,000.
	SweepPageCount int
	// Rows one pass sweeps at most: 100,000; the rest resume at the next pass from the cursor.
	SweepRowCount int
	// Removed rows one pass deletes at most: 10,000.
	DeleteRowCount int
	// The longest one Redis call may take: 10 s.
	CallTimeout time.Duration
	// How long a pass works before it leaves the rest to the next: 45 s, inside the run interval.
	RunBudget time.Duration
}

func DefaultSidecarPushTokenPassSettings() *SidecarPushTokenPassSettings {
	return &SidecarPushTokenPassSettings{
		RunInterval:         60 * time.Second,
		RetryInterval:       10 * time.Second,
		ReconcileWait:       90 * time.Second,
		SweepOverlap:        2 * time.Minute,
		RemovedRowRetention: 24 * time.Hour,
		ReconcileSlotCount:  server.RedisSlotCount,
		SlotRowPageCount:    1000,
		SlotKeyLimit:        10000000,
		SentinelPageCount:   1024,
		SweepPageCount:      1000,
		SweepRowCount:       100000,
		DeleteRowCount:      10000,
		CallTimeout:         10 * time.Second,
		RunBudget:           45 * time.Second,
	}
}

// For each hash slot, the least n whose {tokgen_<n>} hashes to it, computed once.
var pushTokenSentinelNumbers = sync.OnceValue(func() []int {
	numbers := make([]int, server.RedisSlotCount)
	for slot := range numbers {
		numbers[slot] = -1
	}
	remaining := server.RedisSlotCount
	for n := 0; 0 < remaining; n += 1 {
		slot := server.RedisHashSlot(pushTokenSentinelKeyOf(n))
		if numbers[slot] < 0 {
			numbers[slot] = n
			remaining -= 1
		}
	}
	return numbers
})

// {tokgen_<n>}.
func pushTokenSentinelKeyOf(n int) string {
	return "{tokgen_" + strconv.Itoa(n) + "}"
}

// The n of slot's sentinel: the least whose tag hashes to it.
func PushTokenSentinelNumber(slot int) int {
	return pushTokenSentinelNumbers()[slot]
}

// slot's sentinel, {tokgen_<n>}.
func PushTokenSentinelKey(slot int) string {
	return pushTokenSentinelKeyOf(PushTokenSentinelNumber(slot))
}

// One master of the topology, as a pass sees it.
type PushTokenMaster struct {
	// host:port, as the cluster names it.
	Address string
	// INFO's run_id, random per process start.
	RunId string
	// Every slot CLUSTER SHARDS shows it serving.
	Slots []int
}

var (
	// The master no longer serves the slot: the slot waits again.
	errPushTokenSlotMoved = errors.New("push token: the slot moved")
)

// What a pass reads of the topology and lists on one master.
type pushTokenTopology interface {
	// Every master, with its run_id and slots.
	Masters(ctx context.Context) ([]*PushTokenMaster, error)
	// On master's own connection, which must still serve slot: its TIME in milliseconds, then every key
	// of slot, at most limit of them, and whether there were more.
	SlotTimeAndKeys(ctx context.Context, master *PushTokenMaster, slot int, limit int) (t0 int64, keys []string, more bool, err error)
}

// What a pass runs its scripts and keys on, through the cluster's router.
type pushTokenStore interface {
	pushservice.HintRedis
	// GET of each key, nil for an absent one, each key its own command on its own slot.
	GetEach(ctx context.Context, keys []string) ([][]byte, error)
	// SET with no ttl.
	SetPersistent(ctx context.Context, key string, value []byte) error
	// DEL of keys sharing one tag.
	Delete(ctx context.Context, keys []string) error
}

// What a pass carries to the next, in its task's args.
type PushTokenPassState struct {
	// When the last pass whose sweep completed began. Zero before any, when the next sweep reads every
	// row.
	LastCompletedStartTime time.Time `json:"last_completed_start_time"`
	// A sweep a pass left unfinished, which the next resumes.
	Sweep *PushTokenSweepCursor `json:"sweep,omitempty"`
	// Slots whose sentinel was absent or named another run_id, by the master that served them and the
	// start of the pass that found them.
	Found []*PushTokenFoundSlots `json:"found,omitempty"`
}

// Where an unfinished sweep resumes.
type PushTokenSweepCursor struct {
	// When the pass that began this sweep started: the next LastCompletedStartTime once it completes.
	StartTime time.Time `json:"start_time"`
	// The last row it applied; rows past (UpdateTime, ClientId) remain.
	UpdateTime time.Time `json:"update_time"`
	ClientId   server.Id `json:"client_id"`
}

// Slots found under one master by one pass.
type PushTokenFoundSlots struct {
	RunId     string    `json:"run_id"`
	FoundTime time.Time `json:"found_time"`
	Slots     []int     `json:"slots"`
}

// What one pass did.
type PushTokenPassReport struct {
	// Slots this pass found, whose ReconcileWait starts now.
	FoundSlotCount int `json:"found_slot_count"`
	// Found slots still waiting after this pass.
	WaitingSlotCount    int `json:"waiting_slot_count"`
	ReconciledSlotCount int `json:"reconciled_slot_count"`
	ReconciledRowCount  int `json:"reconciled_row_count"`
	// Listed clients with no row, whose tok and tokrev the reconciles deleted.
	DeletedClientCount int  `json:"deleted_client_count"`
	SweptRowCount      int  `json:"swept_row_count"`
	SweepComplete      bool `json:"sweep_complete"`
	DeletedRowCount    int  `json:"deleted_row_count"`
	// Work is left that the next pass continues: run it sooner.
	More bool `json:"more"`
}

// Runs in tests between a reconcile's stages: "listed", once the slot's keys are listed and before its
// rows are read, and "applied", once its rows are applied and before the unlisted clients' keys go.
var pushTokenReconcileTestHook func(stage string, slot int)

// One pass at startTime: the loss check and its due reconciles, when wake is on; the sweep of changed
// rows, when wake is on; and the deletion of rows 24 h after their client's removal, always, since that
// is the rows' ttl. Returns the state the next pass starts from. An error in one part is reported and the
// other parts still run; the state keeps what did not finish.
func RunPushTokenPass(
	ctx context.Context,
	state *PushTokenPassState,
	startTime time.Time,
	wake bool,
	store pushTokenStore,
	topology pushTokenTopology,
	settings *SidecarPushTokenPassSettings,
	tokenSettings *SidecarPushTokenSettings,
) (*PushTokenPassState, *PushTokenPassReport, error) {
	if !server.SidecarMg8Linked() {
		return state, &PushTokenPassReport{}, ErrPushTokenUnavailable
	}
	if state == nil {
		state = &PushTokenPassState{}
	}
	if settings == nil {
		settings = DefaultSidecarPushTokenPassSettings()
	}
	if tokenSettings == nil {
		tokenSettings = DefaultSidecarPushTokenSettings()
	}
	pass := &pushTokenPass{
		ctx:           ctx,
		startTime:     startTime,
		budgetEnd:     time.Now().Add(settings.RunBudget),
		store:         store,
		topology:      topology,
		settings:      settings,
		tokenSettings: tokenSettings,
		report:        &PushTokenPassReport{},
	}
	next := &PushTokenPassState{
		LastCompletedStartTime: state.LastCompletedStartTime,
		Sweep:                  state.Sweep,
		Found:                  state.Found,
	}
	errs := []error{}
	if wake {
		found, err := pass.reconcile(state.Found)
		if err != nil {
			errs = append(errs, fmt.Errorf("loss: %w", err))
		} else {
			next.Found = found
		}
		lastCompletedStartTime, sweep, err := pass.sweep(state.LastCompletedStartTime, state.Sweep)
		next.LastCompletedStartTime, next.Sweep = lastCompletedStartTime, sweep
		if err != nil {
			errs = append(errs, fmt.Errorf("sweep: %w", err))
		}
	}
	if err := pass.deleteRemovedRows(); err != nil {
		errs = append(errs, fmt.Errorf("removed rows: %w", err))
	}
	return next, pass.report, errors.Join(errs...)
}

// One pass's work.
type pushTokenPass struct {
	ctx           context.Context
	startTime     time.Time
	budgetEnd     time.Time
	store         pushTokenStore
	topology      pushTokenTopology
	settings      *SidecarPushTokenPassSettings
	tokenSettings *SidecarPushTokenSettings
	report        *PushTokenPassReport
}

// Whether the run budget is spent or the pass is canceled.
func (self *pushTokenPass) done() bool {
	return self.ctx.Err() != nil || !time.Now().Before(self.budgetEnd)
}

// ctx bounded by CallTimeout.
func (self *pushTokenPass) callContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(self.ctx, self.settings.CallTimeout)
}

// A found slot, as the pass indexes them.
type pushTokenFoundSlot struct {
	runId     string
	foundTime time.Time
}

// Step 5: compares every sentinel with its slot's master's run_id, marks the slots found, reconciles those
// due, and returns what is still found.
func (self *pushTokenPass) reconcile(previous []*PushTokenFoundSlots) ([]*PushTokenFoundSlots, error) {
	foundSlots := map[int]*pushTokenFoundSlot{}
	for _, group := range previous {
		for _, slot := range group.Slots {
			foundSlots[slot] = &pushTokenFoundSlot{runId: group.RunId, foundTime: group.FoundTime}
		}
	}

	masters, err := func() ([]*PushTokenMaster, error) {
		callCtx, cancel := self.callContext()
		defer cancel()
		return self.topology.Masters(callCtx)
	}()
	if err != nil {
		return nil, err
	}
	slotMasters := map[int]*PushTokenMaster{}
	for _, master := range masters {
		for _, slot := range master.Slots {
			slotMasters[slot] = master
		}
	}
	slots := make([]int, 0, len(slotMasters))
	for slot := range slotMasters {
		slots = append(slots, slot)
	}
	slices.Sort(slots)

	sentinels := map[int][]byte{}
	for page := range slices.Chunk(slots, max(self.settings.SentinelPageCount, 1)) {
		keys := make([]string, 0, len(page))
		for _, slot := range page {
			keys = append(keys, PushTokenSentinelKey(slot))
		}
		values, err := func() ([][]byte, error) {
			callCtx, cancel := self.callContext()
			defer cancel()
			return self.store.GetEach(callCtx, keys)
		}()
		if err != nil {
			return nil, err
		}
		for index, slot := range page {
			sentinels[slot] = values[index]
		}
	}

	nextFound := map[int]*pushTokenFoundSlot{}
	dueSlots := []int{}
	for _, slot := range slots {
		master := slotMasters[slot]
		if sentinel := sentinels[slot]; sentinel != nil && string(sentinel) == master.RunId {
			continue
		}
		prior, ok := foundSlots[slot]
		if ok && prior.runId == master.RunId {
			nextFound[slot] = prior
			if self.settings.ReconcileWait <= self.startTime.Sub(prior.foundTime) {
				dueSlots = append(dueSlots, slot)
			}
			continue
		}
		// newly found, or its master changed since: it waits from this pass
		nextFound[slot] = &pushTokenFoundSlot{runId: master.RunId, foundTime: self.startTime}
		self.report.FoundSlotCount += 1
	}

	reconcileErrs := []error{}
	for index, slot := range dueSlots {
		if self.settings.ReconcileSlotCount <= index || self.done() {
			self.report.More = true
			break
		}
		err := self.reconcileSlot(slot, slotMasters[slot])
		if err == nil {
			delete(nextFound, slot)
			self.report.ReconciledSlotCount += 1
			continue
		}
		if errors.Is(err, errPushTokenSlotMoved) {
			// another master took it since this pass read the topology: the next pass finds it under that
			// master, and it waits again from there
			delete(nextFound, slot)
			continue
		}
		reconcileErrs = append(reconcileErrs, fmt.Errorf("slot %d: %w", slot, err))
	}
	self.report.WaitingSlotCount = len(nextFound)
	return groupPushTokenFoundSlots(nextFound), errors.Join(reconcileErrs...)
}

// The found slots grouped by master and finding pass, each group's slots ascending.
func groupPushTokenFoundSlots(foundSlots map[int]*pushTokenFoundSlot) []*PushTokenFoundSlots {
	type groupKey struct {
		runId     string
		foundTime int64
	}
	groups := map[groupKey]*PushTokenFoundSlots{}
	for slot, found := range foundSlots {
		key := groupKey{runId: found.runId, foundTime: found.foundTime.UnixNano()}
		group, ok := groups[key]
		if !ok {
			group = &PushTokenFoundSlots{RunId: found.runId, FoundTime: found.foundTime}
			groups[key] = group
		}
		group.Slots = append(group.Slots, slot)
	}
	grouped := make([]*PushTokenFoundSlots, 0, len(groups))
	for _, group := range groups {
		slices.Sort(group.Slots)
		grouped = append(grouped, group)
	}
	slices.SortFunc(grouped, func(a *PushTokenFoundSlots, b *PushTokenFoundSlots) int {
		if c := a.FoundTime.Compare(b.FoundTime); c != 0 {
			return c
		}
		return strings.Compare(a.RunId, b.RunId)
	})
	return grouped
}

// The client whose tok or tokrev key is, or false for any other key.
func parsePushTokenKey(key string) (server.Id, bool) {
	rest, found := strings.CutPrefix(key, "{scp_")
	if !found {
		return server.Id{}, false
	}
	idText, suffix, found := strings.Cut(rest, "}")
	if !found || (suffix != "tok" && suffix != "tokrev") {
		return server.Id{}, false
	}
	clientId, err := server.ParseId(idText)
	if err != nil {
		return server.Id{}, false
	}
	return clientId, true
}

// One found slot's reconcile on master, which must still serve it: t0 and the listing on its own
// connection, the slot's rows applied, the listed clients with no row deleted, then the sentinel.
func (self *pushTokenPass) reconcileSlot(slot int, master *PushTokenMaster) error {
	t0, keys, more, err := func() (int64, []string, bool, error) {
		callCtx, cancel := self.callContext()
		defer cancel()
		return self.topology.SlotTimeAndKeys(callCtx, master, slot, self.settings.SlotKeyLimit)
	}()
	if err != nil {
		return err
	}
	if more {
		glog.Warningf("[pt]slot %d holds over %d keys: its listing is in part\n", slot, self.settings.SlotKeyLimit)
	}
	listedClientIds := map[server.Id]bool{}
	for _, key := range keys {
		if clientId, ok := parsePushTokenKey(key); ok && PushTokenSlot(clientId) == slot {
			listedClientIds[clientId] = true
		}
	}
	if pushTokenReconcileTestHook != nil {
		pushTokenReconcileTestHook("listed", slot)
	}

	rowClientIds := map[server.Id]bool{}
	cursor := server.Id{}
	for {
		rows, err := readPushTokenSlotRows(self.ctx, slot, cursor, self.settings.SlotRowPageCount)
		if err != nil {
			return err
		}
		for _, row := range rows {
			rowClientIds[row.ClientId] = true
			if err := self.applyRow(row, &t0); err != nil {
				return err
			}
			self.report.ReconciledRowCount += 1
		}
		if len(rows) < self.settings.SlotRowPageCount {
			break
		}
		cursor = rows[len(rows)-1].ClientId
	}
	if pushTokenReconcileTestHook != nil {
		pushTokenReconcileTestHook("applied", slot)
	}

	unrowedClientIds := []server.Id{}
	for clientId := range listedClientIds {
		if !rowClientIds[clientId] {
			unrowedClientIds = append(unrowedClientIds, clientId)
		}
	}
	slices.SortFunc(unrowedClientIds, func(a server.Id, b server.Id) int { return a.Cmp(b) })
	for _, clientId := range unrowedClientIds {
		if err := self.deleteKeys(clientId); err != nil {
			return err
		}
		self.report.DeletedClientCount += 1
	}

	callCtx, cancel := self.callContext()
	defer cancel()
	return self.store.SetPersistent(callCtx, PushTokenSentinelKey(slot), []byte(master.RunId))
}

// Applies row with *t0, and once more with a t0 read again if that expired, keeping the newer t0 for the
// rows after it. A row that does not land is no error here: a later pass's sweep or reconcile lands it.
func (self *pushTokenPass) applyRow(row *SidecarPushTokenRow, t0 *int64) error {
	for attempt := 0; attempt < 2; attempt += 1 {
		applied, err := func() (pushservice.WakeTokenApplyResult, error) {
			callCtx, cancel := self.callContext()
			defer cancel()
			return pushservice.ApplyWakeToken(callCtx, self.store, row.wakeTokenRow(), *t0, self.tokenSettings.ApplyWindow)
		}()
		if err != nil {
			if errors.Is(err, pushservice.ErrWakeTokenInvalid) {
				// a row the apply cannot carry stays as it is; nothing projects it
				glog.Warningf("[pt]row of %s at rev %d is not applicable\n", row.ClientId, row.Rev)
				return nil
			}
			return err
		}
		if applied != pushservice.WakeTokenExpired {
			return nil
		}
		next, err := func() (int64, error) {
			callCtx, cancel := self.callContext()
			defer cancel()
			return pushservice.ReadWakeTokenTime(callCtx, self.store, connect.Id(row.ClientId))
		}()
		if err != nil {
			return err
		}
		*t0 = next
	}
	return nil
}

// DEL of clientId's tok and tokrev, one call on its slot.
func (self *pushTokenPass) deleteKeys(clientId server.Id) error {
	callCtx, cancel := self.callContext()
	defer cancel()
	return self.store.Delete(callCtx, []string{
		pushservice.HintTokenKey(connect.Id(clientId)),
		pushservice.WakeTokenRevisionKey(connect.Id(clientId)),
	})
}

// A page of slot's rows past cursor, by client_id.
func readPushTokenSlotRows(ctx context.Context, slot int, cursor server.Id, count int) ([]*SidecarPushTokenRow, error) {
	rows := []*SidecarPushTokenRow{}
	var returnErr error
	server.Db(ctx, func(conn server.PgConn) {
		rows, returnErr = []*SidecarPushTokenRow{}, nil
		result, err := conn.Query(
			ctx,
			`
				SELECT `+pushTokenColumns+`
				FROM sidecar_push_token
				WHERE slot = $1 AND client_id > $2
				ORDER BY client_id
				LIMIT $3
			`,
			slot,
			cursor,
			count,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				row, err := scanPushTokenRow(result)
				if err != nil {
					returnErr = err
					return
				}
				rows = append(rows, row)
			}
		})
	})
	return rows, returnErr
}

// Step 6's apply: every row changed since SweepOverlap before the last completed pass began, in
// (update_time, client_id) order, at most SweepRowCount a pass. Returns the new last completed start time
// and the cursor a pass left unfinished, nil once complete.
func (self *pushTokenPass) sweep(lastCompletedStartTime time.Time, cursor *PushTokenSweepCursor) (time.Time, *PushTokenSweepCursor, error) {
	if cursor == nil {
		from := time.Time{}
		if !lastCompletedStartTime.IsZero() {
			from = lastCompletedStartTime.Add(-self.settings.SweepOverlap)
		}
		cursor = &PushTokenSweepCursor{StartTime: self.startTime, UpdateTime: from}
	} else {
		resumed := *cursor
		cursor = &resumed
	}
	for self.report.SweptRowCount < self.settings.SweepRowCount {
		if self.done() {
			self.report.More = true
			return lastCompletedStartTime, cursor, nil
		}
		count := min(self.settings.SweepPageCount, self.settings.SweepRowCount-self.report.SweptRowCount)
		rows, err := readPushTokenChangedRows(self.ctx, cursor.UpdateTime, cursor.ClientId, count)
		if err != nil {
			return lastCompletedStartTime, cursor, err
		}
		for _, row := range rows {
			t0, err := func() (int64, error) {
				callCtx, cancel := self.callContext()
				defer cancel()
				return pushservice.ReadWakeTokenTime(callCtx, self.store, connect.Id(row.ClientId))
			}()
			if err != nil {
				return lastCompletedStartTime, cursor, err
			}
			if err := self.applyRow(row, &t0); err != nil {
				return lastCompletedStartTime, cursor, err
			}
			cursor.UpdateTime, cursor.ClientId = row.UpdateTime, row.ClientId
			self.report.SweptRowCount += 1
		}
		if len(rows) < count {
			self.report.SweepComplete = true
			return cursor.StartTime, nil, nil
		}
	}
	self.report.More = true
	return lastCompletedStartTime, cursor, nil
}

// A page of rows past (updateTime, clientId), in that order.
func readPushTokenChangedRows(ctx context.Context, updateTime time.Time, clientId server.Id, count int) ([]*SidecarPushTokenRow, error) {
	rows := []*SidecarPushTokenRow{}
	var returnErr error
	server.Db(ctx, func(conn server.PgConn) {
		rows, returnErr = []*SidecarPushTokenRow{}, nil
		result, err := conn.Query(
			ctx,
			`
				SELECT `+pushTokenColumns+`
				FROM sidecar_push_token
				WHERE (update_time, client_id) > ($1, $2)
				ORDER BY update_time, client_id
				LIMIT $3
			`,
			updateTime.UTC(),
			clientId,
			count,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				row, err := scanPushTokenRow(result)
				if err != nil {
					returnErr = err
					return
				}
				rows = append(rows, row)
			}
		})
	})
	return rows, returnErr
}

// Deletes each row RemovedRowRetention after its client's removal, after DELing its tok and tokrev, at
// most DeleteRowCount a pass, oldest removal first. A row whose DEL fails stays for a later pass.
func (self *pushTokenPass) deleteRemovedRows() error {
	cutoff := self.startTime.Add(-self.settings.RemovedRowRetention).UTC()
	clientIds := []server.Id{}
	var readErr error
	server.Db(self.ctx, func(conn server.PgConn) {
		clientIds, readErr = []server.Id{}, nil
		result, err := conn.Query(
			self.ctx,
			`
				SELECT client_id
				FROM sidecar_push_token
				WHERE removed_time IS NOT NULL AND removed_time < $1
				ORDER BY removed_time, client_id
				LIMIT $2
			`,
			cutoff,
			self.settings.DeleteRowCount,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var clientId server.Id
				if err := result.Scan(&clientId); err != nil {
					readErr = err
					return
				}
				clientIds = append(clientIds, clientId)
			}
		})
	})
	if readErr != nil {
		return readErr
	}
	if len(clientIds) == self.settings.DeleteRowCount {
		self.report.More = true
	}
	deletedKeyClientIds := []server.Id{}
	var keyErr error
	for _, clientId := range clientIds {
		if self.ctx.Err() != nil {
			break
		}
		if err := self.deleteKeys(clientId); err != nil {
			keyErr = err
			continue
		}
		deletedKeyClientIds = append(deletedKeyClientIds, clientId)
	}
	if len(deletedKeyClientIds) == 0 {
		return keyErr
	}
	server.Tx(self.ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			self.ctx,
			`
				DELETE FROM sidecar_push_token
				WHERE
					client_id = ANY($1) AND
					removed_time IS NOT NULL AND
					removed_time < $2
			`,
			deletedKeyClientIds,
			cutoff,
		))
		self.report.DeletedRowCount = int(tag.RowsAffected())
	}, server.TxReadCommitted)
	return keyErr
}

// The production pass's store and topology: server's Redis client, a cluster in production and one node
// on the local and test stacks.
func NewPushTokenServerRedis(ctx context.Context) (pushTokenStore, pushTokenTopology) {
	var client server.RedisClient
	server.Redis(ctx, func(c server.RedisClient) {
		client = c
	})
	return NewPushTokenRedisStore(client), NewPushTokenRedisTopology(client)
}

// A pushTokenStore on client.
func NewPushTokenRedisStore(client redis.UniversalClient) pushTokenStore {
	return &pushTokenRedisStore{client: client}
}

type pushTokenRedisStore struct {
	client redis.UniversalClient
	// *redis.Script by RedisScript.Name
	scripts sync.Map
}

// See sidecar.Redis.
func (self *pushTokenRedisStore) RunScript(ctx context.Context, script *sidecar.RedisScript, keys []string, args []string) (*sidecar.RedisReply, error) {
	if !sidecar.RedisKeysShareTag(keys) {
		return nil, errSidecarPushCrossSlot
	}
	value, _ := self.scripts.LoadOrStore(script.Name, redis.NewScript(script.Source))
	redisScript := value.(*redis.Script)
	argv := make([]any, len(args))
	for index, arg := range args {
		argv[index] = arg
	}
	reply, err := redisScript.Run(ctx, self.client, keys, argv...).Result()
	if errors.Is(err, redis.Nil) {
		reply, err = nil, nil
	}
	if err != nil {
		return nil, err
	}
	return sidecarRedisReply(reply)
}

func (self *pushTokenRedisStore) GetEach(ctx context.Context, keys []string) ([][]byte, error) {
	commands := make([]*redis.StringCmd, len(keys))
	_, err := self.client.Pipelined(ctx, func(pipe redis.Pipeliner) error {
		for index, key := range keys {
			commands[index] = pipe.Get(ctx, key)
		}
		return nil
	})
	if err != nil && !errors.Is(err, redis.Nil) {
		return nil, err
	}
	values := make([][]byte, len(keys))
	for index, command := range commands {
		value, err := command.Bytes()
		if errors.Is(err, redis.Nil) {
			continue
		}
		if err != nil {
			return nil, err
		}
		values[index] = value
	}
	return values, nil
}

func (self *pushTokenRedisStore) SetPersistent(ctx context.Context, key string, value []byte) error {
	return self.client.Set(ctx, key, value, 0).Err()
}

func (self *pushTokenRedisStore) Delete(ctx context.Context, keys []string) error {
	if !sidecar.RedisKeysShareTag(keys) {
		return errSidecarPushCrossSlot
	}
	return self.client.Del(ctx, keys...).Err()
}

// A pushTokenTopology on client: CLUSTER SHARDS and each master's INFO on a cluster, else one master
// serving every slot.
func NewPushTokenRedisTopology(client redis.UniversalClient) pushTokenTopology {
	return &pushTokenRedisTopology{client: client}
}

type pushTokenRedisTopology struct {
	client redis.UniversalClient

	stateLock sync.Mutex
	// a non-cluster node's tok and tokrev keys by slot, from one SCAN a topology
	singleSlotKeys map[int][]string
}

// INFO server's run_id.
func pushTokenRunId(ctx context.Context, node redis.Cmdable) (string, error) {
	info, err := node.Info(ctx, "server").Result()
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(info, "\n") {
		if runId, found := strings.CutPrefix(strings.TrimSpace(line), "run_id:"); found && runId != "" {
			return runId, nil
		}
	}
	return "", errors.New("push token: INFO server holds no run_id")
}

func (self *pushTokenRedisTopology) Masters(ctx context.Context) ([]*PushTokenMaster, error) {
	clusterClient, ok := self.client.(*redis.ClusterClient)
	if !ok {
		runId, err := pushTokenRunId(ctx, self.client)
		if err != nil {
			return nil, err
		}
		slots := make([]int, server.RedisSlotCount)
		for slot := range slots {
			slots[slot] = slot
		}
		return []*PushTokenMaster{{Address: "", RunId: runId, Slots: slots}}, nil
	}
	shards, err := clusterClient.ClusterShards(ctx).Result()
	if err != nil {
		return nil, err
	}
	addressSlots := map[string][]int{}
	for _, shard := range shards {
		for _, node := range shard.Nodes {
			if node.Role != "master" {
				continue
			}
			host := node.IP
			if host == "" {
				host = node.Endpoint
			}
			address := host + ":" + strconv.FormatInt(node.Port, 10)
			for _, slotRange := range shard.Slots {
				for slot := slotRange.Start; slot <= slotRange.End; slot += 1 {
					addressSlots[address] = append(addressSlots[address], int(slot))
				}
			}
		}
	}
	// ForEachMaster runs its callback concurrently, one goroutine per master
	var mastersLock sync.Mutex
	masters := []*PushTokenMaster{}
	err = clusterClient.ForEachMaster(ctx, func(ctx context.Context, node *redis.Client) error {
		address := node.Options().Addr
		slots, served := addressSlots[address]
		if !served {
			return nil
		}
		runId, err := pushTokenRunId(ctx, node)
		if err != nil {
			return err
		}
		mastersLock.Lock()
		defer mastersLock.Unlock()
		masters = append(masters, &PushTokenMaster{Address: address, RunId: runId, Slots: slots})
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(masters, func(a *PushTokenMaster, b *PushTokenMaster) int { return strings.Compare(a.Address, b.Address) })
	return masters, nil
}

func (self *pushTokenRedisTopology) SlotTimeAndKeys(ctx context.Context, master *PushTokenMaster, slot int, limit int) (int64, []string, bool, error) {
	clusterClient, ok := self.client.(*redis.ClusterClient)
	if !ok {
		return self.singleSlotTimeAndKeys(ctx, master, slot, limit)
	}
	node, err := clusterClient.MasterForKey(ctx, PushTokenSentinelKey(slot))
	if err != nil {
		return 0, nil, false, err
	}
	if node.Options().Addr != master.Address {
		return 0, nil, false, errPushTokenSlotMoved
	}
	runId, err := pushTokenRunId(ctx, node)
	if err != nil {
		return 0, nil, false, err
	}
	if runId != master.RunId {
		return 0, nil, false, errPushTokenSlotMoved
	}
	now, err := node.Time(ctx).Result()
	if err != nil {
		return 0, nil, false, err
	}
	count, err := node.ClusterCountKeysInSlot(ctx, slot).Result()
	if err != nil {
		return 0, nil, false, err
	}
	more := int64(limit) < count
	keys, err := node.ClusterGetKeysInSlot(ctx, slot, int(min(count, int64(limit)))).Result()
	if err != nil {
		return 0, nil, false, err
	}
	return now.UnixMilli(), keys, more, nil
}

// The single node's TIME and slot's keys, from one SCAN of the token keys per topology.
func (self *pushTokenRedisTopology) singleSlotTimeAndKeys(ctx context.Context, master *PushTokenMaster, slot int, limit int) (int64, []string, bool, error) {
	runId, err := pushTokenRunId(ctx, self.client)
	if err != nil {
		return 0, nil, false, err
	}
	if runId != master.RunId {
		return 0, nil, false, errPushTokenSlotMoved
	}
	now, err := self.client.Time(ctx).Result()
	if err != nil {
		return 0, nil, false, err
	}
	scanned := func() map[int][]string {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.singleSlotKeys
	}()
	if scanned == nil {
		scanned = map[int][]string{}
		iterator := self.client.Scan(ctx, 0, "{scp_*}tok*", 1000).Iterator()
		for iterator.Next(ctx) {
			key := iterator.Val()
			if clientId, ok := parsePushTokenKey(key); ok {
				keySlot := PushTokenSlot(clientId)
				scanned[keySlot] = append(scanned[keySlot], key)
			}
		}
		if err := iterator.Err(); err != nil {
			return 0, nil, false, err
		}
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.singleSlotKeys = scanned
		}()
	}
	keys := scanned[slot]
	more := limit < len(keys)
	if more {
		keys = keys[:limit]
	}
	return now.UnixMilli(), slices.Clone(keys), more, nil
}
