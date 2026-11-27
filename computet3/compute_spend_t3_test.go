//go:build sidecar_pending

package computet3

// The read routes on the private stack's PostgreSQL and Redis (BAL-M12-15; DESIGN-COMPUTE §9.10, §9.13, §14.3, D-BAL-6,
// D-BAL-7, §18's spend view row): GET api/compute/spend's figures against the charges they sum, through replayed
// batches and retries; its 7 days counted in UTC whatever the session's zone and date style; its first charge day set
// by a charge that settled late; a tagged sidecar's charges counting to each sponsor in turn, both routes agreeing and
// compute_rollup_day finding nothing; a Member's view and holds their own, every other role's the network's; the spend
// view refused to every device that is not the person's consented one; the balance saying who may buy and whether sales
// are open; and a statement's lots and lapses. Each route runs through sidecar/handler's frame over the process's
// global Env, set to the test's, as server's mount serves it.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"testing"
	"time"

	operator "github.com/urnetwork/operator-core"
	"github.com/urnetwork/operator-core/coretest"
	"github.com/urnetwork/sidecar/controller"
	"github.com/urnetwork/sidecar/handler"
	"github.com/urnetwork/sidecar/model"
	"github.com/urnetwork/sidecar/protocol/bridge"

	"github.com/urnetwork/server"
)

// One network's spend under test, on a fake clock: its members, a device and an agent.
type computeT3Spending struct {
	*computeT3Clocked
	networkId server.Id
	sidecarId server.Id
	agentId   server.Id
	memberA   server.Id
	memberB   server.Id
}

// A network funded with $100, its clock at now, the routes' global Env the test's.
func newComputeT3Spending(t3 *computeT3, now time.Time) *computeT3Spending {
	computeT3RenewRegister()
	clocked := newComputeT3Clocked(t3, now)
	spending := &computeT3Spending{
		computeT3Clocked: clocked, networkId: server.NewId(), sidecarId: server.NewId(), agentId: server.NewId(),
		memberA: server.NewId(), memberB: server.NewId(),
	}
	clocked.purchase(spending.networkId, computeT3Usd(100), nil)
	coretest.SetGlobalEnv(t3.t, clocked.env)
	return spending
}

// One charge's subjects and figures.
type computeT3Charge struct {
	kind      model.ComputeChargeKind
	amount    int64
	start     time.Time
	sidecarId *server.Id
	agentId   *server.Id
	memberId  *server.Id
	enclaveId *server.Id
	catalogId string
	provider  int16
}

// Writes charge's operation settled, as its producer's settlement would, and books it: its key. The row's own
// sidecar is the network's device unless the charge names another.
func (self *computeT3Spending) settle(charge *computeT3Charge) server.Id {
	self.t3.t.Helper()
	opKey := server.NewId()
	sidecarId := self.sidecarId
	if charge.sidecarId != nil {
		sidecarId = *charge.sidecarId
	}
	units := model.ComputeUnits{InputTokens: 1000, OutputTokens: 100}
	switch charge.kind {
	case model.ComputeChargeKindWebSearch:
		units = model.ComputeUnits{Searches: 1}
	case model.ComputeChargeKindEnclaveRun:
		units = model.ComputeUnits{Seconds: 600}
	}
	op := &model.ComputeOp{
		OpKey: opKey, NetworkId: self.networkId, State: model.ComputeOpStateSettled, Kind: charge.kind, SubjectRef: server.NewId(),
		SidecarId: sidecarId, AgentId: charge.agentId, MemberId: charge.memberId, EnclaveId: charge.enclaveId,
		Provider: max(1, charge.provider), StartTime: charge.start, HoldNanoUsd: charge.amount, ReserveNanoUsd: charge.amount,
		Outcome: &model.ComputeOpOutcome{Units: units, AmountNanoUsd: charge.amount, CostNanoUsd: charge.amount * 5 / 6,
			EndTime: charge.start.Add(time.Second), SettleTime: self.clock.Now()},
	}
	if charge.catalogId != "" {
		catalogId := charge.catalogId
		op.CatalogId = &catalogId
	}
	if charge.kind == model.ComputeChargeKindWebSearch {
		attempt := int16(1)
		op.Attempt = &attempt
	}
	if inserted, _, err := model.ComputeOpInsertSettled(self.env, self.t3.ctx, self.settings, op); err != nil || !inserted {
		self.t3.t.Fatalf("the charge's row: %v %v", inserted, err)
	}
	self.book()
	return opKey
}

// One booking run of the network's shard.
func (self *computeT3Spending) book() *controller.ComputeBookResult {
	self.t3.t.Helper()
	result, err := controller.ComputeBook(self.env, self.t3.ctx, self.settings, model.ComputeOpShard(self.settings, self.networkId))
	if err != nil {
		self.t3.t.Fatal(err)
	}
	return result
}

