package main

// The platform listener's join and ladder (SMTP-M9-15, SMTP-M9-16; report_join.go), on sidecartest's store, quota and
// sink, the X-UR-Ref key file through model's own reader: the intake the block builds attributes a feedback loop's
// report replaying a network's send and halts the network, sends a fraud to review, answers attributed role mail as
// the case it opens, and drops a replayed X-UR-Ref under another network's signature. Names are .example and
// addresses RFC 5737's; time is a fake clock.

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/mxedgeservice"
	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/smtpedgeservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The edge's X-UR-Ref key in these tests, and the key file that holds it.
var (
	testJoinXUrRefKey  = &smtpedgeservice.SmtpXUrRefKey{KeyId: 1, Key: bytes.Repeat([]byte{0x5a}, 32)}
	testJoinXUrRefFile = []byte("issue_key_id: 1\nkeys:\n  - {id: 1, key: " + base64.StdEncoding.EncodeToString(testJoinXUrRefKey.Key) + "}\n")
)

// When the edge sent, from the report settings' one sending address.
var (
	testJoinSendTime       = time.Date(2026, time.November, 3, 10, 0, 0, 0, time.UTC)
	testJoinSendingAddress = netip.MustParseAddr("192.0.2.10")
)

// Two networks on the root, network A's send in the log, a feedback loop that DNS holds a key for, and the
// platform's intake given the join and the ladder.
type testJoinWorld struct {
	*testReportWorld
	t        *testing.T
	store    *sidecartest.FakeSmtpEdgeStore
	quota    *sidecartest.FakeQuota
	sink     *sidecartest.FakeSmtpAbuseSink
	clock    *sidecartest.FakeClock
	intake   *platformReportIntake
	networkA sidecar.Id
	networkB sidecar.Id
	keyA     *sidecartest.SmtpDkimFixtureKey
	keyB     *sidecartest.SmtpDkimFixtureKey
	loopKey  *sidecartest.SmtpDkimFixtureKey
	sends    int
}

func newTestJoinWorld(t *testing.T) *testJoinWorld {
	t.Helper()
	world := newTestReportWorld()
	clock := sidecartest.NewFakeClock(world.arrival)
	self := &testJoinWorld{
		testReportWorld: world,
		t:               t,
		store:           sidecartest.NewFakeSmtpEdgeStore(clock, nil),
		quota:           sidecartest.NewFakeQuota(clock, nil, nil),
		sink:            sidecartest.NewFakeSmtpAbuseSink(),
		clock:           clock,
		networkA:        sidecar.NewId(),
		networkB:        sidecar.NewId(),
		keyA:            sidecartest.NewSmtpDkimFixtureEd25519Key("join-a", "aaaaaaaaaaaaaaaaaaaaaaaaaa", "n1.root.example"),
		keyB:            sidecartest.NewSmtpDkimFixtureEd25519Key("join-b", "bbbbbbbbbbbbbbbbbbbbbbbbbb", "n2.root.example"),
		loopKey:         world.senderKey("loop", "loop.example"),
	}
	self.store.PutDkimKey(self.keyA.StoredKey(self.networkA))
	self.store.PutDkimKey(self.keyB.StoredKey(self.networkB))
	// both networks Community's, whose first complaint halts the network (§14.8)
	for _, networkId := range []sidecar.Id{self.networkA, self.networkB} {
		self.quota.SetLimits(connect.Id(networkId), &sidecar.QuotaLimits{Tier: "free", Limits: map[string]int64{}})
	}
	intake, err := newPlatformReportIntake(self.store, testReportIntakeSettings(), world.txt, world.tokens)
	if err != nil {
		t.Fatal(err)
	}
	err = intake.attributeThrough(&platformJoinArgs{
		XUrRefKeys: func(settings *smtpedgeservice.SmtpEdgeSettings) error {
			return model.ApplySmtpXUrRefKeyFile(settings, testJoinXUrRefFile)
		},
		Quota: self.quota,
		Sink:  self.sink,
		Clock: clock,
	})
	if err != nil {
		t.Fatal(err)
	}
	self.intake = intake
	return self
}

