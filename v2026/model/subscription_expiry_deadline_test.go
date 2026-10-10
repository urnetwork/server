// Native deadline observations use the same bounded rows as financial expiry.
package model

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Both streams contain protected partial reports. The final row lies outside
// the retained epoch and has an earlier deadline than every in-pass row.
type expiryDeadlineFixture struct {
	now             time.Time
	ids             []server.Id
	firstExpiration time.Time
	lastExpiration  time.Time
}

func newExpiryDeadlineFixture(t testing.TB, ctx context.Context) expiryDeadlineFixture {
	t.Helper()
	networkId, sourceId, destinationId := server.NewId(), server.NewId(), server.NewId()
	addContractPayoutTestClients(ctx, map[server.Id]server.Id{sourceId: networkId, destinationId: networkId})
	now := server.NowUtc().Truncate(time.Microsecond)
	fixture := expiryDeadlineFixture{now: now, firstExpiration: now.Add(40 * time.Minute), lastExpiration: now.Add(20 * time.Minute)}
	for _, row := range []struct {
		created  time.Time
		expires  time.Time
		disputed bool
		pending  bool
	}{
		{created: now.Add(-50 * time.Minute), expires: now.Add(45 * time.Minute)},
		{created: now.Add(-50 * time.Minute), expires: fixture.firstExpiration, disputed: true},
		{created: now.Add(-30 * time.Minute)},
		{created: now.Add(-20 * time.Minute), expires: fixture.lastExpiration, disputed: true, pending: true},
		{created: now.Add(time.Minute), expires: now.Add(10 * time.Minute)},
	} {
		id, err := CreateContractNoEscrow(ctx, networkId, sourceId, networkId, destinationId, 100)
		server.Raise(err)
		server.Raise(CloseContract(ctx, id, sourceId, 17, true))
		var expiration *time.Time
		if !row.expires.IsZero() {
			expiration = &row.expires
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET create_time=$2,expiration_time=$3,dispute=$4 WHERE contract_id=$1`,
				id, row.created, expiration, row.disputed))
			if row.pending {
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO legacy_settlement_intent(contract_id,shard,outcome)
					VALUES($1,get_byte(uuid_send($1::uuid),15)%16,'settled')`, id))
			}
		}, server.TxReadCommitted, server.OptNoRetry())
		fixture.ids = append(fixture.ids, id)
	}
	return fixture
}