// Refunds amount of chargeId, as a dispute or `sidecarctl compute adjust --refund` does, and books it.
func (self *computeT3Spending) refund(chargeId server.Id, amount int64) {
	self.t3.t.Helper()
	if _, err := controller.ComputeRefundCharge(self.env, self.t3.ctx, self.settings, &controller.ComputeRefundChargeRequest{
		ChargeId: chargeId, AmountNanoUsd: amount,
	}); err != nil {
		self.t3.t.Fatal(err)
	}
	self.book()
}

// An open operation holding amount, counting to memberId: what an admitted call holds until it settles.
func (self *computeT3Spending) hold(amount int64, memberId *server.Id) {
	self.t3.t.Helper()
	catalogId := "chutes-qwen3.8-27b-tee-high"
	op := &model.ComputeOp{
		OpKey: server.NewId(), NetworkId: self.networkId, State: model.ComputeOpStateOpen, Kind: model.ComputeChargeKindInference,
		SubjectRef: server.NewId(), SidecarId: self.sidecarId, MemberId: memberId, CatalogId: &catalogId, Provider: 1,
		StartTime: self.clock.Now(), HoldNanoUsd: amount, ReserveNanoUsd: amount,
	}
	if inserted, _, err := model.ComputeOpInsertOpen(self.env, self.t3.ctx, self.settings, op); err != nil || !inserted {
		self.t3.t.Fatalf("the hold's row: %v %v", inserted, err)
	}
}

// A caller of the network: an untagged consented device of memberId with role, as the mount verifies one.
func (self *computeT3Spending) device(memberId server.Id, role bridge.MemberRole) *handler.ComputeApiCaller {
	sidecarId := operator.Id(server.NewId())
	member := operator.Id(memberId)
	return &handler.ComputeApiCaller{
		Credential: handler.ComputeApiCredentialSidecar, NetworkId: operator.Id(self.networkId), SidecarId: &sidecarId,
		MemberId: &member, Role: role, Consented: true,
	}
}

// What a route answers caller, through the frame, its body decoded into answer where it answers 200; the status.
func (self *computeT3Spending) serve(method string, path string, caller *handler.ComputeApiCaller, query url.Values, answer any) int {
	self.t3.t.Helper()
	for _, route := range handler.ComputeApiRoutes(nil) {
		if route.Method != method || route.Path != path {
			continue
		}
		served := route.Serve(self.t3.ctx, caller, query, nil)
		if served.Status == http.StatusOK && answer != nil {
			if err := json.Unmarshal(served.Body, answer); err != nil {
				self.t3.t.Fatalf("%s %s: %v", method, path, err)
			}
		}
		return served.Status
	}
	self.t3.t.Fatalf("no route %s %s", method, path)
	return 0
}

// The spend view caller reads by by.
func (self *computeT3Spending) view(caller *handler.ComputeApiCaller, by model.ComputeSpendBy) *handler.ComputeSpendAnswer {
	self.t3.t.Helper()
	answer := &handler.ComputeSpendAnswer{}
	query := url.Values{}
	if by != model.ComputeSpendByNone {
		query.Set("by", string(by))
	}
	if status := self.serve(http.MethodGet, "/compute/spend", caller, query, answer); status != http.StatusOK {
		self.t3.t.Fatalf("the spend view answered %d", status)
	}
	return answer
}

// A view's line of kind, nil where it has none.
func computeT3SpendLine(answer *handler.ComputeSpendAnswer, kind model.ComputeChargeKind) *handler.ComputeSpendLineBody {
	for _, line := range answer.Lines {
		if line.Kind == int16(kind) {
			return line
		}
	}
	return nil
}

// Every charge's page caller reads, from the first page to the last.
func (self *computeT3Spending) chargePages(caller *handler.ComputeApiCaller, limit int) []*handler.ComputeChargeBody {
	self.t3.t.Helper()
	charges := []*handler.ComputeChargeBody{}
	cursor := ""
	for range 100 {
		answer := &handler.ComputeChargesAnswer{}
		query := url.Values{"limit": {strconv.Itoa(limit)}}
		if cursor != "" {
			query.Set("cursor", cursor)
		}
		if status := self.serve(http.MethodGet, "/compute/charges", caller, query, answer); status != http.StatusOK {
			self.t3.t.Fatalf("the charges answered %d", status)
		}
		if limit < len(answer.Charges) {
			self.t3.t.Fatalf("a page of %d rows past its limit %d", len(answer.Charges), limit)
		}
		charges = append(charges, answer.Charges...)
		if answer.NextCursor == "" {
			return charges
		}
		cursor = answer.NextCursor
	}
	self.t3.t.Fatal("the pages never end")
	return nil
}

// The sum of the network's per-request charges of kind, which the view never reads: what it is checked against.
func (self *computeT3Spending) chargesSum(kind model.ComputeChargeKind) (amount int64, refunded int64, count int64) {
	self.t3.t.Helper()
	amount = self.t3.queryInt(`SELECT coalesce(sum(amount_nano_usd), 0)::bigint FROM compute_charge WHERE network_id = $1 AND kind = $2`, self.networkId, int16(kind))
	refunded = self.t3.queryInt(`SELECT coalesce(-sum(amount_nano_usd), 0)::bigint FROM compute_charge WHERE network_id = $1 AND kind = $2 AND amount_nano_usd < 0`, self.networkId, int16(kind))
	count = self.t3.queryInt(`SELECT count(*) FROM compute_charge WHERE network_id = $1 AND kind = $2 AND amount_nano_usd > 0`, self.networkId, int16(kind))
	return amount, refunded, count
}

