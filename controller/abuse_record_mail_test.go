package controller

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// §21.10's record and its production mailer (abuse_record_mail.go). Synthetic
// keys generated in the test, overrides of abuse.yml and abuse-mailbox.yml, and
// a recording sender or a local SES; the action's store case needs MG5's
// tables, so it runs in a build with sidecar_pending.

// A synthetic MAC key, as vault's abuse_mailbox_mac_key would be.
var testAbuseRecordMacKey = []byte("synthetic abuse mailbox mac key, test only")

func testAbuseRecord() *AbuseRecord {
	networkId := server.NewId()
	until := time.Date(2026, 11, 9, 12, 30, 0, 250*int(time.Millisecond), time.UTC)
	return &AbuseRecord{
		ActionId:  server.NewId(),
		CaseId:    server.NewId(),
		Category:  model.AbuseCategoryPhishing,
		NetworkId: &networkId,
		Kind:      model.AbuseActionKindHostKill,
		Target:    "shop.root.example",
		Until:     &until,
		Time:      time.Date(2026, 11, 8, 12, 30, 0, 125*int(time.Millisecond), time.UTC),
	}
}

// A record parses back to its fields, with exactly §21.10's keys and the MAC,
// in that order, and nothing of a report.
func TestAbuseRecordRoundTrips(t *testing.T) {
	record := testAbuseRecord()
	body, err := EncodeAbuseRecord(record, testAbuseRecordMacKey)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseAbuseRecord(body, testAbuseRecordMacKey)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.ActionId != record.ActionId || parsed.CaseId != record.CaseId || parsed.Category != record.Category ||
		*parsed.NetworkId != *record.NetworkId || parsed.Kind != record.Kind || parsed.Target != record.Target ||
		!parsed.Until.Equal(*record.Until) || !parsed.Time.Equal(record.Time) {
		t.Fatalf("parsed %+v, want %+v", parsed, record)
	}

	keyValues := map[string]json.RawMessage{}
	if err := json.Unmarshal(body, &keyValues); err != nil {
		t.Fatal(err)
	}
	keys := []string{}
	for key := range keyValues {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	want := slices.Clone(abuseRecordKeys)
	slices.Sort(want)
	if !slices.Equal(keys, want) {
		t.Fatalf("keys %v, want %v", keys, want)
	}
	// the canonical order, MAC last
	order := []int{}
	for _, key := range abuseRecordKeys {
		order = append(order, bytes.Index(body, []byte(`"`+key+`":`)))
	}
	if !slices.IsSorted(order) {
		t.Fatalf("keys out of order in %s", body)
	}
	if !bytes.Contains(body, []byte(`"time":"2026-11-08T12:30:00.125Z"`)) || !bytes.Contains(body, []byte(`"until":"2026-11-09T12:30:00.250Z"`)) {
		t.Fatalf("times are not RFC 3339 UTC to the millisecond: %s", body)
	}
	for _, reporterWord := range []string{"reporter", "reason", "reviewer", "url", "address", "text"} {
		if bytes.Contains(body, []byte(reporterWord)) {
			t.Errorf("the record holds %q: %s", reporterWord, body)
		}
	}
}

// A breaker level has no network, and an action that only a lift ends has no
// until: both keys stay, as null.
func TestAbuseRecordKeepsNullKeys(t *testing.T) {
	record := testAbuseRecord()
	record.NetworkId = nil
	record.Until = nil
	body, err := EncodeAbuseRecord(record, testAbuseRecordMacKey)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte(`"network_id":null`)) || !bytes.Contains(body, []byte(`"until":null`)) {
		t.Fatalf("null keys dropped: %s", body)
	}
	parsed, err := ParseAbuseRecord(body, testAbuseRecordMacKey)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.NetworkId != nil || parsed.Until != nil {
		t.Fatalf("parsed %+v", parsed)
	}
}

