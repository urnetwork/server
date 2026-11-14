//go:build sidecar_pending

package model

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// What a §21 action reaches inside one network (abuse_network_subjects.go), on
// MG0's to MG5's tables.

// A sidecar of the network with one bound client.
func testAbuseSidecar(t testing.TB, ctx context.Context, networkId server.Id, clientId server.Id) server.Id {
	t.Helper()
	sidecarId := server.NewId()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `
			INSERT INTO sidecar_identity (sidecar_id, network_id, identity_public_key, identity_alg, status, create_time)
			VALUES ($1, $2, $3, 'ed25519', 'active', $4)
		`, sidecarId, networkId, make([]byte, 32), server.NowUtc()))
		server.RaisePgResult(tx.Exec(ctx, `
			INSERT INTO sidecar_identity_binding (sidecar_id, generation, client_id, client_public_key, signatures, create_time)
			VALUES ($1, 1, $2, $3, $4, $5)
		`, sidecarId, clientId, make([]byte, 32), []byte{1}, server.NowUtc()))
	})
	return sidecarId
}

// Every subject of the network, read a page of pageSize at a time.
func testAbuseSubjects(ctx context.Context, networkId server.Id, pageSize int) (subjectIds []server.Id, pageCount int) {
	after := server.Id{}
	for {
		page, more := AbuseNetworkSubjects(ctx, networkId, after, pageSize)
		pageCount += 1
		subjectIds = append(subjectIds, page...)
		if !more || len(page) == 0 {
			return
		}
		after = page[len(page)-1]
	}
}

// A network's subjects are its sidecars and the clients that are neither bound
// to a sidecar nor acting for another subject, each once, in id order, whatever
// the page size; another network's are not among them. Closing each subject's
// contracts reaches every client of the network.
func TestAbuseNetworkSubjectsCoverEveryClientOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		otherNetworkId := server.NewId()
		sidecarClient, windowClient, deviceClient, deviceChild, otherClient := server.NewId(), server.NewId(), server.NewId(), server.NewId(), server.NewId()
		for _, clientId := range []server.Id{sidecarClient, windowClient, deviceClient, deviceChild} {
			Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")
		}
		Testing_CreateDevice(ctx, otherNetworkId, server.NewId(), otherClient, "", "")
		sidecarId := testAbuseSidecar(t, ctx, networkId, sidecarClient)
		otherSidecarId := testAbuseSidecar(t, ctx, otherNetworkId, otherClient)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET acl_subject_client_id = $2 WHERE client_id = $1`, windowClient, sidecarClient))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET acl_subject_client_id = $2 WHERE client_id = $1`, deviceChild, deviceClient))
		})

		want := []server.Id{sidecarId, deviceClient}
		slices.SortFunc(want, func(a server.Id, b server.Id) int { return a.Cmp(b) })
		for _, pageSize := range []int{1, 2, 100} {
			subjectIds, pageCount := testAbuseSubjects(ctx, networkId, pageSize)
			if !slices.Equal(subjectIds, want) {
				t.Fatalf("page size %d: the subjects are %v, want %v", pageSize, subjectIds, want)
			}
			if pageSize == 1 && pageCount != 2 {
				t.Fatalf("page size 1 took %d reads", pageCount)
			}
		}
		if slices.Contains(want, otherSidecarId) {
			t.Fatal("another network's sidecar is a subject")
		}
		if subjectIds, more := AbuseNetworkSubjects(ctx, networkId, server.Id{}, 0); len(subjectIds) != 0 || more {
			t.Fatalf("a read of no rows answered %v, %t", subjectIds, more)
		}

		// each client of the network is covered by exactly one subject
		covered := map[server.Id]int{}
		for _, subjectId := range want {
			for _, clientId := range aclSubjectClientIds(ctx, networkId, subjectId) {
				covered[clientId] += 1
			}
		}
		for _, clientId := range []server.Id{sidecarClient, windowClient, deviceClient, deviceChild} {
			if covered[clientId] != 1 {
				t.Errorf("client %s is covered %d times", clientId, covered[clientId])
			}
		}
		if 0 < covered[otherClient] {
			t.Error("another network's client is covered")
		}

		// a suspension's closes, subject by subject, stop every open contract of the network
		counterparty := server.NewId()
		Testing_CreateDevice(ctx, otherNetworkId, server.NewId(), counterparty, "", "")
		for _, clientId := range []server.Id{sidecarClient, windowClient, deviceClient, deviceChild} {
			if _, err := CreateContractNoEscrow(ctx, networkId, clientId, otherNetworkId, counterparty, 1024); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := CreateContractNoEscrow(ctx, otherNetworkId, otherClient, otherNetworkId, counterparty, 1024); err != nil {
			t.Fatal(err)
		}
		for _, subjectId := range want {
			if _, err := CloseAclSubjectContracts(ctx, networkId, subjectId); err != nil {
				t.Fatal(err)
			}
		}
		for _, clientId := range []server.Id{sidecarClient, windowClient, deviceClient, deviceChild} {
			if HasOpenContractHole(ctx, clientId, counterparty) {
				t.Errorf("client %s still forwards", clientId)
			}
		}
		if !HasOpenContractHole(ctx, otherClient, counterparty) {
			t.Error("another network's contract stopped")
		}
	})
}