// Each service's total is the sum of its charges, net of refunds with the refunds apart, and its days the same within
// the window: through a batch booked twice, a row delivered again after its booking, a refund retried, and a charge 10
// days back that counts to the total and to no day of the window.
func TestSpendTotalEqualsItsChargesNetOfRefunds(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		now := computeT3Midday(time.Now().UTC())
		spending := newComputeT3Spending(t3, now)
		agentId, memberA := spending.agentId, spending.memberA
		modelId := "chutes-qwen3.8-27b-tee-high"
		first := spending.settle(&computeT3Charge{kind: model.ComputeChargeKindInference, amount: computeT3Usd(1.25), start: now.Add(-time.Hour),
			agentId: &agentId, memberId: &memberA, catalogId: modelId, provider: 1})
		spending.settle(&computeT3Charge{kind: model.ComputeChargeKindInference, amount: computeT3Usd(0.75), start: now.Add(-48 * time.Hour),
			memberId: &memberA, catalogId: modelId, provider: 1})
		spending.settle(&computeT3Charge{kind: model.ComputeChargeKindInference, amount: computeT3Usd(2), start: now.Add(-10 * 24 * time.Hour),
			memberId: &memberA, catalogId: "engy-llama-5-70b-direct-medium", provider: 2})
		spending.settle(&computeT3Charge{kind: model.ComputeChargeKindWebSearch, amount: computeT3Usd(0.012), start: now.Add(-time.Hour).Truncate(time.Minute),
			agentId: &agentId, memberId: &memberA, catalogId: "search/parallel/fast", provider: 8})
		// a batch booked again, and the first call's row delivered again after its booking, add nothing
		if again := spending.book(); again.AppliedCount != 0 {
			t.Fatalf("a second booking applied %d", again.AppliedCount)
		}
		redelivered := &model.ComputeOp{
			OpKey: first, NetworkId: spending.networkId, State: model.ComputeOpStateSettled, Kind: model.ComputeChargeKindInference,
			SubjectRef: server.NewId(), SidecarId: spending.sidecarId, AgentId: &agentId, MemberId: &memberA, Provider: 1,
			StartTime: now.Add(-time.Hour), HoldNanoUsd: computeT3Usd(1.25), ReserveNanoUsd: computeT3Usd(1.25),
			Outcome: &model.ComputeOpOutcome{Units: model.ComputeUnits{InputTokens: 1000}, AmountNanoUsd: computeT3Usd(1.25), EndTime: now, SettleTime: now},
		}
		if _, _, err := model.ComputeOpInsertSettled(spending.env, t3.ctx, spending.settings, redelivered); err != nil {
			t.Fatal(err)
		}
		spending.book()
		// a refund of part of the first call, retried
		spending.refund(first, computeT3Usd(0.25))
		spending.refund(first, computeT3Usd(0.25))

		owner := spending.device(spending.memberA, bridge.MemberRole_MEMBER_ROLE_OWNER)
		for _, by := range []model.ComputeSpendBy{model.ComputeSpendByNone, model.ComputeSpendByProvider, model.ComputeSpendByModel, model.ComputeSpendByMember, model.ComputeSpendByAgent} {
			view := spending.view(owner, by)
			for _, kind := range []model.ComputeChargeKind{model.ComputeChargeKindInference, model.ComputeChargeKindWebSearch} {
				amount, refunded, count := spending.chargesSum(kind)
				line := computeT3SpendLine(view, kind)
				if line == nil || line.Total.AmountNanoUsd != amount || line.Total.RefundedNanoUsd != refunded || line.Total.Count != count {
					t.Errorf("by %q, %s's line %+v; its charges sum to %d, %d refunded, %d charges", by, kind, line, amount, refunded, count)
					continue
				}
				// the window's days: today's and two days back's, never the charge 10 days back
				windowSum := int64(0)
				for _, day := range line.Days {
					windowSum += day.AmountNanoUsd
				}
				windowWant := t3.queryInt(`SELECT coalesce(sum(amount_nano_usd), 0)::bigint FROM compute_charge WHERE network_id = $1 AND kind = $2 AND start_time >= $3`,
					spending.networkId, int16(kind), model.ComputeUtcDay(now).AddDate(0, 0, -6))
				if windowSum != windowWant {
					t.Errorf("by %q, %s's 7 days sum to %d, its charges in the window to %d", by, kind, windowSum, windowWant)
				}
				if by != model.ComputeSpendByNone {
					rowsSum := int64(0)
					for _, row := range line.Rows {
						if by.HasTotal() {
							rowsSum += row.Total.AmountNanoUsd
						} else {
							for _, day := range row.Days {
								rowsSum += day.AmountNanoUsd
							}
						}
					}
					if (by.HasTotal() && rowsSum != amount) || (!by.HasTotal() && rowsSum != windowWant) {
						t.Errorf("by %q, %s's rows sum to %d", by, kind, rowsSum)
					}
				}
			}
		}
		inference := computeT3SpendLine(spending.view(owner, model.ComputeSpendByNone), model.ComputeChargeKindInference)
		if inference.Total.AmountNanoUsd != computeT3Usd(3.75) || inference.Total.RefundedNanoUsd != computeT3Usd(0.25) || inference.Total.Count != 3 {
			t.Errorf("inference's total %+v", inference.Total)
		}
	})
}

