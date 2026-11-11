package quotahost

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/glog"
	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The lease half of quotahost's ReportUsage (DESIGN-DETAILS 13.2.2's a batch): the engine's batch arrives
// with its events as the engine counted them and its id fixed by sidecar.QuotaUsageBatcher, and before it is
// sent on:
//
//	plan     once per batch id, kept with the batch through its retries: for each event on a lease meter,
//	         min(event bytes, spent - carved) of this process's entries on its (network, meter), the
//	         closed ones first, attributed to draws in rank order and grouped by balance, usage_id
//	         model.ServiceUsageId(batch_id, network, meter, balance); carved rises by what was taken
//	carve    one script a part, under its lane's operationLock. Each source's expect is the token's amount
//	         as the host knows it when the carve is first sent, and every retry repeats it. Moved: the
//	         draws shrink. {0, 0}: the sources are gone, the part is dropped and counted unbilled, and its
//	         draws are dropped as expired. No answer: the batch's call fails, and the batcher's retry
//	         repeats the carve as it was, as does any other operation on its tokens before it is sent
//	post     only when every part is carved or dropped: the batch with its UsageCarve rows, each lease
//	         meter's event carrying the sum of its carved parts and no more, to model.ApplyUsageBatch or
//	         api's route, retried under the batch's id
//	give up  a plan not asked for again in PlanHorizon is dropped: its uncarved parts are counted unbilled,
//	         and a carve sent and unanswered is abandoned, its token's next operation an extend. Its carved
//	         tokens are left alone: the post may have applied, and then the flush releases them; if not,
//	         they lapse at CarvedTokenTtl
//
// A carved token is never released by the host: only the flush, with its debit, releases one.

// One source of a part: bytes of one draw.
type leaseBatchSource struct {
	entry     *quotaLeaseEntry
	draw      *quotaLeaseDraw
	byteCount int64
	// what the token held when the carve was first sent, repeated by every retry
	expectByteCount int64
}

type leaseBatchPartState int

const (
	leaseBatchPartPlanned leaseBatchPartState = iota
	leaseBatchPartCarved
	leaseBatchPartDropped
)

// One balance's bytes of one (network, meter) in a batch: one carve, one journal row.
type leaseBatchPart struct {
	plan      *leaseBatchPlan
	lane      *quotaLeaseLane
	key       quotaLeaseKey
	balanceId server.Id
	usageId   server.Id
	sources   []*leaseBatchSource
	byteCount int64
	// under the lane's operationLock
	state         leaseBatchPartState
	sent          bool
	firstSendTime time.Time
}

// One batch's plan, by its id.
type leaseBatchPlan struct {
	batchId server.Id
	parts   []*leaseBatchPart
	// the bytes each lease meter's events asked for, and what the plan took of this process's spent bytes
	// (its parts' and any given up at attribution), by key: what was asked past what was taken no lease
	// spent, and the post counts it unbilled; a dropped part's and a given-up byte were counted when they were
	askedByteCounts map[quotaLeaseKey]int64
	takenByteCounts map[quotaLeaseKey]int64
	// under the set's stateLock
	lastTime time.Time
	posted   bool
}

// The batch's plan: the one made for its id, or a new one (see the file header). Plans past their horizon
// are given up first.
func (self *quotaLeaseSet) planBatch(batch *sidecar.UsageBatch) *leaseBatchPlan {
	var expireds []*leaseBatchPlan
	plan := func() *leaseBatchPlan {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		expireds = self.takeOldPlansWithLock(self.clock.Now())
		return self.planBatchWithLock(batch)
	}()
	for _, expired := range expireds {
		for _, part := range expired.parts {
			func() {
				part.lane.operationLock.Lock()
				defer part.lane.operationLock.Unlock()
				self.abandonPartWithOperationLock(part)
			}()
		}
	}
	return plan
}

