package controller

import (
	"bytes"
	"context"
	"crypto/hpke"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// §19.10's abuse step (abuse_restore_controller.go; ABUSE-M5-18): mailboxes built from testdata/abuse_mailbox/'s
// records, sealed to a key pair drawn for the test, read back, planned and applied on MG5's tables in a build with
// sidecar_pending, with the recording effects and mailer.

// One record of testdata/abuse_mailbox/records.json.
type testAbuseMailboxSpec struct {
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Category   string `json:"category"`
	Target     string `json:"target"`
	Lifts      string `json:"lifts"`
	Hours      int    `json:"hours"`
	UntilHours *int   `json:"until_hours"`
}

// One record of testdata/abuse_mailbox/legal_records.json.
type testLegalMailboxSpec struct {
	Name       string `json:"name"`
	Event      string `json:"event"`
	Kind       string `json:"kind"`
	PlaceDays  int    `json:"place_days"`
	ExpireDays int    `json:"expire_days"`
	SameHoldAs string `json:"same_hold_as"`
}

func testReadMailboxSpecs[T any](t testing.TB, name string) []T {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "abuse_mailbox", name))
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Records []T `json:"records"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	return file.Records
}

// An mbox export of bodies, each sealed plaintext to publicKey.
func testAbuseMailboxExport(t testing.TB, publicKey hpke.PublicKey, plaintexts [][]byte) []byte {
	t.Helper()
	settings := DefaultAbuseMailboxSettings()
	export := &bytes.Buffer{}
	for index, plaintext := range plaintexts {
		sealed, err := SealAbuseMailboxRecord(publicKey, plaintext, settings)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(export, "From records@mailbox.example Mon Mar  2 12:%02d:00 2026\n", index)
		fmt.Fprintf(export, "From: records@sender.example\nTo: abuse-records@mailbox.example\nSubject: URnetwork sealed record\n")
		fmt.Fprintf(export, "Content-Type: text/plain; charset=UTF-8\nContent-Transfer-Encoding: 7bit\n\n")
		export.WriteString(EncodeAbuseMailboxBody(sealed, settings))
		export.WriteString("\n")
	}
	return export.Bytes()
}

// The restore world: a network, the case and kill the dump holds, and the export of records.json's records for it,
// with one forged record and one message that opens to nothing.
type testAbuseRestoreWorld struct {
	networkId  server.Id
	dumpTime   time.Time
	now        time.Time
	privateKey hpke.PrivateKey
	export     []byte
	// each record by its name
	records map[string]*AbuseRecord
}

func newTestAbuseRestoreWorld(t testing.TB, ctx context.Context) *testAbuseRestoreWorld {
	t.Helper()
	networkId, _, dumpCaseId := newTestAbuseCase(t, ctx, false)
	world := &testAbuseRestoreWorld{
		networkId: networkId,
		dumpTime:  server.NowUtc().Add(-48 * time.Hour).Truncate(time.Millisecond),
		records:   map[string]*AbuseRecord{},
	}
	world.now = world.dumpTime.Add(24 * time.Hour)
	privateKey, err := hpke.MLKEM768X25519().GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	world.privateKey = privateKey

	specs := testReadMailboxSpecs[testAbuseMailboxSpec](t, "records.json")
	// every record after the dump under one case the dump lacks, re-created under its id
	afterCaseId := server.NewId()
	for _, spec := range specs {
		caseId := afterCaseId
		if spec.Hours < 0 {
			caseId = dumpCaseId
		}
		record := &AbuseRecord{
			ActionId:  server.NewId(),
			CaseId:    caseId,
			Category:  model.AbuseCategory(spec.Category),
			NetworkId: &networkId,
			Kind:      model.AbuseActionKind(spec.Kind),
			Target:    spec.Target,
			Time:      world.dumpTime.Add(time.Duration(spec.Hours) * time.Hour),
		}
		if spec.Target == "network" {
			record.Target = networkId.String()
		}
		if spec.UntilHours != nil {
			until := world.dumpTime.Add(time.Duration(*spec.UntilHours) * time.Hour)
			record.Until = &until
		}
		world.records[spec.Name] = record
	}
	for _, spec := range specs {
		if spec.Lifts != "" {
			lifted := world.records[spec.Lifts]
			record := world.records[spec.Name]
			record.Target = lifted.ActionId.String()
			record.CaseId = lifted.CaseId
			record.Category = lifted.Category
		}
	}
	// the dump holds the kill from before it, as the restored rows do
	before := world.records["kill_before_dump"]
	server.Tx(ctx, func(tx server.PgTx) {
		if _, _, err := model.RestoreAbuseActionInTx(ctx, tx, &model.AbuseRestoredAction{
			ActionId: before.ActionId, CaseId: before.CaseId, Category: before.Category, NetworkId: before.NetworkId,
			Kind: before.Kind, Target: before.Target, Time: before.Time,
		}, before.Time); err != nil {
			panic(err)
		}
	})
	model.SetAbuseActionApplied(ctx, before.ActionId, before.Time)

	plaintexts := [][]byte{}
	for _, spec := range specs {
		body, err := EncodeAbuseRecord(world.records[spec.Name], testAbuseRecordMacKey)
		if err != nil {
			t.Fatal(err)
		}
		plaintexts = append(plaintexts, body)
	}
	forged, _ := EncodeAbuseRecord(&AbuseRecord{
		ActionId: server.NewId(), CaseId: afterCaseId, Category: model.AbuseCategoryPhishing, NetworkId: &networkId,
		Kind: model.AbuseActionKindLift, Target: world.records["suspension_after_dump"].ActionId.String(), Time: world.dumpTime.Add(5 * time.Hour),
	}, []byte("not the mailbox's mac key at all."))
	plaintexts = append(plaintexts, forged)
	world.export = testAbuseMailboxExport(t, privateKey.PublicKey(), plaintexts)
	world.export = append(world.export, []byte("From nobody Mon Mar  2 13:00:00 2026\nSubject: noise\n\nnot a sealed record\n")...)
	return world
}

// The export reads back: every record opened and verified, the forged one and the noise refused in their places.
func TestParseAbuseRecordsOpensAndVerifies(t *testing.T) {
	requireAbuseTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		newTestAbuseDeps(t)
		world := newTestAbuseRestoreWorld(t, ctx)
		records := ParseAbuseRecords(world.export, world.privateKey, testAbuseRecordMacKey, DefaultAbuseMailboxSettings())
		if len(records.Records) != len(world.records) || len(records.Refused) != 2 {
			t.Fatalf("%d records, %d refused", len(records.Records), len(records.Refused))
		}
		if !errors.Is(records.Refused[0].Err, ErrAbuseRecordMac) || records.Refused[0].Index != len(world.records) {
			t.Fatalf("the forged record: %+v", records.Refused[0])
		}
		if !errors.Is(records.Refused[1].Err, ErrAbuseMailboxSealOpen) {
			t.Fatalf("the noise: %+v", records.Refused[1])
		}
		// another private key opens nothing
		other, _ := hpke.MLKEM768X25519().GenerateKey()
		if refused := ParseAbuseRecords(world.export, other, testAbuseRecordMacKey, DefaultAbuseMailboxSettings()); len(refused.Records) != 0 {
			t.Fatal("another key opened records")
		}
	})
}

// The plan keeps what came after the dump, unexpired, in commit order; the step prints it and applies nothing
// without the flag; with it, the kill, the suspension and the lifts land, the case re-created under its id, the
// kill from before the dump lifted by the lift after it, and no mail, notice or hint goes; a second run changes
// nothing.
func TestApplyAbuseRestorePlanReappliesWhatCameAfterTheDump(t *testing.T) {
	requireAbuseTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		effects, mailer := newTestAbuseDeps(t)
		world := newTestAbuseRestoreWorld(t, ctx)
		records := ParseAbuseRecords(world.export, world.privateKey, testAbuseRecordMacKey, DefaultAbuseMailboxSettings())
		plan := PlanAbuseRestore(records, world.dumpTime, world.now)
		if plan.BeforeDumpCount != 1 || plan.ExpiredCount != 1 || len(plan.Refused) != 2 {
			t.Fatalf("the plan: %d before, %d expired, %d refused", plan.BeforeDumpCount, plan.ExpiredCount, len(plan.Refused))
		}
		names := map[server.Id]string{}
		for name, record := range world.records {
			names[record.ActionId] = name
		}
		order := []string{}
		for _, item := range plan.Items {
			order = append(order, names[item.Record.ActionId])
		}
		want := []string{"kill_after_dump", "suspension_after_dump", "retried_kill", "lift_mailed_before_its_kill", "lift_after_dump_of_kill_before_dump"}
		if strings.Join(order, ",") != strings.Join(want, ",") {
			t.Fatalf("the plan's order %v", order)
		}

		printed := &bytes.Buffer{}
		result, err := ApplyAbuseRestorePlan(ctx, plan, false, printed)
		if err != nil || result.Applied || len(effects.applied()) != 0 {
			t.Fatalf("without the flag: %+v, %v", result, err)
		}
		for _, item := range plan.Items {
			if !strings.Contains(printed.String(), item.Record.ActionId.String()) {
				t.Fatalf("the plan did not print %s", item.Record.ActionId)
			}
			if model.GetAbuseAction(ctx, item.Record.ActionId) != nil {
				t.Fatal("an unflagged run recorded an action")
			}
		}
		if strings.Contains(printed.String(), "shop.root.example") {
			t.Fatal("the plan printed a target")
		}

		adminEventsBefore := testAbuseAdminEvents(t, ctx, world.networkId)
		result, err = ApplyAbuseRestorePlan(ctx, plan, true, &bytes.Buffer{})
		if err != nil || !result.Applied {
			t.Fatalf("with the flag: %+v, %v", result, err)
		}
		for i, outcome := range result.Outcomes {
			if outcome != AbuseRestoreOutcomeApplied {
				t.Fatalf("%s: %s", order[i], outcome)
			}
		}
		afterCase := model.GetAbuseCase(ctx, world.records["kill_after_dump"].CaseId)
		if afterCase == nil || !afterCase.Barred || afterCase.ReporterClass != model.AbuseReporterClassRestore {
			t.Fatalf("the re-created case %+v", afterCase)
		}
		kill := model.GetAbuseAction(ctx, world.records["kill_after_dump"].ActionId)
		if kill == nil || !kill.CreateTime.Equal(world.records["kill_after_dump"].Time) || kill.MailedTime == nil || kill.AppliedTime == nil {
			t.Fatalf("the re-applied kill %+v", kill)
		}
		if lifted := model.GetAbuseAction(ctx, world.records["kill_before_dump"].ActionId); lifted.ReviewState != model.AbuseReviewStateLifted {
			t.Fatalf("the dump's kill reads %s after the lift after the dump", lifted.ReviewState)
		}
		applied := effects.applied()
		if len(applied) != len(plan.Items) {
			t.Fatalf("%d effects", len(applied))
		}
		for _, effect := range applied {
			if !effect.Barred {
				t.Fatal("an effect may hint")
			}
		}
		if len(mailer.mailed()) != 0 {
			t.Fatalf("%d records mailed", len(mailer.mailed()))
		}
		if userAuths, _ := testAbuseNotices(t, ctx, world.networkId); len(userAuths) != 0 {
			t.Fatalf("%d notices owed", len(userAuths))
		}
		if adminEvents := testAbuseAdminEvents(t, ctx, world.networkId); len(adminEvents) != len(adminEventsBefore) {
			t.Fatalf("admin events %v", adminEvents)
		}

		again, err := ApplyAbuseRestorePlan(ctx, plan, true, &bytes.Buffer{})
		if err != nil {
			t.Fatal(err)
		}
		for i, outcome := range again.Outcomes {
			if outcome != AbuseRestoreOutcomeAlready {
				t.Fatalf("a second run: %s %s", order[i], outcome)
			}
		}
		if len(effects.applied()) != len(plan.Items) {
			t.Fatal("a second run applied effects again")
		}
	})
}

// The legal-request mailbox's holds after the dump are re-placed with their original placement, a renewal after the
// dump raises a restored row's expiry, and each is listed; after a site loss each is void.
func TestReplaceLegalHoldsKeepsThePlacement(t *testing.T) {
	requireAbuseTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clock := sidecartest.NewFakeClock(server.NowUtc())
		blobs := sidecartest.NewFakeBlobs(clock, nil, nil, nil)
		first, _ := blobs.FirstCluster()
		model.SetAbuseHoldStore(model.NewAbuseHoldStore(first))
		defer model.SetAbuseHoldStore(nil)

		networkId, _, _ := newTestAbuseCase(t, ctx, false)
		dumpTime := server.NowUtc().Add(-24 * time.Hour).Truncate(time.Millisecond)
		now := server.NowUtc()
		specs := testReadMailboxSpecs[testLegalMailboxSpec](t, "legal_records.json")
		caseIds := map[string]server.Id{}
		records := []*LegalHoldRecord{}
		for _, spec := range specs {
			caseId := server.NewId()
			if spec.SameHoldAs != "" {
				caseId = caseIds[spec.SameHoldAs]
			}
			caseIds[spec.Name] = caseId
			records = append(records, &LegalHoldRecord{
				Event:      LegalHoldEvent(spec.Event),
				CaseId:     caseId,
				NetworkId:  networkId,
				Kind:       sidecar.HoldKind(spec.Kind),
				Scope:      sidecar.HoldScopeNetwork(),
				PlaceTime:  dumpTime.AddDate(0, 0, spec.PlaceDays),
				ExpireTime: dumpTime.AddDate(0, 0, spec.ExpireDays),
			})
		}
		// the dump holds the hold placed before it, at its first expiry
		server.Tx(ctx, func(tx server.PgTx) {
			model.ReplaceLegalHoldInTx(ctx, tx, &model.RestoredLegalHold{
				NetworkId: networkId, CaseId: caseIds["placed_before_dump"], Kind: sidecar.HoldKindPreserve2703f,
				Scope: sidecar.HoldScopeNetwork(), PlaceTime: records[0].PlaceTime, ExpireTime: records[0].ExpireTime,
			})
		})

		// sealed, exported and read back first
		privateKey, _ := hpke.MLKEM768X25519().GenerateKey()
		plaintexts := [][]byte{}
		for _, record := range records {
			body, err := EncodeLegalHoldRecord(record, testAbuseRecordMacKey)
			if err != nil {
				t.Fatal(err)
			}
			plaintexts = append(plaintexts, body)
		}
		parsed := ParseLegalHoldRecords(testAbuseMailboxExport(t, privateKey.PublicKey(), plaintexts), privateKey, testAbuseRecordMacKey, DefaultAbuseMailboxSettings())
		if len(parsed.Records) != len(records) || len(parsed.Refused) != 0 {
			t.Fatalf("%d records, %d refused", len(parsed.Records), len(parsed.Refused))
		}

		listed, err := ReplaceLegalHolds(ctx, parsed.Records, dumpTime, now, false, false, &bytes.Buffer{})
		if err != nil || len(listed) != 3 {
			t.Fatalf("the unflagged list: %d, %v", len(listed), err)
		}
		if model.GetLegalHold(ctx, networkId, caseIds["placed_after_dump"]) != nil {
			t.Fatal("an unflagged run placed a hold")
		}
		printed := &bytes.Buffer{}
		listed, err = ReplaceLegalHolds(ctx, parsed.Records, dumpTime, now, true, true, printed)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range listed {
			if !line.Void || line.Outcome != LegalHoldRestoreOutcomePlaced {
				t.Fatalf("a line %+v", line)
			}
		}
		if !strings.Contains(printed.String(), "void") {
			t.Fatal("a site loss's list does not say void")
		}
		placed := model.GetLegalHold(ctx, networkId, caseIds["placed_after_dump"])
		if placed == nil || !placed.PlaceTime.Equal(records[1].PlaceTime) || !placed.ExpireTime.Equal(records[1].ExpireTime) {
			t.Fatalf("the re-placed hold %+v", placed)
		}
		renewed := model.GetLegalHold(ctx, networkId, caseIds["placed_before_dump"])
		if renewed == nil || !renewed.ExpireTime.Equal(records[3].ExpireTime) || renewed.RenewTime == nil {
			t.Fatalf("the renewed hold %+v", renewed)
		}
		object, found, err := model.NewAbuseHoldStore(first).Read(ctx, networkId, caseIds["report_after_dump"])
		if err != nil || !found || object.Record == nil || !object.Record.PlaceTime.Equal(records[2].PlaceTime) {
			t.Fatalf("the re-placed object %+v, %t, %v", object, found, err)
		}
		if model.GetLegalHold(ctx, networkId, caseIds["ended_long_ago"]) != nil {
			t.Fatal("an ended hold came back")
		}
		again, err := ReplaceLegalHolds(ctx, parsed.Records, dumpTime, now, false, true, &bytes.Buffer{})
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range again {
			if line.Outcome != LegalHoldRestoreOutcomeAlready {
				t.Fatalf("a second run: %+v", line)
			}
		}
	})
}