// An Env whose every transaction runs fourteen hours ahead of UTC, where a minute before UTC midnight is already the next
// day, and in a date style that writes a date day first.
type computeT3StyleEnv struct {
	operator.Env
}

func (self *computeT3StyleEnv) Db() (operator.Db, error) {
	db, err := self.Env.Db()
	if err != nil {
		return nil, err
	}
	return &computeT3StyleDb{db: db}, nil
}

type computeT3StyleDb struct {
	db operator.Db
}

func (self *computeT3StyleDb) Db(ctx context.Context, callback func(conn operator.DbConn) error, options ...operator.DbOption) error {
	return self.db.Db(ctx, func(conn operator.DbConn) error {
		if _, err := conn.Exec(ctx, `SET TimeZone TO '`+computeT3Zone+`'`); err != nil {
			return err
		}
		if _, err := conn.Exec(ctx, `SET DateStyle TO 'SQL, DMY'`); err != nil {
			return err
		}
		defer conn.Exec(ctx, `RESET TimeZone`)
		defer conn.Exec(ctx, `RESET DateStyle`)
		return callback(conn)
	}, options...)
}

func (self *computeT3StyleDb) Tx(ctx context.Context, callback func(tx operator.DbTx) error, options ...operator.DbOption) error {
	return self.db.Tx(ctx, func(tx operator.DbTx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL TimeZone TO '`+computeT3Zone+`'`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SET LOCAL DateStyle TO 'SQL, DMY'`); err != nil {
			return err
		}
		return callback(tx)
	}, options...)
}

// The 7 days are UTC days, today so far and the 6 before, whatever the session's zone and date style: a charge thirty
// seconds before UTC midnight counts to yesterday, one thirty seconds after to today, though every session that reads
// them runs fourteen hours ahead of UTC and writes its dates day first.
func TestSpendSevenDaysAreUtcDays(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		now := computeT3Midday(time.Now().UTC())
		spending := newComputeT3Spending(t3, now)
		midnight := model.ComputeUtcDay(now)
		spending.settle(&computeT3Charge{kind: model.ComputeChargeKindInference, amount: computeT3Usd(1), start: midnight.Add(-30 * time.Second), provider: 1})
		spending.settle(&computeT3Charge{kind: model.ComputeChargeKindInference, amount: computeT3Usd(2), start: midnight.Add(30 * time.Second), provider: 1})
		coretest.SetGlobalEnv(t, &computeT3StyleEnv{Env: spending.env})
		owner := spending.device(spending.memberA, bridge.MemberRole_MEMBER_ROLE_OWNER)
		view := spending.view(owner, model.ComputeSpendByNone)
		want := []string{}
		for i := 6; 0 <= i; i -= 1 {
			want = append(want, midnight.AddDate(0, 0, -i).Format("2006-01-02"))
		}
		if !slices.Equal(view.Days, want) {
			t.Fatalf("the window %v, want %v", view.Days, want)
		}
		line := computeT3SpendLine(view, model.ComputeChargeKindInference)
		if line == nil || line.Days[5].AmountNanoUsd != computeT3Usd(1) || line.Days[6].AmountNanoUsd != computeT3Usd(2) {
			t.Fatalf("the days %+v", line)
		}
		if view.FirstChargeDay != midnight.AddDate(0, 0, -1).Format("2006-01-02") {
			t.Errorf("the first charge day %q", view.FirstChargeDay)
		}
		// the statement's rows and the charges' times read the same in any zone
		statement := &handler.ComputeStatementAnswer{}
		if status := spending.serve(http.MethodGet, "/compute/statement", owner, url.Values{"month": {midnight.AddDate(0, 0, -1).Format("2006-01")}}, statement); status != http.StatusOK {
			t.Fatalf("the statement answered %d", status)
		}
		for _, row := range statement.Rows {
			if row.Day != midnight.AddDate(0, 0, -1).Format("2006-01-02") && row.Day != midnight.Format("2006-01-02") {
				t.Errorf("a statement row of %q", row.Day)
			}
		}
	})
}