// The plan for the batch's id, made when it has none. Under the set's stateLock.
func (self *quotaLeaseSet) planBatchWithLock(batch *sidecar.UsageBatch) *leaseBatchPlan {
	batchId := server.Id(batch.BatchId)
	now := self.clock.Now()
	if plan, ok := self.plans[batchId]; ok {
		plan.lastTime = now
		return plan
	}
	plan := &leaseBatchPlan{
		batchId:         batchId,
		askedByteCounts: map[quotaLeaseKey]int64{},
		takenByteCounts: map[quotaLeaseKey]int64{},
		lastTime:        now,
	}
	keyBalanceParts := map[quotaLeaseKey]map[server.Id]*leaseBatchPart{}
	for _, event := range batch.Events {
		if event == nil || !isLeaseMeter(event.Meter) || event.TransferByteCount <= 0 {
			continue
		}
		key := quotaLeaseKey{networkId: server.Id(event.NetworkId), meter: event.Meter}
		plan.askedByteCounts[key] += event.TransferByteCount
		lane, ok := self.lanes[key]
		if !ok {
			continue
		}
		entries := slices.Clone(lane.closing)
		if lane.open != nil {
			entries = append(entries, lane.open)
		}
		remaining := event.TransferByteCount
		for _, entry := range entries {
			if remaining <= 0 {
				break
			}
			sources, taken := entry.attribute(remaining)
			remaining -= taken
			plan.takenByteCounts[key] += taken
			for _, source := range sources {
				balanceParts, ok := keyBalanceParts[key]
				if !ok {
					balanceParts = map[server.Id]*leaseBatchPart{}
					keyBalanceParts[key] = balanceParts
				}
				part, ok := balanceParts[source.draw.balanceId]
				if !ok {
					part = &leaseBatchPart{
						plan:      plan,
						lane:      lane,
						key:       key,
						balanceId: source.draw.balanceId,
						usageId:   model.ServiceUsageId(batchId, key.networkId, key.meter, source.draw.balanceId),
					}
					balanceParts[source.draw.balanceId] = part
					plan.parts = append(plan.parts, part)
				}
				// one carve names a token once: a second event's bytes of the same draw join its source
				index := slices.IndexFunc(part.sources, func(other *leaseBatchSource) bool { return other.draw == source.draw })
				if 0 <= index {
					part.sources[index].byteCount += source.byteCount
				} else {
					part.sources = append(part.sources, source)
				}
				part.byteCount += source.byteCount
			}
		}
	}
	self.plans[batchId] = plan
	return plan
}

// Attributes up to byteCount of the entry's spent and unplanned bytes to its draws in rank order, the
// lowest first, raising carved by what it took. taken is every byte it took of the spent and unplanned,
// the sources' and those no live draw held, which it gives up.
func (self *quotaLeaseEntry) attribute(byteCount int64) (sources []*leaseBatchSource, taken int64) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	take := min(byteCount, self.spent.Load()-self.carved)
	if take <= 0 {
		return nil, 0
	}
	taken = take
	for _, draw := range self.sortedDrawsWithLock() {
		if take <= 0 {
			break
		}
		absorbed := min(take, draw.freeByteCount())
		if absorbed <= 0 {
			continue
		}
		draw.pendingByteCount += absorbed
		self.carved += absorbed
		take -= absorbed
		sources = append(sources, &leaseBatchSource{entry: self, draw: draw, byteCount: absorbed})
	}
	if 0 < take {
		// spent bytes no live draw holds, as after a drop raced Spend: given up
		self.carved += take
		quotaLeaseUnbilledByteCounter.WithLabelValues(self.key.meter).Add(float64(take))
	}
	return sources, taken
}

// Takes out the plans not asked for in PlanHorizon, for their parts to be given up. Under the set's
// stateLock.
func (self *quotaLeaseSet) takeOldPlansWithLock(now time.Time) (expireds []*leaseBatchPlan) {
	for batchId, plan := range self.plans {
		if self.settings.PlanHorizon <= now.Sub(plan.lastTime) {
			delete(self.plans, batchId)
			expireds = append(expireds, plan)
		}
	}
	return
}

// Gives up a part its batch will never post: its bytes unbilled, and gone from its tokens as the host knows
// them. A carve sent and unanswered may or may not have moved them, so its tokens' next operation is an
// extend, which learns what each holds. Holds the part's lane's operationLock.
func (self *quotaLeaseSet) abandonPartWithOperationLock(part *leaseBatchPart) {
	if part.state != leaseBatchPartPlanned {
		return
	}
	part.state = leaseBatchPartDropped
	quotaLeaseUnbilledByteCounter.WithLabelValues(part.key.meter).Add(float64(part.byteCount))
	for _, source := range part.sources {
		func() {
			source.entry.stateLock.Lock()
			defer source.entry.stateLock.Unlock()
			draw := source.draw
			draw.pendingByteCount = max(0, draw.pendingByteCount-source.byteCount)
			// as the host knows it the bytes left the token; an extend learns what it holds
			draw.amount = max(0, draw.amount-source.byteCount)
			if part.sent {
				draw.needsExtend = true
			}
			if draw.unanswered == part {
				draw.unanswered = nil
			}
		}()
	}
}