// A record no one at URnetwork MACed does not parse: a field changed, the MAC
// gone, another key's MAC, or a form that is not canonical. A blank key MACs and
// verifies nothing.
func TestAbuseRecordMacRefusesForgeries(t *testing.T) {
	record := testAbuseRecord()
	body, err := EncodeAbuseRecord(record, testAbuseRecordMacKey)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		body []byte
		key  []byte
		want error
	}{
		{name: "a changed kind", body: bytes.Replace(body, []byte(`"host_kill"`), []byte(`"lift"`), 1), key: testAbuseRecordMacKey, want: ErrAbuseRecordMac},
		{name: "a changed target", body: bytes.Replace(body, []byte(`shop.root`), []byte(`shoq.root`), 1), key: testAbuseRecordMacKey, want: ErrAbuseRecordMac},
		{name: "another key", body: body, key: []byte("another synthetic key"), want: ErrAbuseRecordMac},
		{name: "no mac", body: []byte(strings.SplitN(string(body), `,"mac"`, 2)[0] + `}`), key: testAbuseRecordMacKey, want: ErrAbuseRecordMac},
		{name: "a time to the second", body: bytes.Replace(body, []byte(`12:30:00.125Z`), []byte(`12:30:00Z`), 1), key: testAbuseRecordMacKey, want: ErrAbuseRecordInvalid},
		{name: "a time with a zone", body: bytes.Replace(body, []byte(`12:30:00.125Z`), []byte(`13:30:00.125+01:00`), 1), key: testAbuseRecordMacKey, want: ErrAbuseRecordInvalid},
		{name: "an extra key", body: bytes.Replace(body, []byte(`{"action_id"`), []byte(`{"reporter":"x","action_id"`), 1), key: testAbuseRecordMacKey, want: ErrAbuseRecordInvalid},
		{name: "a missing key", body: bytes.Replace(body, []byte(`"until":"2026-11-09T12:30:00.250Z",`), nil, 1), key: testAbuseRecordMacKey, want: ErrAbuseRecordInvalid},
		{name: "trailing bytes", body: append(bytes.Clone(body), []byte(` {}`)...), key: testAbuseRecordMacKey, want: ErrAbuseRecordInvalid},
		{name: "a blank key", body: body, key: []byte(" \n"), want: ErrAbuseRecordMacKey},
		{name: "no key", body: body, key: nil, want: ErrAbuseRecordMacKey},
	} {
		if parsed, err := ParseAbuseRecord(c.body, c.key); !errors.Is(err, c.want) {
			t.Errorf("%s parses as %+v: %v", c.name, parsed, err)
		}
	}
	if _, err := EncodeAbuseRecord(record, nil); !errors.Is(err, ErrAbuseRecordMacKey) {
		t.Errorf("a record MACs under no key: %v", err)
	}
}

// The abuse.yml naming a mailbox, as the tests' environment would.
const testAbuseMailboxConfig = "abuse_mailbox: records@mailbox.example\n"

// One mail as the sender was handed it.
type testAbuseMailboxMail struct {
	mailbox  string
	subject  string
	bodyText string
}

// Every mail it was handed, in order. err, when set, fails each after
// recording it.
type testAbuseMailboxSender struct {
	stateLock sync.Mutex
	mails     []testAbuseMailboxMail
	err       error
}

func (self *testAbuseMailboxSender) sendAbuseMailboxMail(ctx context.Context, mailbox string, subject string, bodyText string) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.mails = append(self.mails, testAbuseMailboxMail{
		mailbox:  mailbox,
		subject:  subject,
		bodyText: bodyText,
	})
	return self.err
}

func (self *testAbuseMailboxSender) sent() []testAbuseMailboxMail {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]testAbuseMailboxMail{}, self.mails...)
}

// Pushes abuse.yml and, unless keys is nil, abuse-mailbox.yml for the rest of
// the test.
func pushTestAbuseMailbox(t testing.TB, abuseYaml string, keys *AbuseMailboxKeys) {
	t.Helper()
	t.Cleanup(server.Testing_PushAbuseConfig(abuseYaml))
	if keys != nil {
		t.Cleanup(server.Vault.PushSimpleResource(AbuseMailboxKeysResourceName, AbuseMailboxKeyFile(keys)))
	}
}

// Fails the test if any byte of record outside its seal is in text: the record
// as mailed before the seal, in the clear or in base64, the MAC, or any field.
func requireNoUnsealedAbuseRecordByte(t testing.TB, text []byte, record *AbuseRecord, plaintext []byte) {
	t.Helper()
	encodedPlaintext := base64.StdEncoding.EncodeToString(plaintext)
	// a base64 window of the record shows at one of three alignments
	needles := []string{string(plaintext), encodedPlaintext[4 : 4+48]}
	for _, offset := range []int{1, 2} {
		encoded := base64.StdEncoding.EncodeToString(plaintext[offset:])
		needles = append(needles, encoded[4:4+48])
	}
	mac := string(plaintext[bytes.LastIndex(plaintext, []byte(`"mac":"`))+len(`"mac":"`) : len(plaintext)-2])
	needles = append(needles,
		mac[:16],
		record.ActionId.String(),
		record.CaseId.String(),
		record.Target,
		string(record.Category),
		string(record.Kind),
		record.Time.UTC().Format(abuseRecordTimeLayout),
		`"action_id"`,
		`"mac"`,
	)
	if record.NetworkId != nil {
		needles = append(needles, record.NetworkId.String())
	}
	// and with line breaks gone, as wrapped base64 of the record would show
	unwrapped := []byte(strings.Join(strings.Fields(string(text)), ""))
	for _, needle := range needles {
		if bytes.Contains(text, []byte(needle)) || bytes.Contains(unwrapped, []byte(needle)) {
			t.Fatalf("an unsealed part of the record (%q) reached the sender", needle)
		}
	}
}