// A send of networkId in the log, with messageId, and the X-UR-Ref the edge issued it.
func (self *testJoinWorld) send(networkId sidecar.Id, messageId string) string {
	self.t.Helper()
	self.sends += 1
	send := &smtpedgeservice.SmtpSendRecord{
		SendId:          bytes.Repeat([]byte{byte(self.sends)}, smtpedgeservice.SmtpSendIdByteCount),
		NetworkId:       networkId,
		ClientId:        sidecar.NewId(),
		SendingAddress:  testJoinSendingAddress,
		RecipientDomain: "receiver.example",
		MxHost:          "mx.receiver.example",
		StartTime:       testJoinSendTime,
		EndTime:         testJoinSendTime.Add(30 * time.Second),
		MessageIdHash:   smtpedgeservice.SmtpMessageIdHash(messageId),
		XUrRefKeyId:     testJoinXUrRefKey.KeyId,
		RecipientCount:  1,
		Result:          "sent",
	}
	if err := self.store.InsertSends(context.Background(), []*smtpedgeservice.SmtpSendRecord{send}); err != nil {
		self.t.Fatal(err)
	}
	xUrRef, err := smtpedgeservice.IssueSmtpXUrRef(testJoinXUrRefKey, send.SendId, networkId, smtpedgeservice.SmtpXUrRefDay(testJoinSendTime))
	if err != nil {
		self.t.Fatal(err)
	}
	return xUrRef
}

// The message as the MX received it, from fromDomain with messageId and xUrRef, its Received hop a minute after the
// send from the sending address, signed under key over §14.6's fields.
func (self *testJoinWorld) received(fromDomain string, messageId string, xUrRef string, key *sidecartest.SmtpDkimFixtureKey) []byte {
	received := "Received: from o1.out.report.example ([" + testJoinSendingAddress.String() + "]) by mx.receiver.example; " + testJoinSendTime.Add(time.Minute).Format(time.RFC1123Z)
	message := (&sidecartest.SmtpFixtureMessage{
		FromDomain:  fromDomain,
		MessageId:   messageId,
		XUrRef:      xUrRef,
		Subject:     "a subject",
		Body:        "a body\n",
		Date:        testJoinSendTime,
		ExtraFields: []string{received},
	}).Bytes()
	return sidecartest.SignSmtpDkimFixture(message, key, &sidecartest.SmtpDkimFixtureOptions{
		HeaderRelaxed: true,
		BodyRelaxed:   true,
		Headers:       []string{"from", "to", "subject", "date", "message-id", "x-ur-ref"},
	})
}

// The feedback loop's report of feedbackType about enclosed's headers, signed by the loop.
func (self *testJoinWorld) loopReport(feedbackType string, enclosed []byte) []byte {
	report := sidecartest.BuildSmtpArfReport("loop.example", feedbackType, enclosed, true)
	return sidecartest.SignSmtpDkimFixture(report, self.loopKey, &sidecartest.SmtpDkimFixtureOptions{HeaderRelaxed: true, BodyRelaxed: true})
}

// A person's mail to a role address enclosing a whole message.
func testRoleMailEnclosing(enclosed []byte) []byte {
	return []byte("From: A Person <person@person.example>\r\nTo: abuse@root.example\r\nSubject: spam from your network\r\n" +
		"Content-Type: multipart/mixed; boundary=\"" + sidecartest.SmtpFixtureBoundary + "\"\r\n\r\n" +
		"--" + sidecartest.SmtpFixtureBoundary + "\r\nContent-Type: text/plain\r\n\r\nPlease look at the attached.\r\n" +
		"--" + sidecartest.SmtpFixtureBoundary + "\r\nContent-Type: message/rfc822\r\n\r\n" + string(enclosed) + "\r\n--" + sidecartest.SmtpFixtureBoundary + "--\r\n")
}

// The listener's call for one recipient.
func (self *testJoinWorld) take(recipient string, raw []byte) mxedgeservice.PlatformOutcome {
	self.t.Helper()
	outcome, err := self.accept(self.t, self.intake, recipient, raw)
	if err != nil {
		self.t.Fatalf("%s: %v", recipient, err)
	}
	return outcome
}