// The lease half of ReportUsage: plans the batch, carves each part not yet carved or dropped, and, once
// every part is, posts the batch with its carves through post. An error leaves the plan for the batcher's
// retry under the same id.
func (self *quotaLeaseSet) reportUsage(ctx context.Context, batch *sidecar.UsageBatch, post func(ctx context.Context, batch *sidecar.UsageBatch, carves []*model.UsageCarve) error) error {
	if batch == nil {
		return errors.New("quota lease: no batch")
	}
	if batch.BatchId == (connect.Id{}) {
		return errors.New("quota lease: a zero batch id")
	}
	plan := self.planBatch(batch)
	// each part's state as its carve left it, read under its lane's lock
	partStates := make([]leaseBatchPartState, len(plan.parts))
	for index, part := range plan.parts {
		err := func() error {
			part.lane.operationLock.Lock()
			defer part.lane.operationLock.Unlock()
			err := self.carvePartWithOperationLock(ctx, part)
			partStates[index] = part.state
			return err
		}()
		if err != nil {
			return err
		}
	}
	posted := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		posted = plan.posted
	}()
	if posted {
		return nil
	}
	carves := []*model.UsageCarve{}
	keyCarvedByteCounts := map[quotaLeaseKey]int64{}
	for index, part := range plan.parts {
		if partStates[index] == leaseBatchPartPlanned {
			// abandoned at its horizon while this call carved: the plan is gone
			return errors.New("quota lease: the batch's plan passed its horizon")
		}
		if partStates[index] == leaseBatchPartCarved {
			carves = append(carves, &model.UsageCarve{
				UsageId:   part.usageId,
				NetworkId: part.key.networkId,
				Meter:     part.key.meter,
				BalanceId: part.balanceId,
				ByteCount: part.byteCount,
			})
			keyCarvedByteCounts[part.key] += part.byteCount
		}
	}
	// each lease meter's events carry the sum of its carved parts and no more
	sent := &sidecar.UsageBatch{
		BatchId:     batch.BatchId,
		Producer:    batch.Producer,
		WindowStart: batch.WindowStart,
	}
	keyGiven := map[quotaLeaseKey]int64{}
	for _, event := range batch.Events {
		if event == nil || !isLeaseMeter(event.Meter) {
			sent.Events = append(sent.Events, event)
			continue
		}
		key := quotaLeaseKey{networkId: server.Id(event.NetworkId), meter: event.Meter}
		given := min(max(event.TransferByteCount, 0), keyCarvedByteCounts[key]-keyGiven[key])
		keyGiven[key] += given
		copied := *event
		copied.TransferByteCount = given
		sent.Events = append(sent.Events, &copied)
	}
	if err := post(ctx, sent, carves); err != nil {
		return err
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if !plan.posted {
			plan.posted = true
			// what the events asked for past what the plan took is no lease's spend: unbilled
			for key, asked := range plan.askedByteCounts {
				if rest := asked - plan.takenByteCounts[key]; 0 < rest {
					quotaLeaseUnbilledByteCounter.WithLabelValues(key.meter).Add(float64(rest))
				}
			}
			// the plan stays to its horizon, so the same id sent again answers posted and is never planned
			// again: planning it again would attribute spent bytes twice (LeaseTwin-Replan)
		}
	}()
	return nil
}

