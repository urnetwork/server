//go:build sidecar_e2e && sidecar_pending && sidecar_localstack

// The smoke's mail step (world_smoke_test.go's header) and a world that runs it alone (TestE2eWorldMail):
//
//   - inbound: the receiving network's owner turns mail on through its MailClient (mailcfg, the member's mailbox,
//     POST /mail/enable) and creates an alias, and its last device runs the network's receiver, which listens on
//     smtp/25/mx through sockets under §16.6's gate and serves once its TLSA, posted through /dns/key-record, is live;
//     the dns edge answers the network's MX, its mail host's address at the world's lb interface (the router
//     interface whose banner the edge's probe passed) and the receiver's TLSA, each signed; an internet sender
//     delivers through the lb to mxedge's router on 25, STARTTLS with the mail host as SNI and the receiver's key held
//     to the TLSA, so the router splices the session to the receiver, which deposits the message in the member's
//     mailbox before its 250; and the owner's device, the member's reader, lists and reads it back, untrusted as
//     outside mail is;
//   - outbound: the sending network, verified by a paid balance and older than §21.7's probation, turns mail on, its
//     enable older than §14.7's 24 h, and its owner's device, the member's prover host (WorldMailSender), sends to the
//     receiving network's alias: the message queues in the member's outbox, the host's runner sends it through
//     smtpedge, which resolves the receiving network's MX through the world's DNSSEC and splices to mxedge's router
//     over the LAN, and the receiving network's reader reads it from its mailbox.

package sidecare2e

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server/sidecart3"
)

// What the smoke's mail step runs with (QD-702).
type worldSmokeMailSettings struct {
	// Each mail network's ACL document's grants for mail: its members listen on smtp/25/mx and on every alias, and
	// send as one (§15.5, §15.8), as JSON grant objects a document's grants list takes.
	AclGrants []string
	// The alias the receiving network's owner creates, and the internet sender's address, subject and body.
	Alias   string
	Sender  string
	Subject string
	Body    string
	// The sending network's alias, and its message's subject and body.
	SendAlias   string
	SendSubject string
	SendBody    string
	// How old the sending network reads, past §21.7's 7-day probation, and how long ago its mail was turned on, past
	// §14.7's 24 h: the world's networks are made new.
	SenderNetworkAge time.Duration
	SenderMailAge    time.Duration
	// The helpers' bounds and the receiver's timing, and the sending device's prover.
	Mail   *WorldMailSettings
	Prover *WorldMailSenderSettings
}

func defaultWorldSmokeMailSettings() *worldSmokeMailSettings {
	return &worldSmokeMailSettings{
		AclGrants: []string{
			`{"src": ["autogroup:member"], "dst": ["mail:*"], "allow": ["listen", "send"]}`,
			`{"src": ["autogroup:member"], "dst": ["sock:smtp/25/mx"], "allow": ["listen"]}`,
		},
		Alias:            "smoke",
		Sender:           "smoke-sender@sender.example",
		Subject:          "The e2e world's inbound smoke",
		Body:             "Mail from the internet through mxedge to a receiver of the network.",
		SendAlias:        "outbound",
		SendSubject:      "The e2e world's outbound smoke",
		SendBody:         "Mail from one network through smtpedge and mxedge to another network's mailbox.",
		SenderNetworkAge: 8 * 24 * time.Hour,
		SenderMailAge:    2 * 24 * time.Hour,
		Mail:             DefaultWorldMailSettings(),
		Prover:           DefaultWorldMailSenderSettings(),
	}
}

// One network's mail as the inbound step left it: its alias's address, and the reader whose mailbox takes it.
type worldSmokeMailbox struct {
	Network   *WorldNetwork
	Address   string
	Reader    *sidecart3.T3Device
	Mail      *sidecar.MailClient
	MailboxId sidecar.Id
}

// The ACL document of grants, each a JSON grant object.
func worldAclPolicy(grants ...string) string {
	return `{"grants": [` + strings.Join(grants, ", ") + `]}`
}