// Charges settle out of start order, so the first charge day is the earliest day booked: today's, then three days back
// once a late charge books, and the view's since says so.
func TestSpendFirstChargeDaySetByAnEarlierSettlingCharge(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		now := computeT3Midday(time.Now().UTC())
		spending := newComputeT3Spending(t3, now)
		owner := spending.device(spending.memberA, bridge.MemberRole_MEMBER_ROLE_OWNER)
		if view := spending.view(owner, model.ComputeSpendByNone); view.FirstChargeDay != "" || len(view.Lines) != 0 {
			t.Fatalf("before any charge: %+v", view)
		}
		spending.settle(&computeT3Charge{kind: model.ComputeChargeKindInference, amount: computeT3Usd(0.5), start: now.Add(-time.Hour), provider: 1})
		today := model.ComputeUtcDay(now)
		if view := spending.view(owner, model.ComputeSpendByNone); view.FirstChargeDay != today.Format("2006-01-02") {
			t.Fatalf("after today's charge: %q", view.FirstChargeDay)
		}
		spending.settle(&computeT3Charge{kind: model.ComputeChargeKindInference, amount: computeT3Usd(0.5), start: today.AddDate(0, 0, -3).Add(5 * time.Hour), provider: 1})
		view := spending.view(owner, model.ComputeSpendByNone)
		if view.FirstChargeDay != today.AddDate(0, 0, -3).Format("2006-01-02") {
			t.Fatalf("after the late charge: %q", view.FirstChargeDay)
		}
		if line := computeT3SpendLine(view, model.ComputeChargeKindInference); line == nil || line.Days[3].AmountNanoUsd != computeT3Usd(0.5) || line.Days[6].AmountNanoUsd != computeT3Usd(0.5) {
			t.Errorf("the days %+v", line)
		}
		spending.settle(&computeT3Charge{kind: model.ComputeChargeKindInference, amount: computeT3Usd(0.5), start: now.Add(-time.Minute), provider: 1})
		if view := spending.view(owner, model.ComputeSpendByNone); view.FirstChargeDay != today.AddDate(0, 0, -3).Format("2006-01-02") {
			t.Errorf("after another of today's: %q", view.FirstChargeDay)
		}
	})
}

// A tagged sidecar's charges count to the sponsor the broker wrote at each hold: before its SponsorSet to member A, after
// it to member B. Each Member's spend view and charges hold their own alone, a role that sees the network both, by
// member one row each; and compute_rollup_day over the closed day finds the rollups equal to the charges.
func TestSpendAfterSponsorSetCountsEachSponsorsOwn(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		now := computeT3Midday(time.Now().UTC())
		spending := newComputeT3Spending(t3, now)
		tagged := server.NewId()
		memberA, memberB := spending.memberA, spending.memberB
		yesterday := model.ComputeUtcDay(now).AddDate(0, 0, -1)
		before := spending.settle(&computeT3Charge{kind: model.ComputeChargeKindInference, amount: computeT3Usd(1), start: yesterday.Add(9 * time.Hour),
			sidecarId: &tagged, memberId: &memberA, provider: 1})
		// the SponsorSet: the projection now names B, and the broker writes B on the next hold
		after := spending.settle(&computeT3Charge{kind: model.ComputeChargeKindInference, amount: computeT3Usd(3), start: yesterday.Add(15 * time.Hour),
			sidecarId: &tagged, memberId: &memberB, provider: 1})

		viewerA := spending.device(memberA, bridge.MemberRole_MEMBER_ROLE_MEMBER)
		viewerB := spending.device(memberB, bridge.MemberRole_MEMBER_ROLE_MEMBER)
		for _, own := range []struct {
			viewer *handler.ComputeApiCaller
			amount int64
			charge server.Id
		}{{viewer: viewerA, amount: computeT3Usd(1), charge: before}, {viewer: viewerB, amount: computeT3Usd(3), charge: after}} {
			line := computeT3SpendLine(spending.view(own.viewer, model.ComputeSpendByNone), model.ComputeChargeKindInference)
			if line == nil || line.Total.AmountNanoUsd != own.amount || line.Days[5].AmountNanoUsd != own.amount {
				t.Errorf("member %s's view %+v, want %d", *own.viewer.MemberId, line, own.amount)
			}
			charges := spending.chargePages(own.viewer, 10)
			if len(charges) != 1 || charges[0].ChargeId != operator.Id(own.charge) || charges[0].MemberId == nil || *charges[0].MemberId != *own.viewer.MemberId {
				t.Errorf("member %s's charges %+v", *own.viewer.MemberId, charges)
			}
		}
		admin := spending.device(memberA, bridge.MemberRole_MEMBER_ROLE_ADMIN)
		byMember := computeT3SpendLine(spending.view(admin, model.ComputeSpendByMember), model.ComputeChargeKindInference)
		if byMember == nil || len(byMember.Rows) != 2 || byMember.Total.AmountNanoUsd != computeT3Usd(4) {
			t.Fatalf("the network's view by member %+v", byMember)
		}
		for _, row := range byMember.Rows {
			want := computeT3Usd(1)
			if row.MemberId != nil && *row.MemberId == operator.Id(memberB) {
				want = computeT3Usd(3)
			}
			if row.Total.AmountNanoUsd != want {
				t.Errorf("member %v's row %+v", row.MemberId, row.Total)
			}
		}
		if charges := spending.chargePages(admin, 1); len(charges) != 2 {
			t.Errorf("the network's charges, a row a page: %d", len(charges))
		}
		result, err := controller.ComputeRollupDay(spending.env, t3.ctx, spending.settings, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Mismatches) != 0 {
			t.Errorf("compute_rollup_day found %+v", result.Mismatches)
		}
	})
}