// With no mailbox the production mailer refuses as unavailable and hands the
// sender nothing, so the record stays owed.
func TestProductionAbuseRecordMailerRefusesWithoutAMailbox(t *testing.T) {
	_, keys := newTestAbuseMailboxKeys(t)
	pushTestAbuseMailbox(t, "", keys)
	sender := &testAbuseMailboxSender{}
	mailer := &sesAbuseRecordMailer{sender: sender}
	if err := mailer.MailAbuseRecord(context.Background(), testAbuseRecord()); !errors.Is(err, ErrAbuseRecordMailerUnavailable) {
		t.Fatalf("the mailer answered %v", err)
	}
	if len(sender.sent()) != 0 {
		t.Fatal("a mail went with no mailbox")
	}
}

// With no key file, or one it refuses, the production mailer refuses as
// unavailable and hands the sender nothing, so the record stays owed.
func TestProductionAbuseRecordMailerRefusesWithoutTheKeyFile(t *testing.T) {
	pushTestAbuseMailbox(t, testAbuseMailboxConfig, nil)
	sender := &testAbuseMailboxSender{}
	mailer := &sesAbuseRecordMailer{sender: sender}
	if err := mailer.MailAbuseRecord(context.Background(), testAbuseRecord()); !errors.Is(err, ErrAbuseRecordMailerUnavailable) || !errors.Is(err, ErrAbuseMailboxKeys) {
		t.Fatalf("with no key file the mailer answered %v", err)
	}

	privateKey, keys := newTestAbuseMailboxKeys(t)
	privateKeyBytes, err := privateKey.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	withPrivateKey := append(AbuseMailboxKeyFile(keys), []byte("abuse_mailbox_seal_private_key: "+base64.StdEncoding.EncodeToString(privateKeyBytes)+"\n")...)
	func() {
		defer server.Vault.PushSimpleResource(AbuseMailboxKeysResourceName, withPrivateKey)()
		if err := mailer.MailAbuseRecord(context.Background(), testAbuseRecord()); !errors.Is(err, ErrAbuseRecordMailerUnavailable) || !errors.Is(err, ErrAbuseMailboxKeys) {
			t.Fatalf("with the private key in the file the mailer answered %v", err)
		}
	}()
	if len(sender.sent()) != 0 {
		t.Fatal("a mail went with no good key file")
	}
}