// The smoke's mail step on its own world: two networks of two sidecars, the first receiving and the second, verified
// by a paid balance, sending to it.
func TestE2eWorldMail(t *testing.T) {
	settings := defaultWorldSmokeSettings()
	e2eTestEnv().Run(t, func(t testing.TB) {
		worldSettings := DefaultWorldSettings()
		worldSettings.NetworkCount = 2
		worldSettings.ClientsPerNetwork = 2
		world := NewWorld(t, worldSettings)
		defer world.Close()
		ctx, cancel := world.StepContext(t.Context(), settings.PhaseTimeout)
		networks, err := world.AddNetworks(ctx, nil)
		if err != nil {
			cancel()
			world.FailProgress(t, "the enrollment", err, nil)
		}
		receiving, sending := networks[0], networks[1]
		// the sender's standing is read and cached from its first use, so it is aged before anything reads it
		if err := worldAgeNetwork(ctx, sending.NetworkId, settings.Mail.SenderNetworkAge); err != nil {
			cancel()
			t.Fatal(err)
		}
		if err := world.VerifyNetwork(ctx, sending, false, settings.Tls); err != nil {
			cancel()
			t.Fatalf("%s's paid balance: %v", sending.Name, err)
		}
		for _, network := range networks {
			if _, err := world.SaveAclPolicy(ctx, network, network.Owner(), worldAclPolicy(settings.Mail.AclGrants...)); err != nil {
				cancel()
				world.FailProgress(t, network.Name+"'s mail grants", err, nil)
			}
		}
		cancel()
		mailbox := worldSmokeMailInbound(t, world, settings, receiving, settings.Mail.AclGrants)
		worldSmokeMailOutbound(t, world, settings, sending, mailbox)
	})
}