// A Member's view holds what counts to them alone, its held only the holds of their own; the network's view holds
// every member's, and its held every open hold.
func TestSpendMemberSeesOwnAndHoldsAreItsOwn(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		now := computeT3Midday(time.Now().UTC())
		spending := newComputeT3Spending(t3, now)
		memberA, memberB := spending.memberA, spending.memberB
		spending.settle(&computeT3Charge{kind: model.ComputeChargeKindInference, amount: computeT3Usd(1), start: now.Add(-time.Hour), memberId: &memberA, provider: 1})
		spending.settle(&computeT3Charge{kind: model.ComputeChargeKindInference, amount: computeT3Usd(2), start: now.Add(-time.Hour), memberId: &memberB, provider: 1})
		spending.settle(&computeT3Charge{kind: model.ComputeChargeKindWebSearch, amount: computeT3Usd(0.012), start: now.Add(-time.Hour).Truncate(time.Minute), memberId: &memberB, provider: 8})
		spending.hold(computeT3Usd(2.58), &memberA)
		spending.hold(computeT3Usd(0.5), &memberB)
		spending.hold(computeT3Usd(0.25), nil)

		viewA := spending.view(spending.device(memberA, bridge.MemberRole_MEMBER_ROLE_MEMBER), model.ComputeSpendByMember)
		if len(viewA.Lines) != 1 || viewA.Lines[0].Total.AmountNanoUsd != computeT3Usd(1) || viewA.HeldNanoUsd != computeT3Usd(2.58) {
			t.Errorf("member A's view: %d lines, held %d", len(viewA.Lines), viewA.HeldNanoUsd)
		}
		for _, line := range viewA.Lines {
			for _, row := range line.Rows {
				if row.MemberId == nil || *row.MemberId != operator.Id(memberA) {
					t.Errorf("member A's view holds a row of %v", row.MemberId)
				}
			}
		}
		owner := spending.view(spending.device(memberA, bridge.MemberRole_MEMBER_ROLE_OWNER), model.ComputeSpendByNone)
		if len(owner.Lines) != 2 || owner.HeldNanoUsd != computeT3Usd(2.58)+computeT3Usd(0.5)+computeT3Usd(0.25) {
			t.Errorf("the owner's view: %d lines, held %d", len(owner.Lines), owner.HeldNanoUsd)
		}
		if line := computeT3SpendLine(owner, model.ComputeChargeKindInference); line == nil || line.Total.AmountNanoUsd != computeT3Usd(3) {
			t.Errorf("the owner's inference %+v", line)
		}
	})
}

// An Owner, a Billing admin, an Admin, a Network admin, an IT admin and an Auditor see the network's spend view and
// charges, every member's; a Member only theirs.
func TestSpendOwnerNetworkAdminAndItAdminSeeTheNetwork(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		now := computeT3Midday(time.Now().UTC())
		spending := newComputeT3Spending(t3, now)
		memberA, memberB := spending.memberA, spending.memberB
		spending.settle(&computeT3Charge{kind: model.ComputeChargeKindInference, amount: computeT3Usd(1), start: now.Add(-time.Hour), memberId: &memberA, provider: 1})
		spending.settle(&computeT3Charge{kind: model.ComputeChargeKindInference, amount: computeT3Usd(2), start: now.Add(-time.Hour), memberId: &memberB, provider: 1})
		for _, role := range []bridge.MemberRole{bridge.MemberRole_MEMBER_ROLE_OWNER, bridge.MemberRole_MEMBER_ROLE_BILLING_ADMIN,
			bridge.MemberRole_MEMBER_ROLE_ADMIN, bridge.MemberRole_MEMBER_ROLE_NETWORK_ADMIN, bridge.MemberRole_MEMBER_ROLE_IT_ADMIN,
			bridge.MemberRole_MEMBER_ROLE_AUDITOR} {
			viewer := spending.device(memberA, role)
			line := computeT3SpendLine(spending.view(viewer, model.ComputeSpendByNone), model.ComputeChargeKindInference)
			if line == nil || line.Total.AmountNanoUsd != computeT3Usd(3) || line.Total.Count != 2 {
				t.Errorf("role %v's view %+v", role, line)
			}
			if charges := spending.chargePages(viewer, 100); len(charges) != 2 {
				t.Errorf("role %v sees %d charges", role, len(charges))
			}
		}
		member := spending.device(memberA, bridge.MemberRole_MEMBER_ROLE_MEMBER)
		if line := computeT3SpendLine(spending.view(member, model.ComputeSpendByNone), model.ComputeChargeKindInference); line == nil || line.Total.AmountNanoUsd != computeT3Usd(1) {
			t.Errorf("a Member's view %+v", line)
		}
	})
}

