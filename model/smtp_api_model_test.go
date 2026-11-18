//go:build sidecar_pending

package model

// §14.14's routes' model half on a private database (SMTP-M9-19; I-15, FS-42): a publish writes its row, its record
// and its admin event in one transaction and the same body again nothing; the 65th device selector refused, the
// whole call with it, a burner's never counted, a rotation at the cap taken; a rotated key kept in smtp_dkim_key and
// out of DNS after 7 days; mail_send asked of a publish alone; a removal taking its sidecar's selectors in its own
// write and nothing when it rolls back; FS-42's M9 step, a sidecar revoked after the dump with no selector served
// once the replay ends; the status's limits, halts and day; and the reports a page at a time.

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/dnsservice"
	"github.com/urnetwork/sidecar/smtpedgeservice"

	"github.com/urnetwork/server"
)

// A network with mail records in a root this process serves, and a sending sidecar of it with one RSA-2048 key.
type smtpApiWorld struct {
	root      string
	networkId server.Id
	apex      string
	domain    string
	caller    *SmtpDkimKeysCaller
	publicKey []byte
}

func newSmtpApiWorld(t testing.TB, ctx context.Context) *smtpApiWorld {
	t.Helper()
	records := newTestDnsRecordsWorld(t, ctx, sidecar.AbuseStandingStandard, true)
	if _, err := DnsSyncEdgeRecords(ctx, records.networkId, records.root, true, false); err != nil {
		t.Fatal(err)
	}
	if _, err := DnsSetMailRecords(ctx, records.networkId, records.root, true); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(Testing_SetDnsUserRoot(records.root))
	publicKey, _ := smtpTestDkimPublicKey(t)
	return &smtpApiWorld{
		root:      records.root,
		networkId: records.networkId,
		apex:      records.apex,
		domain:    strings.TrimSuffix(records.apex, "."),
		caller:    &SmtpDkimKeysCaller{NetworkId: records.networkId, ClientId: server.NewId(), SidecarId: server.NewId()},
		publicKey: publicKey,
	}
}

func smtpTestAllowed() (bool, error) {
	return true, nil
}

// Publishes and retires as the world's sidecar, mail_send granted.
func (self *smtpApiWorld) call(ctx context.Context, publish []string, publishDomain string, retire []string) (*sidecar.SmtpDkimKeysResult, error) {
	args := &sidecar.SmtpDkimKeysArgs{}
	for _, selector := range publish {
		args.Publish = append(args.Publish, &sidecar.SmtpDkimKeyPublish{Selector: selector, Domain: publishDomain, PublicKey: self.publicKey})
	}
	for _, selector := range retire {
		args.Retire = append(args.Retire, &sidecar.SmtpDkimKeyRetire{Selector: selector, Domain: self.domain})
	}
	return PublishSmtpDkimKeys(ctx, self.caller, args, smtpTestAllowed, server.NowUtc(), DefaultSmtpApiSettings())
}

// Whether root's snapshot serves a TXT at owner.
func smtpTestServed(t testing.TB, ctx context.Context, root string, owner string) bool {
	t.Helper()
	_, held := testDnsSnapshotRows(t, ctx, root)[dnsservice.DnsRecordKey{Owner: owner, Type: dns.TypeTXT}]
	return held
}

