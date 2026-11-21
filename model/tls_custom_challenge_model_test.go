//go:build sidecar_pending

package model

// A delegated DNS-01 challenge's receipt on a private database (QD-1165): the capture under the caller's claim, the
// worker's steps over §12.6's real write path and its journal, standing in for certs' DNS client with
// DnsPutChallenge, the dns api's own call. A retry recovers the receipt with no second write; a lost acknowledgement
// is settled from the journal, so the hourly cap is not charged twice; a write never made is owed again only past
// SettleAfter, and only DispatchMaxCount times; a confirmed stale verdict owes a new write of the same digest; a
// newer claim holder before the dispatch refuses the old claim's write; a full window refuses with its wait and
// charges nothing; and a receipt goes with its hour. The store's clock starts at the database's now, so the
// journal's commit times and the receipts read one time, and the test moves it.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
)

// A custom world whose desktop holds the account's claim for an order of a device's row.
type testTlsCustomChallengeWorld struct {
	*testTlsCustomWorld
	account   *sidecar.TlsCustomAccount
	certId    sidecar.Id
	desktop   *testTlsCustomDevice
	claim     *sidecar.TlsCustomClaimResult
	settings  *TlsCustomChallengeSettings
	scheduled []server.Id
}

func newTestTlsCustomChallengeWorld(t testing.TB, ctx context.Context) *testTlsCustomChallengeWorld {
	t.Helper()
	world := &testTlsCustomChallengeWorld{testTlsCustomWorld: newTestTlsCustomWorld(t, ctx), settings: DefaultTlsCustomChallengeSettings()}
	// the database keeps microseconds
	world.now = server.NowUtc().Truncate(time.Microsecond)
	// §12.7's ten writes a name an hour, which the local pro.yml leaves out
	t.Cleanup(Testing_SetDnsChallengesPerNameHour(10))
	world.account = world.createAccount(t, newTestTlsCustomDevice(t))
	world.certId = world.queue(t, newTestTlsCustomDevice(t))
	world.desktop = newTestTlsCustomDevice(t)
	world.claim = world.takeClaim(t, world.desktop)
	return world
}

// device's claim for an order of the world's row.
func (self *testTlsCustomChallengeWorld) takeClaim(t testing.TB, device *testTlsCustomDevice) *sidecar.TlsCustomClaimResult {
	t.Helper()
	claim, err := self.store.Claim(self.ctx, self.caller(device, false), &sidecar.TlsCustomClaimArgs{
		Domain:  self.domain,
		Purpose: sidecar.TlsCustomPurposeOrder,
		CertId:  self.certId,
		KeySeq:  self.account.KeySeq,
	})
	if err != nil {
		t.Fatal(err)
	}
	return claim
}