// Carves one part (see the file header), holding its lane's operationLock. A part carved or dropped answers
// nil at once; an unanswered carve answers its error and stays the tokens' unanswered operation.
func (self *quotaLeaseSet) carvePartWithOperationLock(ctx context.Context, part *leaseBatchPart) error {
	settings := self.settings
	if part.state != leaseBatchPartPlanned {
		return nil
	}
	if !part.sent {
		// before the first send: settle every other operation its tokens owe, then fix each expect
		for _, source := range part.sources {
			if err := source.entry.prepareTokenWithOperationLock(ctx, source.draw, part); err != nil {
				return err
			}
		}
		if part.state != leaseBatchPartPlanned {
			return nil
		}
		for _, source := range part.sources {
			func() {
				source.entry.stateLock.Lock()
				defer source.entry.stateLock.Unlock()
				source.expectByteCount = source.draw.amount
				source.draw.unanswered = part
			}()
		}
		part.sent = true
		part.firstSendTime = self.clock.Now()
	}
	carveParts := make([]*model.LeaseCarvePart, 0, len(part.sources))
	for _, source := range part.sources {
		carveParts = append(carveParts, &model.LeaseCarvePart{
			Token:           source.draw.token,
			ByteCount:       source.byteCount,
			ExpectByteCount: source.expectByteCount,
		})
	}
	sendTime := self.clock.Now()
	callCtx, cancel := context.WithTimeout(ctx, settings.CallTimeout)
	moved, _, err := model.CarveLease(callCtx, self.redis, part.balanceId, part.usageId, carveParts, settings.CarvedTokenTtl, settings.TokenTtl)
	cancel()
	if err != nil {
		if glog.V(1) {
			glog.Infof("[quota][lease]a %s carve went unanswered: %s\n", part.key.meter, err)
		}
		return err
	}
	if part.state != leaseBatchPartPlanned {
		// abandoned meanwhile at its plan's horizon
		return nil
	}
	if moved {
		part.state = leaseBatchPartCarved
	} else {
		part.state = leaseBatchPartDropped
		quotaLeaseUnbilledByteCounter.WithLabelValues(part.key.meter).Add(float64(part.byteCount))
	}
	var drops []*leaseBatchSource
	for _, source := range part.sources {
		func() {
			source.entry.stateLock.Lock()
			defer source.entry.stateLock.Unlock()
			draw := source.draw
			if draw.unanswered == part {
				draw.unanswered = nil
			}
			// moved or dropped, the part's bytes leave the token as the host knows it
			draw.pendingByteCount = max(0, draw.pendingByteCount-source.byteCount)
			draw.amount = max(0, draw.amount-source.byteCount)
			if !moved {
				drops = append(drops, source)
				return
			}
			// the carve set the source's expiry: this send's, or the first send's when an earlier try ran
			refreshTime := sendTime
			if part.firstSendTime.Before(refreshTime) {
				refreshTime = part.firstSendTime
			}
			if draw.refreshTime.Before(refreshTime) {
				draw.refreshTime = refreshTime
				draw.spendableUntil = refreshTime.Add(settings.TokenTtl - settings.ExpiryMargin)
			}
			if draw.amount == 0 {
				if index := slices.Index(source.entry.draws, draw); 0 <= index {
					source.entry.draws = slices.Delete(source.entry.draws, index, index+1)
				}
			}
		}()
	}
	for _, source := range drops {
		// the sources were gone or already moved: their draws are dropped as expired
		source.entry.dropDraw(source.draw)
	}
	return nil
}

// Before a carve's first send on draw: an unanswered carve of another part is repeated, and an abandoned
// operation is followed by an extend, so the token's amount is known when the expect is fixed.
func (self *quotaLeaseEntry) prepareTokenWithOperationLock(ctx context.Context, draw *quotaLeaseDraw, part *leaseBatchPart) error {
	var other *leaseBatchPart
	var needsExtend bool
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		other = draw.unanswered
		needsExtend = draw.needsExtend
	}()
	if other != nil && other != part {
		if err := self.set.carvePartWithOperationLock(ctx, other); err != nil {
			return err
		}
	}
	if !needsExtend {
		return nil
	}
	settings := self.set.settings
	sendTime := self.set.clock.Now()
	callCtx, cancel := context.WithTimeout(ctx, settings.CallTimeout)
	amount, err := model.ExtendLease(callCtx, self.set.redis, draw.balanceId, draw.token, settings.TokenTtl)
	cancel()
	if err != nil {
		return fmt.Errorf("quota lease: the extend before a carve went unanswered: %w", err)
	}
	if amount == 0 {
		// the token is gone: the part's bytes from it cannot be carved
		self.dropDraw(draw)
		return nil
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.learnAmountWithLock(draw, amount)
	draw.needsExtend = false
	draw.refreshTime = sendTime
	draw.spendableUntil = sendTime.Add(settings.TokenTtl - settings.ExpiryMargin)
	return nil
}