// One publish writes the row, the record and its admin event, the actor the sidecar; the same body again writes
// nothing; a retirement of a selector the caller does not hold, or a second one, is no error; a domain that is not
// the network's refuses the whole call before anything is written.
func TestSmtpDkimKeysPublishWritesItsRowRecordAndEventTogether(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newSmtpApiWorld(t, ctx)
		selector := smtpTestSelector()
		result, err := world.call(ctx, []string{selector}, world.domain, nil)
		if err != nil || !slices.Equal(result.Published, []string{selector}) || len(result.Retired) != 0 {
			t.Fatalf("publish %+v, %v", result, err)
		}
		if !smtpTestServed(t, ctx, world.root, selector+"._domainkey."+world.apex) {
			t.Fatal("the selector is not served")
		}
		key, err := FindSmtpDkimKey(ctx, selector, world.domain)
		if err != nil || key == nil || string(key.PublicKey) != string(world.publicKey) || key.SidecarId != sidecar.Id(world.caller.SidecarId) {
			t.Fatalf("the row %+v, %v", key, err)
		}
		events := 0
		for _, event := range GetNetworkAdminEvents(ctx, world.networkId, 0, 0) {
			if event.Action == AdminEventActionDnsDkimKey {
				events += 1
				if event.ActorKind != AdminEventActorSidecar || event.ActorId == nil || *event.ActorId != world.caller.SidecarId {
					t.Fatalf("the event's actor %s %v", event.ActorKind, event.ActorId)
				}
			}
		}
		if events != 1 {
			t.Fatalf("%d key events", events)
		}
		serial := testDnsRootSerial(t, ctx, world.root)
		again, err := world.call(ctx, []string{selector}, world.domain, nil)
		if err != nil || !slices.Equal(again.Published, []string{selector}) || testDnsRootSerial(t, ctx, world.root) != serial {
			t.Fatalf("the same publish again %+v, %v", again, err)
		}
		retired, err := world.call(ctx, nil, "", []string{smtpTestSelector()})
		if err != nil || len(retired.Retired) != 1 {
			t.Fatalf("retiring a selector not held %+v, %v", retired, err)
		}

		other := smtpTestSelector()
		_, err = world.call(ctx, []string{other}, "elsewhere.example", nil)
		apiErr := &SmtpApiError{}
		if !errors.As(err, &apiErr) || apiErr.Code != SmtpApiCodeDomain || apiErr.Status != http.StatusBadRequest {
			t.Fatalf("a foreign domain answered %v", err)
		}
		if key, _ := FindSmtpDkimKey(ctx, other, "elsewhere.example"); key != nil {
			t.Fatal("a refused call wrote its row")
		}
		// another sidecar may not take or retire a stored selector
		thief := *world
		thief.caller = &SmtpDkimKeysCaller{NetworkId: world.networkId, ClientId: server.NewId(), SidecarId: server.NewId()}
		if _, err := thief.call(ctx, []string{selector}, world.domain, nil); !errors.As(err, &apiErr) || apiErr.Code != SmtpApiCodeSelectorTaken {
			t.Fatalf("another sidecar's publish answered %v", err)
		}
		if _, err := thief.call(ctx, nil, "", []string{selector}); err != nil {
			t.Fatal(err)
		}
		if state, _ := smtpTestDkimKeyState(ctx, selector, world.domain); state != SmtpDkimKeyStatePublished {
			t.Fatalf("another sidecar retired the selector: %s", state)
		}
	})
}

// A publish asks mail_send, and its refusal writes nothing; a retirement alone never asks it.
func TestSmtpDkimKeysPublishNeedsMailSend(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newSmtpApiWorld(t, ctx)
		selector := smtpTestSelector()
		args := &sidecar.SmtpDkimKeysArgs{Publish: []*sidecar.SmtpDkimKeyPublish{{Selector: selector, Domain: world.domain, PublicKey: world.publicKey}}}
		_, err := PublishSmtpDkimKeys(ctx, world.caller, args, func() (bool, error) { return false, nil }, server.NowUtc(), DefaultSmtpApiSettings())
		apiErr := &SmtpApiError{}
		if !errors.As(err, &apiErr) || apiErr.Code != SmtpApiCodeMailSend || apiErr.Status != http.StatusForbidden {
			t.Fatalf("no grant answered %v", err)
		}
		if _, err := PublishSmtpDkimKeys(ctx, world.caller, args, nil, server.NowUtc(), DefaultSmtpApiSettings()); !errors.Is(err, ErrSmtpApiUnavailable) {
			t.Fatalf("no check answered %v", err)
		}
		if key, _ := FindSmtpDkimKey(ctx, selector, world.domain); key != nil || smtpTestServed(t, ctx, world.root, selector+"._domainkey."+world.apex) {
			t.Fatal("a refused publish wrote")
		}
		asked := false
		retire := &sidecar.SmtpDkimKeysArgs{Retire: []*sidecar.SmtpDkimKeyRetire{{Selector: selector, Domain: world.domain}}}
		if _, err := PublishSmtpDkimKeys(ctx, world.caller, retire, func() (bool, error) { asked = true; return false, nil }, server.NowUtc(), DefaultSmtpApiSettings()); err != nil || asked {
			t.Fatalf("a retirement asked %t, %v", asked, err)
		}
		for _, bad := range []*sidecar.SmtpDkimKeysArgs{
			{},
			{Publish: []*sidecar.SmtpDkimKeyPublish{{Selector: "short", Domain: world.domain, PublicKey: world.publicKey}}},
			{Publish: []*sidecar.SmtpDkimKeyPublish{{Selector: selector, Domain: world.domain, PublicKey: []byte("not a key")}}},
			{Publish: []*sidecar.SmtpDkimKeyPublish{{Selector: selector, Domain: strings.ToUpper(world.domain), PublicKey: world.publicKey}}},
			{Publish: []*sidecar.SmtpDkimKeyPublish{{Selector: selector, Domain: world.domain, PublicKey: world.publicKey}}, Retire: []*sidecar.SmtpDkimKeyRetire{{Selector: selector, Domain: world.domain}}},
		} {
			if _, err := PublishSmtpDkimKeys(ctx, world.caller, bad, smtpTestAllowed, server.NowUtc(), DefaultSmtpApiSettings()); !errors.As(err, &apiErr) || apiErr.Code != SmtpApiCodeArgs {
				t.Fatalf("%+v answered %v", bad, err)
			}
		}
	})
}