// The DNS-01 value of a key authorization.
func testTlsCustomChallengeValue(keyAuthorization string) string {
	digest := sha256.Sum256([]byte(keyAuthorization))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

// device's capture of value, each command it queued noted.
func (self *testTlsCustomChallengeWorld) capture(device *testTlsCustomDevice, value string) (*TlsCustomChallengeReceipt, error) {
	return self.store.CaptureChallenge(self.ctx, self.caller(device, false), self.domain, value, self.settings, func(tx server.PgTx, challengeId server.Id) {
		self.scheduled = append(self.scheduled, challengeId)
	})
}

func (self *testTlsCustomChallengeWorld) mustCapture(t testing.TB, device *testTlsCustomDevice, value string) *TlsCustomChallengeReceipt {
	t.Helper()
	receipt, err := self.capture(device, value)
	if err != nil {
		t.Fatal(err)
	}
	return receipt
}

func (self *testTlsCustomChallengeWorld) step(t testing.TB, challengeId server.Id) *TlsCustomChallengeStep {
	t.Helper()
	step, err := self.store.ChallengeWorkStep(self.ctx, challengeId, self.settings)
	if err != nil {
		t.Fatal(err)
	}
	return step
}

// The write a step asked for, through §12.6's write path as the dns api makes it for certs.
func (self *testTlsCustomChallengeWorld) write(t testing.TB, step *TlsCustomChallengeStep) uint64 {
	t.Helper()
	if step.WriteName == "" {
		t.Fatalf("no write owed: %+v", step)
	}
	result, err := DnsPutChallenge(self.ctx, &DnsChallengeArgs{Root: self.root, Name: step.WriteName, Values: []string{step.WriteValue}})
	if err != nil {
		t.Fatal(err)
	}
	return result.Serial
}

func (self *testTlsCustomChallengeWorld) receipt(t testing.TB, challengeId server.Id) *TlsCustomChallengeReceipt {
	t.Helper()
	receipt, err := self.store.ChallengeReceipt(self.ctx, challengeId)
	if err != nil {
		t.Fatal(err)
	}
	return receipt
}

// How many journal rows wrote the domain's acme name: what the hourly cap counts.
func (self *testTlsCustomChallengeWorld) journalWriteCount(t testing.TB) int {
	t.Helper()
	match, err := json.Marshal(map[string]any{"upserts": []any{map[string]any{"owner": dns.CanonicalName(self.account.AcmeName), "type": dns.TypeTXT}}})
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	server.Db(self.ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			self.ctx,
			`SELECT count(*) FROM dns_zone_journal WHERE root = $1 AND network_id IS NULL AND diff @> $2::jsonb`,
			dnsLabelRoot(self.root),
			string(match),
		).Scan(&count))
	})
	return count
}

// What the acme name's TXT holds now.
func (self *testTlsCustomChallengeWorld) txt(t testing.TB) []string {
	t.Helper()
	rdata := []string{}
	server.Db(self.ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			self.ctx,
			`SELECT rdata FROM network_dns_record WHERE root = $1 AND owner = $2 AND rrtype = $3`,
			dnsLabelRoot(self.root),
			dns.CanonicalName(self.account.AcmeName),
			int32(dns.TypeTXT),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				server.Raise(result.Scan(&rdata))
			}
		})
	})
	return rdata
}

// The answer certs' DNS client gives for a request whose answer never came.
var errTestTlsCustomChallengeLost = errors.New("dns acme-challenge: connection reset")

// A capture under the claim owes one write, queued with it; the worker's write takes a serial, which the receipt
// answers; a retry under the claim with the same digest recovers it with nothing queued or written again.
func TestTlsCustomChallengeWritesOnceAndRecoversTheReceipt(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestTlsCustomChallengeWorld(t, ctx)
		value := testTlsCustomChallengeValue("token1.thumbprint")
		receipt := world.mustCapture(t, world.desktop, value)
		if receipt.State != TlsCustomChallengeStateQueued || receipt.ClaimSeq != world.claim.ClaimSeq || receipt.CertId != server.Id(world.certId) ||
			receipt.SidecarId != world.desktop.sidecarId || len(world.scheduled) != 1 || world.scheduled[0] != receipt.ChallengeId {
			t.Fatalf("the capture: %+v, queued %v", receipt, world.scheduled)
		}
		step := world.step(t, receipt.ChallengeId)
		if step.WriteName != world.account.AcmeName || step.WriteValue != value || step.DispatchCount != 1 {
			t.Fatalf("the step: %+v, want a write at %s", step, world.account.AcmeName)
		}
		serial := world.write(t, step)
		if err := world.store.RecordChallengeWrite(ctx, receipt.ChallengeId, step.DispatchCount, serial, nil); err != nil {
			t.Fatal(err)
		}
		written := world.receipt(t, receipt.ChallengeId)
		if written.State != TlsCustomChallengeStateWritten || written.Serial != serial {
			t.Fatalf("written: %+v, want serial %d", written, serial)
		}
		if txt := world.txt(t); len(txt) != 1 || txt[0] != `"`+value+`"` {
			t.Fatalf("the name holds %v", txt)
		}
		// the retry: the same serial, nothing queued, nothing written
		retried := world.mustCapture(t, world.desktop, value)
		if retried.ChallengeId != receipt.ChallengeId || retried.State != TlsCustomChallengeStateWritten || retried.Serial != serial || len(world.scheduled) != 1 {
			t.Fatalf("the retry: %+v, queued %v", retried, world.scheduled)
		}
		if step := world.step(t, receipt.ChallengeId); *step != (TlsCustomChallengeStep{}) {
			t.Fatalf("a written receipt owes %+v", step)
		}
		if count := world.journalWriteCount(t); count != 1 {
			t.Fatalf("%d writes, want 1", count)
		}
		bySerial, err := world.store.ChallengeReceiptBySerial(ctx, world.caller(world.desktop, false), serial)
		if err != nil || bySerial == nil || bySerial.ChallengeId != receipt.ChallengeId {
			t.Fatalf("by serial: %+v, %v", bySerial, err)
		}
		// another sidecar of the network reads no receipt of the caller's
		if other, err := world.store.ChallengeReceiptBySerial(ctx, world.caller(newTestTlsCustomDevice(t), false), serial); other != nil || err != nil {
			t.Fatalf("another sidecar read %+v, %v", other, err)
		}
	})
}