// A standing change hints the network's active sidecar clients, which hold
// strict_exp tokens, and no device client, no inactive client and no other
// network's.
func TestAbuseNetworkHintClientIds(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		otherNetworkId := server.NewId()
		sidecarClient, secondSidecarClient, deviceClient, inactiveClient, otherClient := server.NewId(), server.NewId(), server.NewId(), server.NewId(), server.NewId()
		for _, clientId := range []server.Id{sidecarClient, secondSidecarClient, deviceClient, inactiveClient} {
			Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")
		}
		Testing_CreateDevice(ctx, otherNetworkId, server.NewId(), otherClient, "", "")
		server.Tx(ctx, func(tx server.PgTx) {
			for _, clientId := range []server.Id{sidecarClient, secondSidecarClient, inactiveClient, otherClient} {
				server.RaisePgResult(tx.Exec(ctx, `
					INSERT INTO network_client_owner (client_id, strict_exp) VALUES ($1, true)
					ON CONFLICT (client_id) DO UPDATE SET strict_exp = true
				`, clientId))
			}
			server.RaisePgResult(tx.Exec(ctx, `
				INSERT INTO network_client_owner (client_id, strict_exp) VALUES ($1, false)
				ON CONFLICT (client_id) DO UPDATE SET strict_exp = false
			`, deviceClient))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET active = false WHERE client_id = $1`, inactiveClient))
		})
		want := []server.Id{sidecarClient, secondSidecarClient}
		slices.SortFunc(want, func(a server.Id, b server.Id) int { return a.Cmp(b) })
		if clientIds := AbuseNetworkHintClientIds(ctx, networkId, 100); !slices.Equal(clientIds, want) {
			t.Fatalf("the hinted clients are %v, want %v", clientIds, want)
		}
		if clientIds := AbuseNetworkHintClientIds(ctx, networkId, 1); len(clientIds) != 1 {
			t.Fatalf("a bound of one answered %v", clientIds)
		}
	})
}

// The row network_abuse_state holds for a network, or "" for none.
func testNetworkAbuseState(t testing.TB, ctx context.Context, networkId server.Id) (state string, caseId server.Id, untilTime *time.Time) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT state, case_id, until_time FROM network_abuse_state WHERE network_id = $1`, networkId)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&state, &caseId, &untilTime))
			}
		})
	})
	return
}

// The suspended row: set under its case, a second suspension's case replacing
// it and a restricted row becoming it; ended to restricted while the window is
// still ahead, and to no row otherwise. Ending what is not suspended changes
// nothing, so a restricted row and a missing one stay as they are.
func TestNetworkAbuseSuspendedRow(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		networkId := server.NewId()
		caseId, secondCaseId := server.NewId(), server.NewId()

		if ended, err := EndNetworkAbuseSuspended(ctx, networkId, nil, now); err != nil || ended {
			t.Fatalf("ending no row: %t, %v", ended, err)
		}
		if _, suspended := GetNetworkAbuseSuspended(ctx, networkId); suspended {
			t.Fatal("a network with no row reads suspended")
		}

		if err := SetNetworkAbuseSuspended(ctx, networkId, caseId, now); err != nil {
			t.Fatal(err)
		}
		if state, rowCaseId, untilTime := testNetworkAbuseState(t, ctx, networkId); state != "suspended" || rowCaseId != caseId || untilTime != nil {
			t.Fatalf("the row reads %s under %s until %v", state, rowCaseId, untilTime)
		}
		if err := SetNetworkAbuseSuspended(ctx, networkId, secondCaseId, now); err != nil {
			t.Fatal(err)
		}
		if rowCaseId, suspended := GetNetworkAbuseSuspended(ctx, networkId); !suspended || rowCaseId != secondCaseId {
			t.Fatalf("the second suspension reads %s, %t", rowCaseId, suspended)
		}

		// a lift inside the window leaves the network restricted until its end
		restrictedUntil := now.Add(29 * 24 * time.Hour).Truncate(time.Microsecond)
		if ended, err := EndNetworkAbuseSuspended(ctx, networkId, &restrictedUntil, now); err != nil || !ended {
			t.Fatalf("ending the suspension: %t, %v", ended, err)
		}
		state, _, untilTime := testNetworkAbuseState(t, ctx, networkId)
		if state != "restricted" || untilTime == nil || !untilTime.Equal(restrictedUntil) {
			t.Fatalf("after the lift the row reads %s until %v", state, untilTime)
		}
		if _, suspended := GetNetworkAbuseSuspended(ctx, networkId); suspended {
			t.Fatal("a restricted network reads suspended")
		}
		// a restricted row is no suspension to end
		if ended, err := EndNetworkAbuseSuspended(ctx, networkId, nil, now); err != nil || ended {
			t.Fatalf("ending a restricted row: %t, %v", ended, err)
		}
		if state, _, _ := testNetworkAbuseState(t, ctx, networkId); state != "restricted" {
			t.Fatalf("the restricted row reads %q", state)
		}

		// a suspension replaces a restricted row, and a lift as wrong leaves none
		if err := SetNetworkAbuseSuspended(ctx, networkId, caseId, now); err != nil {
			t.Fatal(err)
		}
		if state, _, untilTime := testNetworkAbuseState(t, ctx, networkId); state != "suspended" || untilTime != nil {
			t.Fatalf("the suspension over a restricted row reads %s until %v", state, untilTime)
		}
		if ended, err := EndNetworkAbuseSuspended(ctx, networkId, nil, now); err != nil || !ended {
			t.Fatalf("ending as wrong: %t, %v", ended, err)
		}
		if state, _, _ := testNetworkAbuseState(t, ctx, networkId); state != "" {
			t.Fatalf("after a lift as wrong the row reads %q", state)
		}

		// a window already past leaves no row either
		if err := SetNetworkAbuseSuspended(ctx, networkId, caseId, now); err != nil {
			t.Fatal(err)
		}
		past := now.Add(-time.Hour)
		if ended, err := EndNetworkAbuseSuspended(ctx, networkId, &past, now); err != nil || !ended {
			t.Fatalf("ending past the window: %t, %v", ended, err)
		}
		if state, _, _ := testNetworkAbuseState(t, ctx, networkId); state != "" {
			t.Fatalf("past the window the row reads %q", state)
		}
	})
}

