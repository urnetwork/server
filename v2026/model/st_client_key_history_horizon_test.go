package model

// The key history retention keeps a gone client's history 7 days, whatever
// the contract horizon reaper's runtime control says (owner, 2026-11-16).
// DB-backed.

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Sets the contract horizon reaper's control for the test's Redis database.
func setGoneKeyHistoryTestContractHorizon(t testing.TB, ctx context.Context, fields map[string]string) {
	t.Helper()
	controlCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if fields == nil {
		if err := RemoveContractHorizonControl(controlCtx); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err := SetContractHorizonControl(controlCtx, fields); err != nil {
		t.Fatal(err)
	}
}

// The reaper's control no longer moves the key history horizon: with the
// control absent, on at 45 days, in a dry run, off, or unreadable, a client
// gone 8 days loses its history and one gone 6 days keeps it. Before
// 2026-11-16 the retention followed the reaper while its mode was on and kept
// 91 days otherwise, so the client gone 8 days kept its history in every case.
func TestGoneStClientKeyHistoryIgnoresTheContractHorizon(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		const day = 24 * time.Hour
		for _, test := range []struct {
			name       string
			control    map[string]string
			unreadable bool
		}{
			{name: "no reaper control"},
			{name: "the reaper on at 45 days", control: map[string]string{ContractHorizonControlFieldMode: "on", ContractHorizonControlFieldHorizonDays: "45"}},
			{name: "the reaper in a dry run", control: map[string]string{ContractHorizonControlFieldMode: "dry_run", ContractHorizonControlFieldHorizonDays: "45"}},
			{name: "the reaper off", control: map[string]string{ContractHorizonControlFieldMode: "off"}},
			{name: "an unreadable reaper control", unreadable: true},
		} {
			keptId, removedId := server.NewId(), server.NewId()
			for _, clientId := range []server.Id{keptId, removedId} {
				insertGoneKeyHistoryTestRows(t, ctx, clientId, server.NewId())
			}
			setGoneKeyHistoryTestDays(t, ctx, keptId, 6)
			setGoneKeyHistoryTestDays(t, ctx, removedId, 8)
			setGoneKeyHistoryTestContractHorizon(t, ctx, nil)
			if test.control != nil {
				setGoneKeyHistoryTestContractHorizon(t, ctx, test.control)
			}
			if test.unreadable {
				setGoneKeyHistoryTestUnreadable(t, ctx, contractHorizonControlKey)
			}

			now := goneKeyHistoryTestDbNow(t, ctx)
			report, _ := RemoveGoneStClientKeyHistory(ctx, now, goneKeyHistoryTestDeleteOnly(now), DefaultStClientKeyHistoryRetentionSettings())
			if report.RemovedClientCount != 1 || report.RemovedHistoryCount != 1 || report.Horizon != 7*day {
				t.Fatalf("with %s the retention = %+v, want one client at 7 days", test.name, report)
			}
			if rows := readGoneKeyHistoryTestRows(t, ctx, keptId); rows != (goneKeyHistoryTestRows{history: 1, heads: 1, gone: true}) {
				t.Fatalf("with %s a client gone 6 days lost key history: %+v", test.name, rows)
			}
			if rows := readGoneKeyHistoryTestRows(t, ctx, removedId); rows != (goneKeyHistoryTestRows{}) {
				t.Fatalf("with %s a client gone 8 days kept key history: %+v", test.name, rows)
			}
		}
	})
}