// A write whose answer was lost, its commit made, is settled from the journal with its serial and charged once: at a
// cap of one write an hour a second write would be refused, and the receipt still ends written.
func TestTlsCustomChallengeSettlesALostAcknowledgement(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestTlsCustomChallengeWorld(t, ctx)
		defer Testing_SetDnsChallengesPerNameHour(1)()
		value := testTlsCustomChallengeValue("token2.thumbprint")
		receipt := world.mustCapture(t, world.desktop, value)
		step := world.step(t, receipt.ChallengeId)
		serial := world.write(t, step)
		// the dns api committed, and certs never heard
		if err := world.store.RecordChallengeWrite(ctx, receipt.ChallengeId, step.DispatchCount, 0, errTestTlsCustomChallengeLost); err != nil {
			t.Fatal(err)
		}
		if unheard := world.receipt(t, receipt.ChallengeId); unheard.State != TlsCustomChallengeStateDispatched {
			t.Fatalf("an unheard write: %+v", unheard)
		}
		if settled := world.step(t, receipt.ChallengeId); *settled != (TlsCustomChallengeStep{}) {
			t.Fatalf("the settle owes %+v", settled)
		}
		written := world.receipt(t, receipt.ChallengeId)
		if written.State != TlsCustomChallengeStateWritten || written.Serial != serial {
			t.Fatalf("settled: %+v, want serial %d", written, serial)
		}
		if count := world.journalWriteCount(t); count != 1 {
			t.Fatalf("%d writes, want 1", count)
		}
		// what a second charge would have met
		if _, err := DnsPutChallenge(ctx, &DnsChallengeArgs{Root: world.root, Name: step.WriteName, Values: []string{value}}); !errors.Is(err, ErrDnsChallengeRate) {
			t.Fatalf("a second write at a cap of one: %v", err)
		}
	})
}