// The spend view, from the private stack's figures, is refused to a tagged sidecar, an enclave, an ephemeral and a
// Delegation-admitted sidecar and the network's credential, whatever role stands behind each, 403
// consented_device_required; the person's consented device of the same role reads it.
func TestSpendRefusedToTaggedEnclaveEphemeralAndDelegation(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		now := computeT3Midday(time.Now().UTC())
		spending := newComputeT3Spending(t3, now)
		memberA := spending.memberA
		spending.settle(&computeT3Charge{kind: model.ComputeChargeKindInference, amount: computeT3Usd(1), start: now.Add(-time.Hour), memberId: &memberA, provider: 1})
		for _, label := range []string{"tagged", "enclave", "ephemeral", "delegation", "network credential"} {
			caller := spending.device(memberA, bridge.MemberRole_MEMBER_ROLE_OWNER)
			switch label {
			case "tagged":
				caller.Tagged, caller.MemberId, caller.Role = true, nil, bridge.MemberRole_MEMBER_ROLE_UNSPECIFIED
			case "enclave":
				caller.Enclave = true
			case "ephemeral":
				caller.Ephemeral = true
			case "delegation":
				caller.DelegationAdmitted = true
			case "network credential":
				caller.Credential, caller.SidecarId = handler.ComputeApiCredentialNetwork, nil
			}
			if status := spending.serve(http.MethodGet, "/compute/spend", caller, url.Values{}, nil); status != http.StatusForbidden {
				t.Errorf("%s answers %d", label, status)
			}
		}
		if line := computeT3SpendLine(spending.view(spending.device(memberA, bridge.MemberRole_MEMBER_ROLE_OWNER), model.ComputeSpendByNone), model.ComputeChargeKindInference); line == nil {
			t.Error("the person's consented device reads no line")
		}
	})
}

// The balance tells each caller whether they may buy: an Owner and a Billing admin from a device or the network's
// credential may, an Admin, an Auditor, a Member and a tagged sidecar may not; and whether sales are open, closed until a
// service the network may use can spend credit. Its figures are the ledger's: available less the open holds.
func TestBalanceSaysWhoMayBuyAndWhetherSalesAreOpen(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		computeT3SpendableOnce.Do(func() {
			controller.RegisterComputeSpendableService("computet3_test_spendable", computeT3SpendableSale)
		})
		now := computeT3Midday(time.Now().UTC())
		spending := newComputeT3Spending(t3, now)
		spending.settle(&computeT3Charge{kind: model.ComputeChargeKindInference, amount: computeT3Usd(10), start: now.Add(-time.Hour), provider: 1})
		spending.hold(computeT3Usd(2.5), nil)
		memberA := spending.memberA
		network := spending.device(memberA, bridge.MemberRole_MEMBER_ROLE_OWNER)
		network.Credential, network.SidecarId = handler.ComputeApiCredentialNetwork, nil
		tagged := spending.device(memberA, bridge.MemberRole_MEMBER_ROLE_UNSPECIFIED)
		tagged.Tagged, tagged.MemberId = true, nil
		callers := []struct {
			label  string
			caller *handler.ComputeApiCaller
			mayBuy bool
		}{
			{label: "owner", caller: spending.device(memberA, bridge.MemberRole_MEMBER_ROLE_OWNER), mayBuy: true},
			{label: "billing admin", caller: spending.device(memberA, bridge.MemberRole_MEMBER_ROLE_BILLING_ADMIN), mayBuy: true},
			{label: "network credential", caller: network, mayBuy: true},
			{label: "admin", caller: spending.device(memberA, bridge.MemberRole_MEMBER_ROLE_ADMIN)},
			{label: "auditor", caller: spending.device(memberA, bridge.MemberRole_MEMBER_ROLE_AUDITOR)},
			{label: "member", caller: spending.device(memberA, bridge.MemberRole_MEMBER_ROLE_MEMBER)},
			{label: "tagged sidecar", caller: tagged},
		}
		for _, open := range []bool{false, true} {
			if open {
				computeT3SpendableSale.open(spending.networkId)
			}
			for _, c := range callers {
				balance := &handler.ComputeBalanceAnswer{}
				if status := spending.serve(http.MethodGet, "/compute/balance", c.caller, nil, balance); status != http.StatusOK {
					t.Fatalf("%s's balance answered %d", c.label, status)
				}
				if balance.MayBuy != c.mayBuy || balance.SalesOpen != open {
					t.Errorf("%s with sales open %t: may buy %t, sales open %t", c.label, open, balance.MayBuy, balance.SalesOpen)
				}
				if balance.BalanceNanoUsd != computeT3Usd(90) || balance.HeldNanoUsd != computeT3Usd(2.5) || balance.AvailableNanoUsd != computeT3Usd(87.5) ||
					balance.State != "active" || balance.Version <= 0 || balance.UpdateTimeMs <= 0 || balance.AutoRenew == nil {
					t.Errorf("%s's figures %+v", c.label, balance)
				}
			}
		}
	})
}