// The smoke's inbound mail (the file's header): network turns mail on and receives a message from the internet
// through the world's lb and mxedge's router into its member's mailbox. network's document must already hold the
// step's AclGrants, and grants is the whole of its grants, which a save the step makes keeps.
func worldSmokeMailInbound(t testing.TB, world *World, settings *worldSmokeSettings, network *WorldNetwork, grants []string) *worldSmokeMailbox {
	mailSettings := settings.Mail
	ctx, cancel := world.StepContext(t.Context(), settings.PhaseTimeout)
	defer cancel()
	owner := network.Owner()
	receiver := network.Clients[len(network.Clients)-1]
	if receiver == owner {
		t.Fatalf("%s has one sidecar; the step's receiver is a device of the owner's other than the reader", network.Name)
	}
	ownerGate, err := world.InstallDeviceGate(ctx, owner, sidecar.SidecarActionAliasCreate)
	if err != nil {
		t.Fatal(err)
	}
	receiverGate, err := world.InstallDeviceGate(ctx, receiver)
	if err != nil {
		t.Fatal(err)
	}
	// the owner's device reads the member's mailbox and runs no receiver; mail on first, then the alias, so the
	// receiver's first read of mailcfg holds it
	ownerMail, err := world.StartMail(ctx, owner, false, mailSettings.Mail)
	if err != nil {
		t.Fatal(err)
	}
	enabled, err := world.EnableMail(ctx, owner, ownerMail, mailSettings.Mail)
	if err != nil {
		world.FailProgress(t, network.Name+"'s mail", err, nil)
	}
	if enabled.EnabledTime.IsZero() {
		t.Fatalf("%s's mail enable answered no time: %+v", network.Name, enabled)
	}
	dnsClient, err := owner.Sidecar().Dns()
	if err != nil {
		t.Fatalf("%s's dns client: %v", owner.Name, err)
	}
	zone, err := dnsClient.Zone(ctx)
	if err != nil || zone.Apex() == "" {
		t.Fatalf("%s's zone: %+v, %v", network.Name, zone, err)
	}
	domain := strings.TrimSuffix(zone.Apex(), ".")
	address := mailSettings.Alias + "@" + domain
	if _, err := ownerMail.CreateAlias(ctx, &sidecar.MailAliasCreateArgs{LocalPart: mailSettings.Alias}); err != nil {
		world.FailProgress(t, network.Name+"'s alias", err, nil)
	}
	if approved := ownerGate.ApprovedCount(); approved != 1 {
		t.Fatalf("%s's alias passed %d stand-in approvals, not 1", owner.Name, approved)
	}
	receiverMail, err := world.StartMail(ctx, receiver, true, mailSettings.Mail)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := world.AwaitMailReceiver(ctx, receiver, receiverMail, mailSettings.Mail); err != nil {
		world.FailProgress(t, network.Name+"'s receiver", err, nil)
	}
	if passed := receiverGate.ListenPassedCount(); 0 < passed {
		// the TLS step's finding, met again by the receiver's own listen, which retries until a gate lets it through
		t.Errorf("§16.6's approval gate refused %s's receiver listen on smtp/25/mx %d times as an action with no effect: socketListenAction builds the listen's action with no Effect, so no receiver listens behind the stock gate", receiver.Name, passed)
	}
	// the receiver deposits each message to the member's mailbox on a repo:<id>:deposit token (evs_deposit.go)
	mailboxId, ok := worldMemberMailboxId(receiver)
	if !ok {
		t.Fatalf("%s's verified state holds no mailbox of its member", receiver.Name)
	}
	if access := worldEvsAccess(receiver, mailboxId); !access.Rights.Has(sidecar.AclEvsDeposit) {
		// a product finding, which the step reports and carries on past through a rule a network's admin can write
		t.Errorf("%s, a consented device of its member and %s's receiver, holds %b on the member's mailbox and no deposit, so the exchange refuses the repo:<mailbox>:deposit token every deposit asks and the receiver never answers a message's DATA: EvsAccess gives an owner-bound mailbox's consented devices read and write alone (acl_evs.go's addOwnerBoundRights), the exchange holds the deposit scope to the deposit bit (AclExchangeScopes), and §8.5's deposit for every subject the projection marks mail_send (owner decision 333) is not built", receiver.Name, network.Name, access.Rights)
		deposit := fmt.Sprintf(`{"src": ["autogroup:member"], "dst": ["evs:%s"], "allow": ["deposit"]}`, mailboxId)
		if _, err := world.SaveAclPolicy(ctx, network, owner, worldAclPolicy(append(slices.Clone(grants), deposit)...)); err != nil {
			world.FailProgress(t, network.Name+"'s mailbox deposit rule", err, nil)
		}
		if access := worldEvsAccess(receiver, mailboxId); !access.Rights.Has(sidecar.AclEvsDeposit) {
			t.Fatalf("%s holds %b on the mailbox under a rule granting members deposit on it", receiver.Name, access.Rights)
		}
	}
	key, err := receiver.Sidecar().TlsDeviceKey()
	if err != nil {
		t.Fatalf("%s's TLS key: %v", receiver.Name, err)
	}
	spkiSha256 := key.SpkiSha256()
	mailDns, err := world.AwaitMailDns(ctx, domain, [][]byte{spkiSha256[:]}, nil)
	if err != nil {
		world.FailProgress(t, network.Name+"'s mail records", err, nil)
	}
	if mailDns.MxHost != "mx."+domain {
		t.Fatalf("%s's MX names %s, not its mail host", domain, mailDns.MxHost)
	}
	changes := newWorldRepoChanges(owner, mailboxId)
	defer changes.Close()
	messageId := fmt.Sprintf("%d.smoke@sender.example", time.Now().UnixNano())
	message := worldMailMessageBytes(mailSettings.Sender, address, mailSettings.Subject, messageId, mailSettings.Body)
	delivery := &WorldMailDelivery{Dns: mailDns, From: mailSettings.Sender, To: []string{address}, Message: message}
	if err := world.AwaitMailDelivery(ctx, delivery, mailSettings.Mail); err != nil {
		world.FailProgress(t, "the delivery through mxedge", err, nil)
	}
	got, err := world.AwaitMailMessage(ctx, owner, ownerMail, mailSettings.Subject, changes, mailSettings.Mail)
	if err != nil {
		world.FailProgress(t, network.Name+"'s mailbox", err, nil)
	}
	worldSmokeMailCheck(t, owner, got, mailSettings.Sender, messageId, mailSettings.Body, sidecar.MailTrustUntrusted)
	worldSmokeMailIngestByHint(t, owner, got, changes.Count(), mailSettings.Mail)
	return &worldSmokeMailbox{Network: network, Address: address, Reader: owner, Mail: ownerMail, MailboxId: mailboxId}
}