// With limits enforced, the network's 64 device selectors are taken and the 65th refused with the quota's refusal,
// the whole call with it, a burner's publish in the same call included; a burner's selector alone is taken past
// the cap, since burners' never count; and a rotation at the cap, one published for one retired, is taken.
func TestSmtpDkimSelectorSixtyFiveRefusedAndABurnersNotCounted(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newSmtpApiWorld(t, ctx)
		t.Cleanup(Testing_SetEnforceSidecarLimits(true))
		t.Cleanup(Testing_SetSidecarLimits(NetworkTierFree, map[string]int64{SmtpDkimSelectorsLimitKey: 64}))
		selectors := []string{}
		for range 64 {
			selectors = append(selectors, smtpTestSelector())
		}
		for _, page := range [][]string{selectors[:32], selectors[32:]} {
			if _, err := world.call(ctx, page, world.domain, nil); err != nil {
				t.Fatal(err)
			}
		}
		burnerDomain := "maple-1234." + world.domain
		burner := smtpTestSelector()
		sixtyFifth := smtpTestSelector()
		args := &sidecar.SmtpDkimKeysArgs{Publish: []*sidecar.SmtpDkimKeyPublish{
			{Selector: burner, Domain: burnerDomain, PublicKey: world.publicKey},
			{Selector: sixtyFifth, Domain: world.domain, PublicKey: world.publicKey},
		}}
		_, err := PublishSmtpDkimKeys(ctx, world.caller, args, smtpTestAllowed, server.NowUtc(), DefaultSmtpApiSettings())
		quotaErr := &QuotaRefusalError{}
		if !errors.As(err, &quotaErr) || quotaErr.Refusal.LimitKey != SmtpDkimSelectorsLimitKey || quotaErr.Refusal.Limit != 64 || quotaErr.Refusal.Used != 64 {
			t.Fatalf("the 65th answered %v", err)
		}
		for _, key := range [][2]string{{burner, burnerDomain}, {sixtyFifth, world.domain}} {
			if found, _ := FindSmtpDkimKey(ctx, key[0], key[1]); found != nil {
				t.Fatalf("the refused call wrote %s", key[0])
			}
		}
		if smtpTestServed(t, ctx, world.root, burner+"._domainkey.maple-1234."+world.apex) {
			t.Fatal("the refused call published the burner's record")
		}
		if _, err := world.call(ctx, []string{burner}, burnerDomain, nil); err != nil {
			t.Fatalf("a burner's selector past the cap: %v", err)
		}
		if !smtpTestServed(t, ctx, world.root, burner+"._domainkey.maple-1234."+world.apex) {
			t.Fatal("the burner's selector is not served")
		}
		rotated := smtpTestSelector()
		if _, err := world.call(ctx, []string{rotated}, world.domain, []string{selectors[0]}); err != nil {
			t.Fatalf("a rotation at the cap: %v", err)
		}
	})
}