// A dispatch never made is not read as unmade before SettleAfter has passed, since its request may still commit;
// past it, the write is owed again under a new dispatch, the first never charged; DispatchMaxCount unmade
// dispatches refuse the receipt.
func TestTlsCustomChallengeOwesAnUnmadeWriteOnlyPastSettleAfter(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestTlsCustomChallengeWorld(t, ctx)
		world.settings.DispatchMaxCount = 2
		value := testTlsCustomChallengeValue("token3.thumbprint")
		receipt := world.mustCapture(t, world.desktop, value)
		dispatchTime := world.now
		first := world.step(t, receipt.ChallengeId)
		if err := world.store.RecordChallengeWrite(ctx, receipt.ChallengeId, first.DispatchCount, 0, errTestTlsCustomChallengeLost); err != nil {
			t.Fatal(err)
		}
		world.now = world.now.Add(world.settings.SettleAfter / 2)
		if waiting := world.step(t, receipt.ChallengeId); !waiting.WaitUntil.Equal(dispatchTime.Add(world.settings.SettleAfter)) || waiting.WriteName != "" {
			t.Fatalf("inside SettleAfter: %+v", waiting)
		}
		world.now = dispatchTime.Add(world.settings.SettleAfter)
		second := world.step(t, receipt.ChallengeId)
		if second.WriteName == "" || second.DispatchCount != 2 {
			t.Fatalf("past SettleAfter: %+v", second)
		}
		// an answer for the first dispatch, come late, changes nothing
		if err := world.store.RecordChallengeWrite(ctx, receipt.ChallengeId, first.DispatchCount, 7, nil); err != nil {
			t.Fatal(err)
		}
		if late := world.receipt(t, receipt.ChallengeId); late.State != TlsCustomChallengeStateDispatched {
			t.Fatalf("a late answer moved the receipt: %+v", late)
		}
		serial := world.write(t, second)
		if err := world.store.RecordChallengeWrite(ctx, receipt.ChallengeId, second.DispatchCount, serial, nil); err != nil {
			t.Fatal(err)
		}
		if written := world.receipt(t, receipt.ChallengeId); written.State != TlsCustomChallengeStateWritten || written.Serial != serial {
			t.Fatalf("written: %+v", written)
		}
		if count := world.journalWriteCount(t); count != 1 {
			t.Fatalf("%d writes, want 1", count)
		}

		// another digest, a new order's under the claim, unmade twice
		other := world.mustCapture(t, world.desktop, testTlsCustomChallengeValue("token4.thumbprint"))
		for dispatch := 1; dispatch <= world.settings.DispatchMaxCount; dispatch += 1 {
			step := world.step(t, other.ChallengeId)
			if step.DispatchCount != dispatch {
				t.Fatalf("dispatch %d: %+v", dispatch, step)
			}
			if err := world.store.RecordChallengeWrite(ctx, other.ChallengeId, step.DispatchCount, 0, errTestTlsCustomChallengeLost); err != nil {
				t.Fatal(err)
			}
			world.now = world.now.Add(world.settings.SettleAfter)
		}
		if step := world.step(t, other.ChallengeId); *step != (TlsCustomChallengeStep{}) {
			t.Fatalf("past DispatchMaxCount: %+v", step)
		}
		if refused := world.receipt(t, other.ChallengeId); refused.State != TlsCustomChallengeStateRefused || refused.Refusal != TlsCustomChallengeRefusalAttempts {
			t.Fatalf("past DispatchMaxCount: %+v", refused)
		}
	})
}