// Future hints cannot cross LIMIT or a captured creation epoch. The NULL
// deadline remains creation+60 minutes, including a fresh partial report.
func TestExpiryDeadlineRawPagesKeepBothBoundsAndNullFallback(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		fixture := newExpiryDeadlineFixture(t, ctx)
		before := map[server.Id][]byte{}
		for _, id := range fixture.ids {
			before[id] = readRedisExpiryRepairTestState(ctx, id)
		}
		readIntent := func() (snapshot string) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT to_jsonb(i)::text FROM legacy_settlement_intent i WHERE contract_id=$1`, fixture.ids[3]).Scan(&snapshot))
			})
			return
		}
		intentBefore := readIntent()
		cursor := &ContractExpiryCursor{ScanBefore: fixture.now}
		count, next, deadline, err := forceCloseOpenContractIdsPage(ctx, fixture.now.Add(-12*time.Minute), 1, 1, 1, 0, cursor)
		if err != nil || count != 0 || next == nil || deadline == nil || !deadline.Equal(fixture.firstExpiration) ||
			next.Open == nil || next.Open.ContractId != fixture.ids[0] || next.Dispute == nil || next.Dispute.ContractId != fixture.ids[1] {
			t.Fatal("first deadline observation escaped its two bounded raw rows", count, err)
		}
		count, next, deadline, err = forceCloseOpenContractIdsPage(ctx, fixture.now.Add(-12*time.Minute), 1, 1, 1, 0, next)
		if err != nil || count != 0 || next == nil || deadline == nil || !deadline.Equal(fixture.lastExpiration) ||
			next.Open == nil || next.Open.ContractId != fixture.ids[2] || next.Dispute == nil || next.Dispute.ContractId != fixture.ids[3] {
			t.Fatal("second page lost a live or intent-owned deadline", count, err)
		}
		count, next, deadline, err = forceCloseOpenContractIdsPage(ctx, fixture.now.Add(-12*time.Minute), 1, 1, 1, 0, next)
		if err != nil || count != 0 || next != nil || deadline != nil {
			t.Fatal("fixed-pass EOF consumed a post-epoch deadline", count, err)
		}
		// Isolate the NULL row from the disputed stream without another query
		// or unbounded selector. Its exact fallback is observable on this page.
		position := &ContractExpiryCursor{ScanBefore: fixture.now, DisputeDone: true,
			Open: &ContractExpiryPosition{CreateTime: fixture.now.Add(-50 * time.Minute), ContractId: fixture.ids[0]}}
		_, _, deadline, err = forceCloseOpenContractIdsPage(ctx, fixture.now.Add(-12*time.Minute), 1, 1, 1, 0, position)
		if err != nil || deadline == nil || !deadline.Equal(fixture.now.Add(30*time.Minute)) {
			t.Fatal("missing expiration did not retain its exact 60-minute fallback", err)
		}
		for _, id := range fixture.ids {
			if !bytes.Equal(before[id], readRedisExpiryRepairTestState(ctx, id)) {
				t.Fatal("deadline observation changed reports, financial rows or intent custody")
			}
		}
		if readIntent() != intentBefore {
			t.Fatal("deadline observation changed an existing settlement owner's arguments")
		}
	})
}

// An expired row that already has a settlement owner cannot contribute a past
// hint on every new scan, which would turn the recurring sweep into a hot loop.
func TestExpiryDeadlineExpiredOwnedRowsDoNotRepeatImmediateWake(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		fixture := newExpiryDeadlineFixture(t, ctx)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET expiration_time=$2 WHERE contract_id=ANY($1::uuid[])`,
				fixture.ids, fixture.now.Add(-time.Minute)))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO legacy_settlement_intent(contract_id,shard,outcome)
				SELECT id,get_byte(uuid_send(id),15)%16,'settled' FROM unnest($1::uuid[]) row(id)
				ON CONFLICT(contract_id) DO NOTHING`, fixture.ids))
		})
		count, next, deadline, err := forceCloseOpenContractIdsPage(ctx, fixture.now.Add(-12*time.Minute), 32, 1, 1, 0,
			&ContractExpiryCursor{ScanBefore: fixture.now})
		if err != nil || count != 0 || next != nil || deadline != nil {
			t.Fatal("expired intent-owned rows repeatedly requested an immediate sweep", count, err)
		}
	})
}

// Minimum observations survive JSON persistence and every fair lane. The same
// answer must be retained when several raw subpages run in one task budget.
func TestExpiryDeadlineScheduledPagesKeepMinimumAcrossContinuation(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		fixture := newExpiryDeadlineFixture(t, ctx)
		var state struct {
			Cursor     *ContractExpirySweepCursor `json:"cursor"`
			Expiration *time.Time                 `json:"expiration"`
		}
		finished := false
		for turn := range 64 {
			prior, err := json.Marshal(state)
			server.Raise(err)
			count, next, expiration, err := ForceCloseOpenContractIdsScheduledPage(ctx, fixture.now.Add(-12*time.Minute), 1, 1, 1, 0, state.Cursor, state.Expiration)
			unchanged, marshalErr := json.Marshal(state)
			if err != nil || marshalErr != nil || count != 0 || !bytes.Equal(prior, unchanged) {
				t.Fatal("completed deadline scan mutated its durable input or live contracts", err)
			}
			if state.Expiration != nil && (expiration == nil || expiration.After(*state.Expiration)) {
				t.Fatal("continuation forgot an earlier raw-page deadline", turn)
			}
			state.Cursor, state.Expiration = next, expiration
			raw, err := json.Marshal(state)
			server.Raise(err)
			server.Raise(json.Unmarshal(raw, &state))
			if next == nil {
				finished = true
				break
			}
		}
		if !finished || state.Expiration == nil || !state.Expiration.Equal(fixture.lastExpiration) {
			t.Fatal("finite fair pass lost its earliest observed expiration")
		}
		count, next, expiration, err := ForceCloseOpenContractIdsScheduledPage(ctx, fixture.now.Add(-12*time.Minute), 4096, 1, 1, 0, nil, nil)
		if err != nil || count != 0 || next != nil || expiration == nil || !expiration.Equal(fixture.lastExpiration) {
			t.Fatal("multi-subpage task retained only its final raw-page observation", err)
		}
	})
}

// A canceled or invalid model invocation cannot replace prior durable hints
// with observations from an incomplete page or alter the caller's timestamp.
func TestExpiryDeadlineInterruptedPageKeepsPriorHint(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		fixture := newExpiryDeadlineFixture(t, t.Context())
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		prior := fixture.now.Add(3 * time.Hour)
		cursor := &ContractExpirySweepCursor{Historical: &ContractExpiryCursor{ScanBefore: fixture.now}}
		count, next, deadline, err := ForceCloseOpenContractIdsScheduledPage(ctx, fixture.now.Add(-12*time.Minute), 1, 1, 1, 0, cursor, &prior)
		if err == nil || count != 0 || next != cursor || deadline != &prior || !prior.Equal(fixture.now.Add(3*time.Hour)) {
			t.Fatal("canceled page published a new cursor or deadline", err)
		}
	})
}