// A rotation keeps the old selector's row, retired, and its record served until 7 days after it; then key expiry
// takes the record out and keeps the row, removed.
func TestSmtpDkimRotatedKeyStaysAndLeavesDnsAfterSevenDays(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newSmtpApiWorld(t, ctx)
		old := smtpTestSelector()
		next := smtpTestSelector()
		if _, err := world.call(ctx, []string{old}, world.domain, nil); err != nil {
			t.Fatal(err)
		}
		rotateTime := server.NowUtc()
		result, err := world.call(ctx, []string{next}, world.domain, []string{old})
		if err != nil || !slices.Equal(result.Published, []string{next}) || !slices.Equal(result.Retired, []string{old}) {
			t.Fatalf("rotation %+v, %v", result, err)
		}
		oldOwner := old + "._domainkey." + world.apex
		if state, _ := smtpTestDkimKeyState(ctx, old, world.domain); state != SmtpDkimKeyStateRetired || !smtpTestServed(t, ctx, world.root, oldOwner) {
			t.Fatalf("after the rotation the old key is %s", state)
		}
		settings := DefaultSmtpRetentionSettings()
		if early := ExpireSmtpDkimKeys(ctx, rotateTime.Add(settings.DkimRetiredKeepTimeout-time.Hour), settings); early.RemovedFromDnsCount != 0 || !smtpTestServed(t, ctx, world.root, oldOwner) {
			t.Fatalf("early %+v", early)
		}
		due := ExpireSmtpDkimKeys(ctx, rotateTime.Add(settings.DkimRetiredKeepTimeout+time.Hour), settings)
		if due.RemovedFromDnsCount != 1 || smtpTestServed(t, ctx, world.root, oldOwner) || !smtpTestServed(t, ctx, world.root, next+"._domainkey."+world.apex) {
			t.Fatalf("due %+v", due)
		}
		if state, _ := smtpTestDkimKeyState(ctx, old, world.domain); state != SmtpDkimKeyStateRemoved {
			t.Fatalf("the old row is %q", state)
		}
	})
}

// The removal effect takes the sidecar's selectors out of DNS in the removal's own transaction: rolled back, they
// stay served; committed, they are gone, the rows removed and kept, another sidecar's untouched.
func TestSmtpDkimRemovalDeletesSelectorsInItsOwnWrite(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newSmtpApiWorld(t, ctx)
		network := smtpTestSelector()
		burner := smtpTestSelector()
		if _, err := world.call(ctx, []string{network}, world.domain, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := world.call(ctx, []string{burner}, "maple-1234."+world.domain, nil); err != nil {
			t.Fatal(err)
		}
		other := *world
		other.caller = &SmtpDkimKeysCaller{NetworkId: world.networkId, ClientId: server.NewId(), SidecarId: server.NewId()}
		kept := smtpTestSelector()
		if _, err := other.call(ctx, []string{kept}, world.domain, nil); err != nil {
			t.Fatal(err)
		}
		removal := &Removal{
			Subjects: []*RemovalSubject{{NetworkId: world.networkId, Kind: sidecar.RevocationKindSidecar, SubjectId: world.caller.SidecarId}},
			Reason:   sidecar.RevocationReasonRemoved,
			Time:     server.NowUtc(),
		}
		owners := []string{network + "._domainkey." + world.apex, burner + "._domainkey.maple-1234." + world.apex}
		rolledBack := errors.New("the removal rolled back")
		err := dnsCatchRefusal(func() {
			server.Tx(ctx, func(tx server.PgTx) {
				if err := ApplyRemovalEffectsInTx(ctx, tx, removal); err != nil {
					t.Error(err)
				}
				panic(dnsWriteRefusal{err: rolledBack})
			}, server.TxReadCommitted)
		})
		if !errors.Is(err, rolledBack) {
			t.Fatal(err)
		}
		for _, owner := range owners {
			if !smtpTestServed(t, ctx, world.root, owner) {
				t.Fatalf("a rolled back removal took %s", owner)
			}
		}
		server.Tx(ctx, func(tx server.PgTx) {
			if err := ApplyRemovalEffectsInTx(ctx, tx, removal); err != nil {
				t.Fatal(err)
			}
		}, server.TxReadCommitted)
		for _, owner := range owners {
			if smtpTestServed(t, ctx, world.root, owner) {
				t.Fatalf("the removal left %s", owner)
			}
		}
		for _, key := range [][2]string{{network, world.domain}, {burner, "maple-1234." + world.domain}} {
			if state, removeTime := smtpTestDkimKeyState(ctx, key[0], key[1]); state != SmtpDkimKeyStateRemoved || removeTime == nil {
				t.Fatalf("the removed sidecar's row is %q", state)
			}
		}
		if !smtpTestServed(t, ctx, world.root, kept+"._domainkey."+world.apex) {
			t.Fatal("the removal took another sidecar's selector")
		}
	})
}