// A month's statement holds its day rows and the lots made in it, and a promotion made the month before whose expiry
// fell in it, with what of it lapsed; a lot of another month is not in it. A Member's statement holds their rows and no
// lot.
func TestStatementShowsLotsAndLapses(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		february := time.Date(2027, time.February, 20, 12, 0, 0, 0, time.UTC)
		spending := newComputeT3Spending(t3, february)
		lapseAt := time.Date(2027, time.March, 5, 12, 0, 0, 0, time.UTC)
		lapsing := spending.credit(&controller.ComputeCreditRequest{
			NetworkId: spending.networkId, Kind: model.ComputeLotKindPromotion, Market: model.ComputeMarketPromotion,
			MarketRef: server.NewId().String(), AmountNanoUsd: computeT3Usd(5), ExpireTime: &lapseAt,
		})
		spending.clock.AdvanceTo(time.Date(2027, time.February, 25, 12, 0, 0, 0, time.UTC))
		februaryPurchase := spending.purchase(spending.networkId, computeT3Usd(10), nil)
		spending.clock.AdvanceTo(time.Date(2027, time.March, 10, 12, 0, 0, 0, time.UTC))
		if _, err := controller.ComputeLapsePromos(spending.env, t3.ctx, spending.settings); err != nil {
			t.Fatal(err)
		}
		marchPurchase := spending.purchase(spending.networkId, computeT3Usd(25), nil)
		marchExpire := time.Date(2027, time.April, 11, 12, 0, 0, 0, time.UTC)
		marchPromotion := spending.credit(&controller.ComputeCreditRequest{
			NetworkId: spending.networkId, Kind: model.ComputeLotKindPromotion, Market: model.ComputeMarketPromotion,
			MarketRef: server.NewId().String(), AmountNanoUsd: computeT3Usd(2), ExpireTime: &marchExpire,
		})
		memberA := spending.memberA
		spending.settle(&computeT3Charge{kind: model.ComputeChargeKindInference, amount: computeT3Usd(0.5),
			start: time.Date(2027, time.March, 9, 8, 0, 0, 0, time.UTC), memberId: &memberA, provider: 1})

		owner := spending.device(memberA, bridge.MemberRole_MEMBER_ROLE_OWNER)
		statement := &handler.ComputeStatementAnswer{}
		if status := spending.serve(http.MethodGet, "/compute/statement", owner, url.Values{"month": {"2027-03"}}, statement); status != http.StatusOK {
			t.Fatalf("March's statement answered %d", status)
		}
		lotIds := []operator.Id{}
		for _, lot := range statement.Lots {
			lotIds = append(lotIds, lot.LotId)
			if lot.LotId == operator.Id(lapsing.Lot.LotId) && (lot.LapsedNanoUsd == nil || *lot.LapsedNanoUsd != computeT3Usd(5) || lot.ExpireTimeMs == nil) {
				t.Errorf("the lapsed promotion %+v", lot)
			}
		}
		for _, want := range []server.Id{lapsing.Lot.LotId, marchPurchase.Lot.LotId, marchPromotion.Lot.LotId} {
			if !slices.Contains(lotIds, operator.Id(want)) {
				t.Errorf("March's statement lacks lot %s", want)
			}
		}
		if slices.Contains(lotIds, operator.Id(februaryPurchase.Lot.LotId)) || len(statement.Lots) != 3 {
			t.Errorf("March's statement holds %d lots, February's purchase among them: %t", len(statement.Lots), slices.Contains(lotIds, operator.Id(februaryPurchase.Lot.LotId)))
		}
		if statement.Month != "2027-03" || len(statement.Rows) != 1 || statement.Rows[0].Day != "2027-03-09" || statement.Rows[0].AmountNanoUsd != computeT3Usd(0.5) {
			t.Errorf("March's rows %+v", statement.Rows)
		}
		february2 := &handler.ComputeStatementAnswer{}
		if status := spending.serve(http.MethodGet, "/compute/statement", owner, url.Values{"month": {"2027-02"}}, february2); status != http.StatusOK || len(february2.Rows) != 0 {
			t.Fatalf("February's statement answered %d with %d rows", status, len(february2.Rows))
		}
		// February's: the first purchase, the lapsing promotion and the second purchase, all made in it
		if len(february2.Lots) != 3 {
			t.Errorf("February's statement holds %d lots", len(february2.Lots))
		}
		mine := &handler.ComputeStatementAnswer{}
		if status := spending.serve(http.MethodGet, "/compute/statement", spending.device(memberA, bridge.MemberRole_MEMBER_ROLE_MEMBER), url.Values{"month": {"2027-03"}}, mine); status != http.StatusOK {
			t.Fatalf("a Member's statement answered %d", status)
		}
		if len(mine.Rows) != 1 || len(mine.Lots) != 0 {
			t.Errorf("a Member's statement: %d rows, %d lots", len(mine.Rows), len(mine.Lots))
		}
	})
}