// GET's readiness read is the worker's, recorded on the receipt; a stale answer is final for the serial, and the next
// capture of the same digest owes a new write at a new serial, which the old serial no longer reads.
func TestTlsCustomChallengeStaleVerdictOwesANewWrite(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestTlsCustomChallengeWorld(t, ctx)
		value := testTlsCustomChallengeValue("token5.thumbprint")
		receipt := world.mustCapture(t, world.desktop, value)
		step := world.step(t, receipt.ChallengeId)
		first := world.write(t, step)
		if err := world.store.RecordChallengeWrite(ctx, receipt.ChallengeId, step.DispatchCount, first, nil); err != nil {
			t.Fatal(err)
		}
		ask := func(serial uint64, answer string) {
			t.Helper()
			scheduledCount := len(world.scheduled)
			requestTime, err := world.store.RequestChallengeReadiness(ctx, receipt.ChallengeId, serial, func(tx server.PgTx, challengeId server.Id) {
				world.scheduled = append(world.scheduled, challengeId)
			})
			if err != nil || requestTime.IsZero() || len(world.scheduled) != scheduledCount+1 {
				t.Fatalf("the ask: %v, %v", requestTime, err)
			}
			if step := world.step(t, receipt.ChallengeId); step.ReadySerial != serial {
				t.Fatalf("the read owed: %+v, want %d", step, serial)
			}
			if err := world.store.RecordChallengeReadiness(ctx, receipt.ChallengeId, serial, answer); err != nil {
				t.Fatal(err)
			}
			if step := world.step(t, receipt.ChallengeId); *step != (TlsCustomChallengeStep{}) {
				t.Fatalf("an answered read owes %+v", step)
			}
		}
		ask(first, "ready")
		if again := world.mustCapture(t, world.desktop, value); again.State != TlsCustomChallengeStateWritten || again.Serial != first || again.ReadyAnswer != "ready" {
			t.Fatalf("a retry after ready: %+v", again)
		}
		world.now = world.now.Add(time.Second)
		ask(first, TlsCustomChallengeReadyStale)
		// stale is final for the serial: nothing more is asked
		if requestTime, err := world.store.RequestChallengeReadiness(ctx, receipt.ChallengeId, first, func(tx server.PgTx, challengeId server.Id) {
			t.Fatal("a stale serial queued a read")
		}); err != nil || !requestTime.IsZero() {
			t.Fatalf("an ask after stale: %v, %v", requestTime, err)
		}
		requeued := world.mustCapture(t, world.desktop, value)
		if requeued.ChallengeId != receipt.ChallengeId || requeued.State != TlsCustomChallengeStateQueued || requeued.Serial != 0 {
			t.Fatalf("the capture after stale: %+v", requeued)
		}
		step = world.step(t, receipt.ChallengeId)
		if step.WriteValue != value || step.DispatchCount != 1 {
			t.Fatalf("the rewrite: %+v", step)
		}
		second := world.write(t, step)
		if err := world.store.RecordChallengeWrite(ctx, receipt.ChallengeId, step.DispatchCount, second, nil); err != nil {
			t.Fatal(err)
		}
		if second <= first {
			t.Fatalf("the rewrite took %d after %d", second, first)
		}
		if count := world.journalWriteCount(t); count != 2 {
			t.Fatalf("%d writes, want 2", count)
		}
		caller := world.caller(world.desktop, false)
		if old, err := world.store.ChallengeReceiptBySerial(ctx, caller, first); old != nil || err != nil {
			t.Fatalf("the stale serial still reads %+v, %v", old, err)
		}
		if current, err := world.store.ChallengeReceiptBySerial(ctx, caller, second); current == nil || err != nil || current.ReadyAnswer != "" {
			t.Fatalf("the new serial: %+v, %v", current, err)
		}
	})
}