// The production mailer hands the sender one mail to the mailbox, under the
// fixed subject, whose body opens with the private key to the record MACed
// under the file's key.
func TestProductionAbuseRecordMailerSealsEachRecord(t *testing.T) {
	privateKey, keys := newTestAbuseMailboxKeys(t)
	pushTestAbuseMailbox(t, testAbuseMailboxConfig, keys)
	sender := &testAbuseMailboxSender{}
	mailer := &sesAbuseRecordMailer{sender: sender}
	record := testAbuseRecord()
	if err := mailer.MailAbuseRecord(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	mails := sender.sent()
	if len(mails) != 1 || mails[0].mailbox != "records@mailbox.example" || mails[0].subject != abuseMailboxMailSubject {
		t.Fatalf("mails %+v", mails)
	}
	settings := DefaultAbuseMailboxSettings()
	sealed, err := DecodeAbuseMailboxBody(mails[0].bodyText, settings)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := OpenAbuseMailboxRecord(privateKey, sealed, settings)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseAbuseRecord(opened, keys.MacKey)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.ActionId != record.ActionId || parsed.CaseId != record.CaseId || parsed.Kind != record.Kind ||
		parsed.Target != record.Target || *parsed.NetworkId != *record.NetworkId || !parsed.Time.Equal(record.Time) {
		t.Fatalf("parsed %+v, want %+v", parsed, record)
	}
}

// No unsealed byte reaches the sender: its subject is fixed, its body is
// base64 of exactly the sealed bytes, and neither holds the record, its MAC or
// any field, in the clear or in base64; the decoded body holds none either.
func TestProductionAbuseRecordMailerSendsNoUnsealedByte(t *testing.T) {
	privateKey, keys := newTestAbuseMailboxKeys(t)
	pushTestAbuseMailbox(t, testAbuseMailboxConfig, keys)
	settings := DefaultAbuseMailboxSettings()
	for _, record := range []*AbuseRecord{
		testAbuseRecord(),
		func() *AbuseRecord {
			record := testAbuseRecord()
			record.NetworkId = nil
			record.Until = nil
			return record
		}(),
	} {
		sender := &testAbuseMailboxSender{}
		mailer := &sesAbuseRecordMailer{sender: sender}
		if err := mailer.MailAbuseRecord(context.Background(), record); err != nil {
			t.Fatal(err)
		}
		mails := sender.sent()
		if len(mails) != 1 {
			t.Fatalf("%d mails", len(mails))
		}
		plaintext, err := EncodeAbuseRecord(record, keys.MacKey)
		if err != nil {
			t.Fatal(err)
		}
		mail := mails[0]
		requireNoUnsealedAbuseRecordByte(t, []byte(mail.subject+"\n"+mail.mailbox+"\n"+mail.bodyText), record, plaintext)

		sealed, err := DecodeAbuseMailboxBody(mail.bodyText, settings)
		if err != nil {
			t.Fatal(err)
		}
		if len(sealed) != 1+1120+len(plaintext)+16 {
			t.Fatalf("the body holds %d bytes, not one sealed record of %d", len(sealed), len(plaintext))
		}
		requireNoUnsealedAbuseRecordByte(t, sealed, record, plaintext)
		if mail.bodyText != EncodeAbuseMailboxBody(sealed, settings) {
			t.Fatal("the body holds more than the sealed bytes in base64")
		}
		if opened, err := OpenAbuseMailboxRecord(privateKey, sealed, settings); err != nil || !bytes.Equal(opened, plaintext) {
			t.Fatalf("the body opens as %q: %v", opened, err)
		}
	}
}

// A send that fails is this record's failure, not the mailer's, so the pass
// goes on to the next owed record; a sender with no email.yml is unavailable.
func TestProductionAbuseRecordMailerLeavesAFailedSendOwed(t *testing.T) {
	_, keys := newTestAbuseMailboxKeys(t)
	pushTestAbuseMailbox(t, testAbuseMailboxConfig, keys)
	failure := errors.New("synthetic ses failure")
	sender := &testAbuseMailboxSender{err: failure}
	mailer := &sesAbuseRecordMailer{sender: sender}
	err := mailer.MailAbuseRecord(context.Background(), testAbuseRecord())
	if !errors.Is(err, failure) || errors.Is(err, ErrAbuseRecordMailerUnavailable) {
		t.Fatalf("a failed send answered %v", err)
	}
	sender.err = fmt.Errorf("%w: no email.yml", ErrAbuseMailboxUnavailable)
	if err := mailer.MailAbuseRecord(context.Background(), testAbuseRecord()); !errors.Is(err, ErrAbuseRecordMailerUnavailable) {
		t.Fatalf("a sender with no email.yml answered %v", err)
	}
}

// SES itself is handed the sealed body alone: one text/plain part and no html
// part, under the fixed subject, to the mailbox from email.yml's sender, and
// no form value of the call holds an unsealed byte.
func TestProductionAbuseRecordMailerReachesSesSealed(t *testing.T) {
	endpoint := newAwsEndpoint(t, "text/xml", `<SendEmailResponse xmlns="http://ses.amazonaws.com/doc/2010-12-01/">
  <SendEmailResult>
    <MessageId>synthetic-message-id</MessageId>
  </SendEmailResult>
  <ResponseMetadata>
    <RequestId>synthetic-request-id</RequestId>
  </ResponseMetadata>
</SendEmailResponse>`)
	privateKey, keys := newTestAbuseMailboxKeys(t)
	pushTestAbuseMailbox(t, testAbuseMailboxConfig, keys)
	mailer := &sesAbuseRecordMailer{
		sender: &sesAbuseMailboxSender{
			sender:      endpoint.sender(5 * time.Second),
			emailConfig: &EmailConfig{CompanySenderEmail: "sender@synthetic.example"},
		},
	}
	record := testAbuseRecord()
	if err := sendWithGuard(t, func() error {
		return mailer.MailAbuseRecord(context.Background(), record)
	}); err != nil {
		t.Fatal(err)
	}
	request := endpoint.receivedRequest()
	if request == nil {
		t.Fatal("SES saw no call")
	}
	if err := request.ParseForm(); err != nil {
		t.Fatal(err)
	}
	form := request.PostForm
	if form.Get("Action") != "SendEmail" ||
		form.Get("Destination.ToAddresses.member.1") != "records@mailbox.example" ||
		form.Get("Source") != "sender@synthetic.example" ||
		form.Get("Message.Subject.Data") != abuseMailboxMailSubject {
		t.Fatalf("SES call %v", form)
	}
	for key := range form {
		if strings.HasPrefix(key, "Message.Body.Html") {
			t.Fatalf("SES was handed an html part: %s", key)
		}
	}
	plaintext, err := EncodeAbuseRecord(record, keys.MacKey)
	if err != nil {
		t.Fatal(err)
	}
	all := &bytes.Buffer{}
	for key, values := range form {
		all.WriteString(key + "=" + strings.Join(values, ",") + "\n")
	}
	requireNoUnsealedAbuseRecordByte(t, all.Bytes(), record, plaintext)

	settings := DefaultAbuseMailboxSettings()
	sealed, err := DecodeAbuseMailboxBody(form.Get("Message.Body.Text.Data"), settings)
	if err != nil {
		t.Fatal(err)
	}
	if opened, err := OpenAbuseMailboxRecord(privateKey, sealed, settings); err != nil || !bytes.Equal(opened, plaintext) {
		t.Fatalf("SES's body opens as %q: %v", opened, err)
	}
}

// The send ends with the caller's context: a pass that is ending stops a send
// SES is holding.
func TestAbuseMailboxMailEndsWithItsContext(t *testing.T) {
	endpoint := newSilentAwsEndpoint(t)
	sender := &sesAbuseMailboxSender{
		sender:      endpoint.sender(time.Minute),
		emailConfig: &EmailConfig{CompanySenderEmail: "sender@synthetic.example"},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- sender.sendAbuseMailboxMail(ctx, "records@mailbox.example", abuseMailboxMailSubject, "AAAA\n")
	}()
	// SES holds the call; then the caller ends
	select {
	case <-endpoint.received:
	case <-time.After(awsSendTimeoutTestGuard):
		t.Fatal("SES saw no call")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("the send answered %v", err)
		}
	case <-time.After(awsSendTimeoutTestGuard):
		t.Fatal("the send outlived its context")
	}
	if err := (&sesAbuseMailboxSender{sender: endpoint.sender(time.Minute), emailConfig: &EmailConfig{}}).sendAbuseMailboxMail(
		context.Background(), "records@mailbox.example", abuseMailboxMailSubject, "AAAA\n",
	); !errors.Is(err, ErrAbuseMailboxUnavailable) {
		t.Fatalf("a sender with no address answered %v", err)
	}
}