// The intake the block builds joins with the real attribution and halts with the real ladder: a loop's abuse report
// replaying network A's send is taken, stored as a high join to A, and halts A, a Community network, on its first
// complaint, where the ladder writes the halt every smtpedge reloads; nothing reaches B.
func TestPlatformIntakeAttributesALoopReportAndHaltsItsNetwork(t *testing.T) {
	w := newTestJoinWorld(t)
	messageId := "<a1@n1.root.example>"
	xUrRef := w.send(w.networkA, messageId)
	if outcome := w.take("root@rpt.report.example", w.loopReport("abuse", w.received("n1.root.example", messageId, xUrRef, w.keyA))); outcome != mxedgeservice.PlatformOutcomeTaken {
		t.Fatalf("outcome %v", outcome)
	}
	complaints := w.store.Complaints()
	if len(complaints) != 1 {
		t.Fatalf("%d complaints", len(complaints))
	}
	complaint := complaints[0]
	if complaint.NetworkId != w.networkA || complaint.JoinResult != string(smtpedgeservice.SmtpConfidenceHigh) || complaint.Type != smtpedgeservice.SmtpComplaintTypeAbuse ||
		complaint.Reporter != "loop.example" || complaint.XUrRef != xUrRef {
		t.Fatalf("complaint %+v", complaint)
	}
	halts := w.store.Halts()
	if len(halts) != 1 || halts[0].NetworkId != w.networkA || halts[0].Label != "" || halts[0].Trigger != smtpedgeservice.SmtpHaltTriggerCommunityComplaint {
		t.Fatalf("halts %+v", halts)
	}
	active, err := w.store.ActiveHalts(context.Background())
	if err != nil || len(active) != 1 || active[0].NetworkId != w.networkA {
		t.Fatalf("the halt the edges reload: %+v, %v", active, err)
	}
	// an abuse complaint is the ladder's alone: no review is asked of §21
	if reports := w.sink.Reports(); len(reports) != 0 {
		t.Fatalf("the sink was sent %+v", reports)
	}
}

// A fraud report halts the network on any tier and asks §21's sink for review, as the loop's class.
func TestPlatformIntakeSendsAFraudToReview(t *testing.T) {
	w := newTestJoinWorld(t)
	messageId := "<f1@n1.root.example>"
	xUrRef := w.send(w.networkA, messageId)
	if outcome := w.take("root@rpt.report.example", w.loopReport("fraud", w.received("n1.root.example", messageId, xUrRef, w.keyA))); outcome != mxedgeservice.PlatformOutcomeTaken {
		t.Fatalf("outcome %v", outcome)
	}
	halts := w.store.Halts()
	if len(halts) != 1 || halts[0].NetworkId != w.networkA || halts[0].Trigger != smtpedgeservice.SmtpHaltTriggerFraud {
		t.Fatalf("halts %+v", halts)
	}
	reports := w.sink.Reports()
	if len(reports) != 1 || reports[0].NetworkId != w.networkA || reports[0].ReporterClass != smtpedgeservice.SmtpReporterClassLoop ||
		reports[0].Category != smtpedgeservice.SmtpComplaintTypeFraud || reports[0].JoinResult != string(smtpedgeservice.SmtpConfidenceHigh) {
		t.Fatalf("the sink was sent %+v", reports)
	}
}

// A report replaying network A's X-UR-Ref under network B's verifying signature, which a recipient on the shared
// addresses could write, is dropped, not demoted (D-SMTP-8): no row, no halt and nothing for the sink; and a report
// from a loop the allowlist does not name is dropped too.
func TestPlatformIntakeDropsAReplayedXUrRef(t *testing.T) {
	w := newTestJoinWorld(t)
	xUrRefA := w.send(w.networkA, "<a2@n1.root.example>")
	w.send(w.networkB, "<b2@n2.root.example>")
	if outcome := w.take("root@rpt.report.example", w.loopReport("abuse", w.received("n2.root.example", "<b2@n2.root.example>", xUrRefA, w.keyB))); outcome != mxedgeservice.PlatformOutcomeTaken {
		t.Fatalf("outcome %v", outcome)
	}
	// an unlisted reporter about a genuine send
	strangerKey := w.senderKey("stranger", "stranger.example")
	stranger := sidecartest.SignSmtpDkimFixture(
		sidecartest.BuildSmtpArfReport("stranger.example", "abuse", w.received("n1.root.example", "<a2@n1.root.example>", xUrRefA, w.keyA), true),
		strangerKey, nil)
	if outcome := w.take("root@rpt.report.example", stranger); outcome != mxedgeservice.PlatformOutcomeTaken {
		t.Fatalf("outcome %v", outcome)
	}
	if len(w.store.Complaints()) != 0 || len(w.store.Halts()) != 0 || len(w.sink.Reports()) != 0 {
		t.Fatalf("a dropped report left %d complaints, %d halts, %d reports", len(w.store.Complaints()), len(w.store.Halts()), len(w.sink.Reports()))
	}
}