// The smoke's outbound mail (the file's header): network, verified and aged past probation, turns mail on, its enable
// aged past the send gate's delay, and its owner's device, the member's prover host, sends to mailbox's address:
// MailClient.Send queues the message in the member's outbox, the host's runner takes it, its sender Finds ur/smtpedge
// and opens the stream to smtpedge, whose gate, quota and validating resolver pass it to mxedge's router over the LAN
// splice, and the message, end to end under TLS with the receiver's key held to its TLSA, is deposited in mailbox's
// member's mailbox and read back there. network's document must already hold the step's AclGrants.
func worldSmokeMailOutbound(t testing.TB, world *World, settings *worldSmokeSettings, network *WorldNetwork, mailbox *worldSmokeMailbox) {
	mailSettings := settings.Mail
	ctx, cancel := world.StepContext(t.Context(), settings.PhaseTimeout)
	defer cancel()
	if !world.apiSmtpGateInstalled {
		// a product finding, which the step reports and carries on past through the world's stand-in
		t.Errorf("api's process installs no §8 check for its smtp routes (model.SetSmtpApiGate is called by its handler tests alone), so POST /smtp/dkim-keys answers every publish 503 and no sending device publishes the DKIM selector it signs with; the world installs worldSmtpApiGate in its place")
	}
	owner := network.Owner()
	ownerGate, err := world.InstallDeviceGate(ctx, owner, sidecar.SidecarActionAliasCreate, sidecar.SidecarActionMail)
	if err != nil {
		t.Fatal(err)
	}
	ownerMail, err := world.StartMail(ctx, owner, false, mailSettings.Mail)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := world.EnableMail(ctx, owner, ownerMail, mailSettings.Mail); err != nil {
		world.FailProgress(t, network.Name+"'s mail", err, nil)
	}
	if err := worldAgeMailEnable(ctx, network.NetworkId, mailSettings.SenderMailAge); err != nil {
		t.Fatal(err)
	}
	zone, err := worldDeviceZone(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	domain := strings.TrimSuffix(zone.Apex(), ".")
	from := mailSettings.SendAlias + "@" + domain
	if _, err := ownerMail.CreateAlias(ctx, &sidecar.MailAliasCreateArgs{LocalPart: mailSettings.SendAlias}); err != nil {
		world.FailProgress(t, network.Name+"'s alias", err, nil)
	}
	sender, err := world.StartMailSender(ctx, owner, ownerMail, mailSettings.Prover)
	if err != nil {
		t.Fatal(err)
	}
	changes := newWorldRepoChanges(mailbox.Reader, mailbox.MailboxId)
	defer changes.Close()
	sent, err := ownerMail.Send(ctx, &sidecar.MailSendArgs{
		From:    from,
		To:      []string{mailbox.Address},
		Subject: mailSettings.SendSubject,
		Text:    mailSettings.SendBody,
	})
	if err != nil {
		world.FailProgress(t, network.Name+"'s send", err, nil)
	}
	if len(sent.Recipients) != 1 || sent.Recipients[0].Outcome != sidecar.MailSendQueued {
		t.Fatalf("%s's send to %s: %+v", owner.Name, mailbox.Network.Name, sent.Recipients)
	}
	approvals := ownerGate.ApprovedCount()
	result, err := world.AwaitMailSendResult(ctx, sender, ownerMail, sent.SendId, func(result *sidecar.SmtpSendResult) bool {
		return len(result.Domains) == 1 && result.Domains[0].Outcome != sidecar.SmtpOutcomeQueued && result.Domains[0].Outcome != sidecar.SmtpOutcomeDeferred
	}, mailSettings.Mail)
	if err != nil {
		outcomes := []string{}
		if result != nil {
			for _, outcome := range result.Domains {
				outcomes = append(outcomes, fmt.Sprintf("%+v", *outcome))
			}
		}
		events := []string{}
		for _, event := range sender.Events() {
			events = append(events, fmt.Sprintf("%s %s %s", event.Kind, event.State, event.Reason))
		}
		world.FailProgress(t, "the send through smtpedge", fmt.Errorf("%w; its outcomes %v; the sender's events %v", err, outcomes, events), nil)
	}
	if outcome := result.Domains[0]; outcome.Outcome != sidecar.SmtpOutcomeSent {
		t.Fatalf("smtpedge's send to %s ended %+v, not sent", mailbox.Network.Name, *outcome)
	}
	got, err := world.AwaitMailMessage(ctx, mailbox.Reader, mailbox.Mail, mailSettings.SendSubject, changes, mailSettings.Mail)
	if err != nil {
		world.FailProgress(t, mailbox.Network.Name+"'s mailbox", err, nil)
	}
	// mail from another network is internet mail at its readers, not a member's, its Message-ID under the sender's
	// network domain (§14.6)
	worldSmokeMailCheck(t, mailbox.Reader, got, from, "@"+domain, mailSettings.SendBody, sidecar.MailTrustUntrusted)
	worldSmokeMailIngestByHint(t, mailbox.Reader, got, changes.Count(), mailSettings.Mail)
	if approvals < 2 {
		t.Logf("%s's alias and send passed %d stand-in approvals", owner.Name, approvals)
	}
}

// Reports the reader's ingest of got as a finding unless the deposit's own hint brought it: the reader's ingestor
// watches every ref of the mailbox, a deposit ref's among them (mail_ingest_reader.go's Watch), and §19.8's
// head-to-hint target is 2 s, so a deposit should wake it long before IngestHintTimeout; the wait woke it itself past
// that, as an app's wake does, and carried on.
func worldSmokeMailIngestByHint(t testing.TB, reader *sidecart3.T3Device, got *WorldMailMessage, changes int64, settings *WorldMailSettings) {
	t.Helper()
	if !got.ByHint {
		t.Errorf("%s ingested the deposit only on a wake (MailClient.Resync) past %s, never on the deposit's hint: the receiver's deposit push committed, and the ingestor, which watches every ref of the mailbox, ran no pass for it, while the reader's EvsClient raised %d changes of the mailbox before the wake; whether evs's block sent no hint for the deposit ref, the hint never reached the reader, or its read raised no change the ingestor took is not located further here", reader.Name, settings.IngestHintTimeout, changes)
	}
}

// Holds read to the message sent: its sender, Message-ID and body, and the trust a reader gives it.
func worldSmokeMailCheck(t testing.TB, reader *sidecart3.T3Device, got *WorldMailMessage, sender string, messageId string, body string, trust sidecar.MailTrust) {
	t.Helper()
	switch {
	case !strings.Contains(got.Header("From"), sender):
		t.Fatalf("%s reads the message from %q, not %s", reader.Name, got.Header("From"), sender)
	case !strings.Contains(got.Header("Message-ID"), messageId):
		t.Fatalf("%s reads the message as %q, not %s", reader.Name, got.Header("Message-ID"), messageId)
	case strings.TrimSpace(got.Read.Text) != body:
		t.Fatalf("%s reads the message's text as %q, not %q", reader.Name, got.Read.Text, body)
	case got.Read.Trust != trust:
		t.Fatalf("%s reads the message as %s, not %s", reader.Name, got.Read.Trust, trust)
	}
}

// One plain-text message as an MTA sends it: its header fields, a blank line and the body, every line ended by CRLF.
func worldMailMessageBytes(from string, to string, subject string, messageId string, body string) []byte {
	lines := []string{
		"From: " + from,
		"To: " + to,
		"Subject: " + subject,
		"Date: " + time.Now().UTC().Format(time.RFC1123Z),
		"Message-ID: <" + messageId + ">",
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=utf-8",
		"",
		body,
		"",
	}
	return []byte(strings.Join(lines, "\r\n"))
}