// The actions that stand are the network's own, not lifts, not lifted and not
// past their own until.
func TestGetNetworkStandingAbuseActions(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		args, caseId := testHostKill(t, ctx, now)
		pending, _, err := testRecordAbuseAction(t, ctx, args, now)
		if err != nil {
			t.Fatal(err)
		}
		record := func(kind AbuseActionKind, change func(args *AbuseActionArgs)) *AbuseAction {
			actionArgs := &AbuseActionArgs{ActionId: server.NewId(), CaseId: caseId, Kind: kind, Target: "shop.root.example"}
			if change != nil {
				change(actionArgs)
			}
			action, _, err := testRecordAbuseAction(t, ctx, actionArgs, now)
			if err != nil {
				t.Fatal(err)
			}
			return action
		}
		reviewerId := server.NewId()
		upheld := record(AbuseActionKindLabelKill, func(args *AbuseActionArgs) { args.ReviewerId = &reviewerId })
		ahead := now.Add(time.Hour)
		timed := record(AbuseActionKindZoneSuspend, func(args *AbuseActionArgs) { args.UntilTime = &ahead })
		past := now.Add(-time.Hour)
		record(AbuseActionKindHostSuspend, func(args *AbuseActionArgs) { args.UntilTime = &past })
		for _, reviewState := range []AbuseReviewState{AbuseReviewStateLifted, AbuseReviewStateLiftedWrong} {
			lifted := record(AbuseActionKindHostKill, nil)
			if _, _, _, err := testRecordAbuseLift(t, ctx, &AbuseLiftArgs{
				ActionId: server.NewId(), CaseId: caseId, LiftedActionId: lifted.ActionId, ReviewState: reviewState,
			}, now); err != nil {
				t.Fatal(err)
			}
		}
		// another network's action
		otherArgs, _ := testHostKill(t, ctx, now)
		if _, _, err := testRecordAbuseAction(t, ctx, otherArgs, now); err != nil {
			t.Fatal(err)
		}

		standing := GetNetworkStandingAbuseActions(ctx, *pending.NetworkId, now)
		got := []server.Id{}
		for _, action := range standing {
			got = append(got, action.ActionId)
		}
		want := []server.Id{pending.ActionId, upheld.ActionId, timed.ActionId}
		slices.SortFunc(got, func(a server.Id, b server.Id) int { return a.Cmp(b) })
		slices.SortFunc(want, func(a server.Id, b server.Id) int { return a.Cmp(b) })
		if !slices.Equal(got, want) {
			t.Fatalf("the standing actions are %v, want %v", got, want)
		}
		// past its own until, the timed action stands no longer
		if later := GetNetworkStandingAbuseActions(ctx, *pending.NetworkId, ahead.Add(time.Second)); len(later) != 2 {
			t.Fatalf("past the until %d actions stand", len(later))
		}
	})
}