// Role mail enclosing a network's message joins to it and opens §21's case, which the intake answers as the case it
// made: the listener takes it, 250, where unattributed role mail is answered unread, 550. Role mail halts nothing.
func TestPlatformIntakeAnswersAttributedRoleMailAsTheCaseItOpens(t *testing.T) {
	w := newTestJoinWorld(t)
	messageId := "<r1@n1.root.example>"
	xUrRef := w.send(w.networkA, messageId)
	enclosed := w.received("n1.root.example", messageId, xUrRef, w.keyA)
	if outcome := w.take("abuse@root.example", testRoleMailEnclosing(enclosed)); outcome != mxedgeservice.PlatformOutcomeTaken {
		t.Fatalf("attributed role mail: %v, want taken", outcome)
	}
	reports := w.sink.Reports()
	if len(reports) != 1 || reports[0].NetworkId != w.networkA || reports[0].ReporterClass != smtpedgeservice.SmtpReporterClassRole {
		t.Fatalf("the sink was sent %+v", reports)
	}
	complaints := w.store.Complaints()
	if len(complaints) != 1 || complaints[0].Type != smtpedgeservice.SmtpComplaintTypeRole || complaints[0].Reporter != "" {
		t.Fatalf("complaints %+v", complaints)
	}
	if halts := w.store.Halts(); len(halts) != 0 {
		t.Fatalf("role mail halted %+v", halts)
	}
	// role mail that names no send stays unread
	if outcome := w.take("postmaster@root.example", testRoleMail("quietnet.root.example")); outcome != mxedgeservice.PlatformOutcomeUnread {
		t.Fatalf("unattributed role mail: %v, want unread", outcome)
	}
}

// The join needs the X-UR-Ref keys, a Quota and §21's sink, and a store that reads the send log and halts: without
// one, or with a key file refused or holding no key, the intake is left as it was, attributing nothing, and the
// error names the part and never a key.
func TestPlatformJoinRefusesWhatItCannotRunOn(t *testing.T) {
	world := newTestReportWorld()
	newIntake := func() *platformReportIntake {
		intake, err := newPlatformReportIntake(sidecartest.NewFakeSmtpEdgeStore(nil, nil), testReportIntakeSettings(), world.txt, world.tokens)
		if err != nil {
			t.Fatal(err)
		}
		return intake
	}
	quota := sidecartest.NewFakeQuota(nil, nil, nil)
	sink := sidecartest.NewFakeSmtpAbuseSink()
	keys := func(file []byte) func(settings *smtpedgeservice.SmtpEdgeSettings) error {
		return func(settings *smtpedgeservice.SmtpEdgeSettings) error {
			return model.ApplySmtpXUrRefKeyFile(settings, file)
		}
	}
	secret := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x33}, 8))
	for _, c := range []struct {
		name string
		args *platformJoinArgs
		want string
	}{
		{name: "no args", args: nil, want: "needs the X-UR-Ref keys"},
		{name: "no quota", args: &platformJoinArgs{XUrRefKeys: keys(testJoinXUrRefFile), Sink: sink}, want: "needs the X-UR-Ref keys"},
		{name: "no sink", args: &platformJoinArgs{XUrRefKeys: keys(testJoinXUrRefFile), Quota: quota}, want: "needs the X-UR-Ref keys"},
		{name: "a short key", args: &platformJoinArgs{XUrRefKeys: keys([]byte("issue_key_id: 1\nkeys:\n  - {id: 1, key: " + secret + "}\n")), Quota: quota, Sink: sink}, want: model.SmtpXUrRefKeyFileName},
		{name: "no key", args: &platformJoinArgs{XUrRefKeys: keys([]byte("issue_key_id: 1\nkeys: []\n")), Quota: quota, Sink: sink}, want: "holds no key"},
	} {
		intake := newIntake()
		err := intake.attributeThrough(c.args)
		if err == nil || !strings.Contains(err.Error(), c.want) || strings.Contains(err.Error(), secret) {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
			continue
		}
		if intake.deps.Join != nil || intake.deps.Halts != nil {
			t.Errorf("%s: the refused join was given", c.name)
		}
		// the intake answers as before: role mail unread
		if outcome, err := world.accept(t, intake, "abuse@root.example", testRoleMail("quietnet.root.example")); err != nil || outcome != mxedgeservice.PlatformOutcomeUnread {
			t.Errorf("%s: role mail after a refused join: %v, %v", c.name, outcome, err)
		}
	}
	// a store that reads no send log
	intake, err := newPlatformReportIntake(testIntakeOnlyStore{store: sidecartest.NewFakeSmtpEdgeStore(nil, nil)}, testReportIntakeSettings(), world.txt, world.tokens)
	if err != nil {
		t.Fatal(err)
	}
	if err := intake.attributeThrough(&platformJoinArgs{XUrRefKeys: keys(testJoinXUrRefFile), Quota: quota, Sink: sink}); err == nil || !strings.Contains(err.Error(), "reads no send log") {
		t.Errorf("a store with no send log: %v", err)
	}
}