// A desktop that takes the claim over after the capture and before the dispatch: the worker refuses the old claim's
// receipt with nothing written, the old holder's next capture is refused, and the new holder's capture writes.
func TestTlsCustomChallengeRefusesANewerClaimBeforeDispatch(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestTlsCustomChallengeWorld(t, ctx)
		value := testTlsCustomChallengeValue("token6.thumbprint")
		receipt := world.mustCapture(t, world.desktop, value)
		// the holder's lease lapses, and another desktop takes the claim over
		world.now = world.now.Add(3 * time.Minute)
		other := newTestTlsCustomDevice(t)
		takeover := world.takeClaim(t, other)
		if takeover.ClaimSeq != world.claim.ClaimSeq+1 {
			t.Fatalf("the takeover: %+v", takeover)
		}
		if step := world.step(t, receipt.ChallengeId); *step != (TlsCustomChallengeStep{}) {
			t.Fatalf("the old claim's step: %+v", step)
		}
		if refused := world.receipt(t, receipt.ChallengeId); refused.State != TlsCustomChallengeStateRefused || refused.Refusal != TlsCustomChallengeRefusalClaim {
			t.Fatalf("the old claim's receipt: %+v", refused)
		}
		if count := world.journalWriteCount(t); count != 0 {
			t.Fatalf("%d writes for a refused claim", count)
		}
		if _, err := world.capture(world.desktop, value); testTlsCustomRefused(t, err).Refusal.Reason != sidecar.TlsCustomRefusalClaim {
			t.Fatalf("the old holder's capture: %v", err)
		}
		newer := world.mustCapture(t, other, value)
		if newer.ChallengeId == receipt.ChallengeId || newer.ClaimSeq != takeover.ClaimSeq {
			t.Fatalf("the new holder's receipt: %+v", newer)
		}
		step := world.step(t, newer.ChallengeId)
		serial := world.write(t, step)
		if err := world.store.RecordChallengeWrite(ctx, newer.ChallengeId, step.DispatchCount, serial, nil); err != nil {
			t.Fatal(err)
		}
		if written := world.receipt(t, newer.ChallengeId); written.State != TlsCustomChallengeStateWritten || written.Serial != serial {
			t.Fatalf("the new holder's write: %+v", written)
		}
		// a claim released before its dispatch refuses it too
		if _, err := world.store.Progress(ctx, world.caller(other, false), &sidecar.TlsCustomProgressArgs{
			Domain:   world.domain,
			ClaimSeq: takeover.ClaimSeq,
			CertId:   world.certId,
			Event:    sidecar.TlsCustomEventRelease,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := world.capture(other, testTlsCustomChallengeValue("token7.thumbprint")); testTlsCustomRefused(t, err).Refusal.Reason != sidecar.TlsCustomRefusalClaim {
			t.Fatalf("a capture after the release: %v", err)
		}
	})
}

// The name's window full: the worker refuses the receipt with the time it frees and charges nothing, and a capture
// keeps that refusal until the window frees.
func TestTlsCustomChallengeFullWindowRefusesWithoutCharging(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestTlsCustomChallengeWorld(t, ctx)
		defer Testing_SetDnsChallengesPerNameHour(1)()
		// an earlier write of the name, this hour, and the clock after it
		if _, err := DnsPutChallenge(ctx, &DnsChallengeArgs{Root: world.root, Name: world.account.AcmeName, Values: []string{testTlsCustomChallengeValue("earlier")}}); err != nil {
			t.Fatal(err)
		}
		world.now = server.NowUtc().Truncate(time.Microsecond)
		value := testTlsCustomChallengeValue("token8.thumbprint")
		receipt := world.mustCapture(t, world.desktop, value)
		if step := world.step(t, receipt.ChallengeId); *step != (TlsCustomChallengeStep{}) {
			t.Fatalf("a full window's step: %+v", step)
		}
		refused := world.receipt(t, receipt.ChallengeId)
		if refused.State != TlsCustomChallengeStateRefused || refused.Refusal != TlsCustomChallengeRefusalBudget || refused.RetryAfter <= 0 ||
			DefaultDnsChallengeSettings().RateWindow < refused.RetryAfter {
			t.Fatalf("a full window: %+v", refused)
		}
		scheduledCount := len(world.scheduled)
		again := world.mustCapture(t, world.desktop, value)
		if again.State != TlsCustomChallengeStateRefused || len(world.scheduled) != scheduledCount {
			t.Fatalf("a capture while the window is full: %+v", again)
		}
		if count := world.journalWriteCount(t); count != 1 {
			t.Fatalf("%d writes, want the earlier one", count)
		}
		// once the window frees, a capture owes the write again
		world.now = world.now.Add(refused.RetryAfter)
		if freed := world.mustCapture(t, world.desktop, value); freed.State != TlsCustomChallengeStateQueued {
			t.Fatalf("a capture once the window freed: %+v", freed)
		}
	})
}