// A process mails records only once it installs the production mailer, which
// api's and taskworker's startup alone do: before, the process's mailer
// refuses each record as unavailable.
func TestAbuseRecordMailerIsInstalledByApiAndTaskworkerAlone(t *testing.T) {
	_, mailer, _ := currentAbuseActionDeps()
	if err := mailer.MailAbuseRecord(context.Background(), testAbuseRecord()); !errors.Is(err, ErrAbuseRecordMailerUnavailable) {
		t.Fatalf("a process that installed nothing mails: %v", err)
	}
	restore := InstallAbuseRecordMailer()
	_, installed, _ := currentAbuseActionDeps()
	restore()
	if _, ok := installed.(*sesAbuseRecordMailer); ok != server.SidecarMg5Linked() {
		t.Fatalf("installed %T in a binary that links MG5: %t", installed, server.SidecarMg5Linked())
	}

	// the module's callers of the install: exactly the two mains
	moduleRoot, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	callers := []string{}
	err = filepath.WalkDir(moduleRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if path != moduleRoot {
				if _, statErr := os.Stat(filepath.Join(path, "go.mod")); statErr == nil {
					return filepath.SkipDir
				}
			}
			switch entry.Name() {
			case "testdata", "node_modules", ".git":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(bytes.ReplaceAll(source, []byte("func InstallAbuseRecordMailer()"), nil), []byte("InstallAbuseRecordMailer()")) {
			rel, _ := filepath.Rel(moduleRoot, path)
			callers = append(callers, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(callers)
	if want := []string{"cli/api/main.go", "cli/taskworker/main.go"}; !slices.Equal(callers, want) {
		t.Fatalf("InstallAbuseRecordMailer is called from %v, want %v", callers, want)
	}
}

// An action whose key file is missing stays owed, its record unsent and
// mailed_time NULL, through the request and every pass; once the file is in
// vault the next pass mails it once, sealed, and sets mailed_time, and no later
// pass mails it again.
func TestAbuseActionMailedOnceWhenTheKeyFileArrives(t *testing.T) {
	requireAbuseTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		effects, _ := newTestAbuseDeps(t)
		sender := &testAbuseMailboxSender{}
		t.Cleanup(SetAbuseRecordMailer(&sesAbuseRecordMailer{sender: sender}))
		pushTestAbuseMailbox(t, testAbuseMailboxConfig, nil)
		_, _, caseId := newTestAbuseCase(t, ctx, false)
		args := testAbuseKillArgs(caseId)

		result, err := ApplyAbuseAction(ctx, args)
		if err != nil {
			t.Fatal(err)
		}
		if !result.Applied || result.Mailed {
			t.Fatalf("with no key file the action reads %+v", result)
		}
		if action := model.GetAbuseAction(ctx, args.ActionId); action.MailedTime != nil {
			t.Fatalf("with no key file the row reads mailed: %+v", action)
		}
		if pass := testRunAbuseActionPass(ctx); pass.MailedCount != 0 || pass.OwedCount != 1 {
			t.Fatalf("a pass with no key file %+v", pass)
		}
		if len(sender.sent()) != 0 {
			t.Fatal("a mail went with no key file")
		}

		privateKey, keys := newTestAbuseMailboxKeys(t)
		t.Cleanup(server.Vault.PushSimpleResource(AbuseMailboxKeysResourceName, AbuseMailboxKeyFile(keys)))
		if pass := testRunAbuseActionPass(ctx); pass.MailedCount != 1 || pass.AppliedCount != 0 || pass.OwedCount != 0 {
			t.Fatalf("the pass once the key file is in %+v", pass)
		}
		action := model.GetAbuseAction(ctx, args.ActionId)
		if action.MailedTime == nil {
			t.Fatalf("the mailed row reads %+v", action)
		}
		mails := sender.sent()
		if len(mails) != 1 {
			t.Fatalf("%d mails", len(mails))
		}
		settings := DefaultAbuseMailboxSettings()
		sealed, err := DecodeAbuseMailboxBody(mails[0].bodyText, settings)
		if err != nil {
			t.Fatal(err)
		}
		opened, err := OpenAbuseMailboxRecord(privateKey, sealed, settings)
		if err != nil {
			t.Fatal(err)
		}
		record, err := ParseAbuseRecord(opened, keys.MacKey)
		if err != nil {
			t.Fatal(err)
		}
		if record.ActionId != args.ActionId || record.CaseId != caseId || record.Kind != model.AbuseActionKindHostKill {
			t.Fatalf("the mailed record %+v", record)
		}

		if pass := testRunAbuseActionPass(ctx); pass.MailedCount != 0 || pass.AppliedCount != 0 {
			t.Fatalf("a later pass %+v", pass)
		}
		if len(sender.sent()) != 1 || len(effects.applied()) != 1 {
			t.Fatalf("%d mails and %d effects in all", len(sender.sent()), len(effects.applied()))
		}
		if again := model.GetAbuseAction(ctx, args.ActionId); !again.MailedTime.Equal(*action.MailedTime) {
			t.Fatalf("mailed_time moved from %s to %s", action.MailedTime, again.MailedTime)
		}
	})
}

// The record of an action is its row's fields, a lift's target the lifted id.
func TestAbuseRecordOfAnAction(t *testing.T) {
	networkId := server.NewId()
	liftedId := server.NewId()
	action := &model.AbuseAction{
		ActionId:   server.NewId(),
		CaseId:     server.NewId(),
		NetworkId:  &networkId,
		Category:   model.AbuseCategoryCopyright,
		Kind:       model.AbuseActionKindLift,
		Target:     liftedId.String(),
		CreateTime: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC),
	}
	record := AbuseRecordOf(action)
	if record.ActionId != action.ActionId || record.CaseId != action.CaseId || *record.NetworkId != networkId ||
		record.Kind != model.AbuseActionKindLift || record.Target != liftedId.String() || record.Until != nil ||
		!record.Time.Equal(action.CreateTime) {
		t.Fatalf("record %+v", record)
	}
}