// FS-42's M9 step: a sidecar revoked after the dump comes back from the restore with its selector published, row and
// record both, and api's replay of the removal takes the record off both NS, the snapshot every NS serves; the
// replay run again writes nothing and fails nothing.
func TestSmtpDkimRevokedAfterTheDumpHasNoSelectorOnceTheReplayEnds(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newSmtpApiWorld(t, ctx)
		selector := smtpTestSelector()
		if _, err := world.call(ctx, []string{selector}, world.domain, nil); err != nil {
			t.Fatal(err)
		}
		owner := selector + "._domainkey." + world.apex
		removal := &Removal{
			Subjects: []*RemovalSubject{{NetworkId: world.networkId, Kind: sidecar.RevocationKindSidecar, SubjectId: world.caller.SidecarId}},
			Reason:   sidecar.RevocationReasonRemoved,
			Time:     server.NowUtc(),
		}
		applyRemoval := func() {
			server.Tx(ctx, func(tx server.PgTx) {
				if err := ApplyRemovalEffectsInTx(ctx, tx, removal); err != nil {
					t.Fatal(err)
				}
			}, server.TxReadCommitted)
		}
		applyRemoval()
		if smtpTestServed(t, ctx, world.root, owner) {
			t.Fatal("the revocation left the selector")
		}
		// the dumps, taken before the revocation, put back the published row and its record
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE smtp_dkim_key SET state = $3, retire_time = NULL, remove_time = NULL WHERE selector = $1 AND domain = $2`, selector, world.domain, SmtpDkimKeyStatePublished))
			sidecarId := world.caller.SidecarId
			if _, err := DnsPutDkimSelectorInTx(ctx, tx, &DnsDkimSelector{Root: world.root, NetworkId: world.networkId, Selector: selector, Value: smtpDkimRecordValue(world.publicKey), SidecarId: &sidecarId}); err != nil {
				t.Fatal(err)
			}
		}, server.TxReadCommitted)
		if !smtpTestServed(t, ctx, world.root, owner) {
			t.Fatal("the restore did not put the selector back")
		}
		// api's replay applies the removal recorded after the dump again
		applyRemoval()
		if smtpTestServed(t, ctx, world.root, owner) {
			t.Fatal("the replay left the selector on the NS")
		}
		serial := testDnsRootSerial(t, ctx, world.root)
		applyRemoval()
		if testDnsRootSerial(t, ctx, world.root) != serial {
			t.Fatal("a second replay wrote")
		}
	})
}

// The status gives the network's smtp limits, the halts holding now with their scope, and the day's counts.
func TestSmtpStatusGivesLimitsHaltsAndTheDay(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		now := server.NowUtc()
		halts := []*smtpedgeservice.SmtpHalt{
			{HaltId: sidecar.NewId(), NetworkId: sidecar.Id(networkId), Label: "maple-1234", Trigger: smtpedgeservice.SmtpHaltTriggerLabelComplaint, StartTime: now.Add(-time.Hour)},
			{HaltId: sidecar.NewId(), NetworkId: sidecar.Id(networkId), Trigger: smtpedgeservice.SmtpHaltTriggerFraud, StartTime: now.Add(-25 * time.Hour)},
		}
		for _, halt := range halts {
			if err := SetSmtpHalt(ctx, halt); err != nil {
				t.Fatal(err)
			}
		}
		readDay := func(ctx context.Context, readNetworkId server.Id, day int64) (*SmtpStatusDay, error) {
			if readNetworkId != networkId || day != smtpedgeservice.SmtpSendDay(now) {
				t.Errorf("read the day %d of %s", day, readNetworkId)
			}
			return &SmtpStatusDay{ConnectionCount: 3, RecipientCount: 7, ByteCount: 4096}, nil
		}
		status, err := GetSmtpStatus(ctx, networkId, now, readDay)
		if err != nil {
			t.Fatal(err)
		}
		if len(status.Halts) != 1 || status.Halts[0].Label != "maple-1234" || status.Halts[0].HaltId != server.Id(halts[0].HaltId) ||
			status.Halts[0].EndTime != halts[0].StartTime.Add(smtpedgeservice.SmtpHaltDuration).UnixMilli() {
			t.Fatalf("halts %+v", status.Halts)
		}
		if status.Today == nil || status.Today.RecipientCount != 7 {
			t.Fatalf("today %+v", status.Today)
		}
		for key := range status.Limits {
			if !strings.HasPrefix(key, "smtp_") && key != SmtpDkimSelectorsLimitKey {
				t.Fatalf("a limit of another area: %s", key)
			}
		}
		if _, ok := status.Limits[smtpedgeservice.SmtpLimitConnectionsPerDay]; !ok {
			t.Fatalf("no connections a day in %v", status.Limits)
		}
		failed, err := GetSmtpStatus(ctx, networkId, now, func(ctx context.Context, networkId server.Id, day int64) (*SmtpStatusDay, error) {
			return nil, errors.New("redis did not answer")
		})
		if err != nil || failed.Today != nil || len(failed.Halts) != 1 {
			t.Fatalf("with no redis %+v, %v", failed, err)
		}
	})
}

// The reports come a page at a time on the route's cursor, complaints by arrival with their sealed copies under the
// page's byte bound, the page's first always taken, and aggregate rows by key, until both lists are done; another
// network's rows never appear, and a cursor this route did not give is refused.
func TestSmtpReportsComeAPageAtATime(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := sidecar.NewId()
		otherNetworkId := sidecar.NewId()
		since := time.Date(2032, time.March, 1, 0, 0, 0, 0, time.UTC)
		complaintIds := []sidecar.Id{}
		for i := range 5 {
			complaint := smtpTestComplaint(networkId, since.Add(time.Duration(i)*time.Hour))
			if err := InsertSmtpComplaint(ctx, complaint); err != nil {
				t.Fatal(err)
			}
			complaintIds = append(complaintIds, complaint.ComplaintId)
		}
		early := smtpTestComplaint(networkId, since.Add(-time.Minute))
		foreign := smtpTestComplaint(otherNetworkId, since.Add(time.Minute))
		for _, complaint := range []*smtpedgeservice.SmtpComplaint{early, foreign} {
			if err := InsertSmtpComplaint(ctx, complaint); err != nil {
				t.Fatal(err)
			}
		}
		for i := range 3 {
			report := smtpTestAggregate("reporter.example", smtpTestReportId(), networkId, uint64(i+1))
			report.Day = since.AddDate(0, 0, i)
			report.ArrivalTime = since.AddDate(0, 0, i+1)
			if _, err := InsertSmtpAggregate(ctx, report); err != nil {
				t.Fatal(err)
			}
		}
		settings := DefaultSmtpApiSettings()
		settings.ReportsPageCount = 2
		settings.ReportsPageAggregateRowCount = 2
		// two copies fit, so the bound is tested apart below
		settings.ReportsPageSealedByteCount = 2 * 96

		gotComplaints := []sidecar.Id{}
		gotAggregates := 0
		cursor := ""
		for pageCount := 0; ; pageCount++ {
			if 10 < pageCount {
				t.Fatal("the pages never end")
			}
			page, err := GetSmtpReportsPage(ctx, server.Id(networkId), since, cursor, settings)
			if err != nil {
				t.Fatal(err)
			}
			if 2 < len(page.Complaints) || 2 < len(page.Aggregates) {
				t.Fatalf("a page of %d and %d", len(page.Complaints), len(page.Aggregates))
			}
			for _, complaint := range page.Complaints {
				if len(complaint.SealedReport) != 96 {
					t.Fatalf("a sealed copy of %d", len(complaint.SealedReport))
				}
				gotComplaints = append(gotComplaints, sidecar.Id(complaint.ComplaintId))
			}
			gotAggregates += len(page.Aggregates)
			if page.Cursor == "" {
				break
			}
			cursor = page.Cursor
		}
		if !slices.Equal(gotComplaints, complaintIds) || gotAggregates != 3 {
			t.Fatalf("read %v and %d aggregates, want %v", gotComplaints, gotAggregates, complaintIds)
		}

		// a bound under one copy still takes the page's first
		settings.ReportsPageSealedByteCount = 10
		page, err := GetSmtpReportsPage(ctx, server.Id(networkId), since, "", settings)
		if err != nil || len(page.Complaints) != 1 || page.Cursor == "" {
			t.Fatalf("a small bound %d complaints, %v", len(page.Complaints), err)
		}
		apiErr := &SmtpApiError{}
		if _, err := GetSmtpReportsPage(ctx, server.Id(networkId), since, "not a cursor", settings); !errors.As(err, &apiErr) || apiErr.Code != SmtpApiCodeCursor {
			t.Fatalf("a foreign cursor answered %v", err)
		}
	})
}