// The intake's store alone, with none of the reads a join and a ladder make.
type testIntakeOnlyStore struct {
	store *sidecartest.FakeSmtpEdgeStore
}

func (self testIntakeOnlyStore) InsertSends(ctx context.Context, sends []*smtpedgeservice.SmtpSendRecord) error {
	return self.store.InsertSends(ctx, sends)
}

func (self testIntakeOnlyStore) FindSends(ctx context.Context, key *smtpedgeservice.SmtpJoinKey) ([]*smtpedgeservice.SmtpSendRecord, error) {
	return self.store.FindSends(ctx, key)
}

func (self testIntakeOnlyStore) InsertComplaint(ctx context.Context, complaint *smtpedgeservice.SmtpComplaint) error {
	return self.store.InsertComplaint(ctx, complaint)
}

func (self testIntakeOnlyStore) InsertAggregate(ctx context.Context, report *smtpedgeservice.SmtpAggregateReport) (bool, error) {
	return self.store.InsertAggregate(ctx, report)
}

func (self testIntakeOnlyStore) SetHalt(ctx context.Context, halt *smtpedgeservice.SmtpHalt) error {
	return self.store.SetHalt(ctx, halt)
}

func (self testIntakeOnlyStore) ActiveHalts(ctx context.Context) ([]*smtpedgeservice.SmtpHalt, error) {
	return self.store.ActiveHalts(ctx)
}

// The block's own join: a binary without MG9 gets none and says so; one with MG9 needs the vault's X-UR-Ref key file
// and then gives its intake the join and the ladder, over model's store, with MG9 among the groups its readiness waits
// on.
func TestBlockAttributesOnlyWithMg9AndItsKeyFile(t *testing.T) {
	if groups := reportJoinGroups(); len(groups) != 1 || groups[0].Name != "MG9" || groups[0].Linked() != server.SidecarMg9Linked() {
		t.Fatalf("groups %+v", groups)
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		newIntake := func() *platformReportIntake {
			intake, err := newPlatformReportIntake(model.NewSmtpEdgeStore(nil), testReportIntakeSettings(), newTestReportWorld().txt, nil)
			if err != nil {
				t.Fatal(err)
			}
			return intake
		}
		intake := newIntake()
		end, err := attributeBlockReports(ctx, intake)
		end()
		if !server.SidecarMg9Linked() {
			if err == nil || !strings.Contains(err.Error(), "MG9") || intake.deps.Join != nil {
				t.Fatalf("without MG9: %v", err)
			}
			return
		}
		if !errors.Is(err, model.ErrSmtpXUrRefKeyFileRefused) || !strings.Contains(err.Error(), model.SmtpXUrRefKeyFileName) || intake.deps.Join != nil {
			t.Fatalf("with no key file: %v", err)
		}
		pop := server.Vault.PushSimpleResource(model.SmtpXUrRefKeyFileName, testJoinXUrRefFile)
		defer pop()
		intake = newIntake()
		end, err = attributeBlockReports(ctx, intake)
		defer end()
		if err != nil {
			t.Fatal(err)
		}
		if intake.deps.Join == nil || intake.deps.Halts == nil || intake.deps.Sink == nil {
			t.Fatalf("the block's intake has join %v, ladder %v", intake.deps.Join, intake.deps.Halts)
		}
	})
}