// Refused captures: no orderer, a caller not holding the claim, and another digest while a write is unheard; another
// digest once the write is settled owes a write of it.
func TestTlsCustomChallengeCaptureRefusals(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestTlsCustomChallengeWorld(t, ctx)
		value := testTlsCustomChallengeValue("token9.thumbprint")
		ungranted := world.caller(world.desktop, false)
		ungranted.PublishGranted = func(string) bool { return false }
		_, err := world.store.CaptureChallenge(ctx, ungranted, world.domain, value, world.settings, func(tx server.PgTx, challengeId server.Id) {})
		if refusal := testTlsCustomRefused(t, err); refusal.Status != http.StatusForbidden || refusal.Refusal.Reason != sidecar.TlsCustomRefusalGrant {
			t.Fatalf("no grant: %+v", refusal)
		}
		if _, err := world.capture(newTestTlsCustomDevice(t), value); testTlsCustomRefused(t, err).Refusal.Reason != sidecar.TlsCustomRefusalClaim {
			t.Fatalf("not the holder: %v", err)
		}
		if _, err := world.store.CaptureChallenge(ctx, world.caller(world.desktop, false), "other.example", value, world.settings, func(tx server.PgTx, challengeId server.Id) {}); err == nil {
			t.Fatal("a domain with no account")
		}
		receipt := world.mustCapture(t, world.desktop, value)
		step := world.step(t, receipt.ChallengeId)
		other := testTlsCustomChallengeValue("token10.thumbprint")
		_, err = world.capture(world.desktop, other)
		if refusal := testTlsCustomRefused(t, err); refusal.Status != http.StatusConflict || refusal.Refusal.Reason != sidecar.TlsCustomRefusalInFlight || refusal.Refusal.RetryAfterMs == 0 {
			t.Fatalf("another digest while a write is unheard: %+v", refusal)
		}
		serial := world.write(t, step)
		if err := world.store.RecordChallengeWrite(ctx, receipt.ChallengeId, step.DispatchCount, serial, nil); err != nil {
			t.Fatal(err)
		}
		replaced := world.mustCapture(t, world.desktop, other)
		if replaced.ChallengeId != receipt.ChallengeId || replaced.State != TlsCustomChallengeStateQueued {
			t.Fatalf("another digest after the write: %+v", replaced)
		}
		if step := world.step(t, receipt.ChallengeId); step.WriteValue != other {
			t.Fatalf("the new digest's write: %+v", step)
		}
	})
}

// A receipt goes with its hour: unread past it, owing nothing, deleted by the purge, and a capture under the claim
// starts a new one.
func TestTlsCustomChallengeReceiptGoesWithItsHour(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestTlsCustomChallengeWorld(t, ctx)
		value := testTlsCustomChallengeValue("token11.thumbprint")
		receipt := world.mustCapture(t, world.desktop, value)
		world.now = world.now.Add(world.settings.ReceiptLifetime)
		if gone := world.receipt(t, receipt.ChallengeId); gone != nil {
			t.Fatalf("a receipt past its hour reads %+v", gone)
		}
		if step := world.step(t, receipt.ChallengeId); *step != (TlsCustomChallengeStep{}) {
			t.Fatalf("a receipt past its hour owes %+v", step)
		}
		purgedCount, more, err := PurgeExpiredTlsCustomChallenges(ctx, world.now, world.settings.PurgeBatchSize)
		if err != nil || purgedCount != 1 || more {
			t.Fatalf("the purge: %d, %t, %v", purgedCount, more, err)
		}
		// the claim's lease renewed past the hour, its next capture starts afresh
		world.claim = world.takeClaim(t, world.desktop)
		fresh := world.mustCapture(t, world.desktop, value)
		if fresh.ChallengeId == receipt.ChallengeId || fresh.State != TlsCustomChallengeStateQueued {
			t.Fatalf("a capture after the hour: %+v", fresh)
		}
	})
}
