//go:build sidecar_e2e && sidecar_pending && sidecar_localstack

// Mail through smtpedge and mxedge under concurrency (e2e-mail-concurrency; the owner's end-to-end tests of
// 2026-11-21): M networks of the world (world_test.go), each with mail on, its mailboxes and its receiver, several of
// its sidecars sending at once to its own mailboxes, to the other networks' and to the internet sender's domain, while
// the internet sender (world_mail_dns_test.go) delivers to every network through the lb as four kinds of sender, and
// chaos restarts smtpedge, mxedge's router and one network's prover host, each on its stores:
//
//	network   a personal network of one member with several mailboxes: the member's, which every alias of the network
//	          names, and a group's for each of Groups, a group repo of its own whose address names it, the member
//	          joined to it (§15.8): its owner turns mail on, makes the aliases and the groups (setUpMailNetwork's
//	          header) and reads every mailbox, the groups' messages through its view's r/ references; its second
//	          sidecar is the member's prover host (WorldMailSender); its last is its receiver, which deposits in each
//	          mailbox, a group's by the deposit grant group-create files to the member's devices (QD-1177); every
//	          other sidecar sends, and every one with mail started ingests the deposits, as racing readers do (§15.7)
//	sends     one burst a sending device, one goroutine each, random pauses between its sends (RunBursts): each send
//	          names an address of one of its own network's mailboxes (an alias, or with OwnGroupSends a group too) and
//	          one of another network's, alias or group, and every other send the internet sender's inbox too, so each
//	          is a same-network deposit and internet mail through smtpedge under one effect key (§15.10)
//	internet  one burst a kind of internet sender (worldSenderKind): each delivery through the lb to mxedge's router on
//	          25 and STARTTLS to the network's mail host, retried while it is refused, as an MTA retries; every other
//	          one names every address of the network, one transaction the receiver deposits in each of its mailboxes;
//	          each once the prover hosts have started its share of the SMTP attempts the sends make, so they spread
//	          across them
//	chaos     one burst that restarts each of ChaosBlocks and then one network's prover host, each once the prover
//	          hosts have started its share of those attempts, so each falls while connections are in flight, the
//	          blocks on their stores and the prover host on its device's
//	deposits  then a receiver that cannot deposit (mailDepositFault at evs's origin): its sessions answer 451 4.3.0, and
//	          the internet sender's retry and a network's (its SmtpSender's) land once the fault lifts
//
// The checks, at the end, are strict:
//   - every message reaches each mailbox it names exactly once, whatever aliases of that mailbox it names, and no
//     mailbox holds another's: a mailbox's received entries are exactly the messages that name it, and its sent copies
//     exactly its own member's sends (§15.10's step 2). One exception, which the design makes (§14.4's outcomes, §15.5's
//     retry, §22.7: mail claims no exactly-once SMTP), and only where the case saw its cause: a message may reach a
//     domain twice where a cut ended an attempt at it after the message's last byte and before the answer, so the
//     retry may deliver what the first took (an internet delivery's DeliverMail error at the DATA's end with no reply;
//     a prover host's attempt with no result after its last part), or where the prover host's restart left the
//     domain unended in or/, which the new composition sends again (SmtpSendResult.ResentDomains); each is logged
//   - every body reads back as sent and from its sender, and verifies: a network's internet mail passes DKIM under its
//     network's key at each receiver and at the internet MX, and the internet sender's reads its kind's verdicts (SPF,
//     DKIM, DMARC, the forward-confirmed PTR) with §15.9's spam filing, deposited and answered 250 whatever they are
//     (§15.5)
//   - the outbox: each send that queued internet mail was answered one effect key, whose or/ record holds one sent
//     outcome a recipient domain and whose o/ entry, while one is kept, is signed by the device that sent it and names
//     that send and exactly its internet recipients; no member's or/ or o/ holds a key no send was answered
//   - the internet MX took each send naming it once, from its network's bounce address
//   - the ledgers: once every metering block has drained, each network's balances were debited exactly what its
//     meters hold, never overdrawn, and its mx and smtp meters hold at least the bytes of the mail it took through
//     mxedge and sent through smtpedge
//
// A failure from a product bug stays failing (the e2e conventions). The case's findings, each fixed since:
//
//   - as it landed (2026-11-22), the groups' mailboxes listed none of their mail: the member's readers held no key of
//     a group repo ("gkm unavailable: no generation of the repo is held") until a writer's hourly resync, since the
//     network's first device made its first ReaderKey upload before gkm could load the network's state, so no holder
//     could seal it the group's late delivery; gkm-pending-hint-delay (sidecar 61be60ee to 4a803f0c, server f42425473,
//     3d47ad55c) has a reader gkm serves no ReaderKey for publish one before its late ask. Same-network sends to a
//     network's own group stayed off (OwnGroupSends) until group-create granted a personal network's group its deposit
//     to the member's devices (QD-1177, QD-1180; personal-group-deposit, sidecar 27e157b4, 20fc0e65, server 5284737b1
//     to 6b6b2f201), and the case has sent them since
//   - on those, the groups' mailboxes still listed none of their mail: ingest took the deposits of a device that reads
//     a group without writing it, as each of the member's devices but the group's creator does, for read receipts and
//     left them standing (question personal-group-deposit-group-deposits-classed-receipts, QD-1181).
//     ingest-receipts-owner-bound (sidecar 6fdaa49c, 05687327) classes such a depositor as a read receipt's only in an
//     owner-bound repo, and the case passed in full on it (2026-11-22), its group mailboxes included
//
// The churn case met one more loss in a group mailbox, under racing ingest pushes, which mail-group-ingest-loss fixed
// (churn_load_test.go's header). On every fix, e2e-headers-final's run passed the case and
// TestE2eMailInternetSenderVerdicts (2026-11-23, 149 s and 107 s, server 9d7022daa on sidecar c88a64af). Run it
// through the build's wrapper: -tags 'sidecar_e2e sidecar_pending sidecar_localstack'.

package sidecare2e

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/mail"
	"net/textproto"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server/sidecart3"
)

// What the mail cases run with (QD-702).
type mailConcurrencySettings struct {
	// M, the networks, and the sidecars of each: its owner, which reads the mailbox, its prover host, more senders,
	// and its receiver, the last.
	NetworkCount      int
	ClientsPerNetwork int
	// The aliases each network makes, every one its member's mailbox's.
	Aliases []string
	// The group addresses each network makes, each a mailbox of its own, a group repo, whose one member is the
	// network's (§15.8's group, `ur id group-add`), so a network holds several mailboxes.
	Groups []string
	// Whether a send may name its own network's groups, a same-network deposit the sending device makes itself
	// (§15.10), which group-create's deposit grant to a personal network's member's devices allows every sending device
	// (QD-1177). The other networks' groups take it through smtpedge and their receivers.
	OwnGroupSends bool
	// The internet sender's inbox, which a network's internet mail names.
	InternetTo string
	// Each sending device's sends.
	SendsPerDevice int
	// Each kind of internet sender's deliveries.
	InternetDeliveriesPerKind int
	// The blocks chaos restarts, in order, and whether it then restarts the first network's prover host: each once the
	// sends have passed its share of their count.
	ChaosBlocks []string
	ChaosSender bool
	// The bursts' pauses, seed and phase bound, and each other phase's bound, and the bound on one network's wait for
	// its reader to list every message its mailboxes took.
	Bursts         *WorldBurstSettings
	PhaseTimeout   time.Duration
	MailboxTimeout time.Duration
	// The case's bound from the world's start: past the conventions' 10 minutes, since it sets up M networks' mail.
	CaseTimeout time.Duration
	// Each network's ACL document's grants for mail: its members listen on smtp/25/mx and on every alias, and send as
	// one (§15.5, §15.8), as the smoke's.
	AclGrants []string
	// How old each network reads, past §21.7's 7-day probation, and how long ago its mail was turned on, past §14.7's
	// 24 h: the world's networks are made new.
	NetworkAge time.Duration
	MailAge    time.Duration
	// The helpers' bounds and the receiver's timing, and each prover host's composition.
	Mail   *WorldMailSettings
	Prover *WorldMailSenderSettings
	// The SMTP sender's schedule on the world's real time (mailSenderTimingSettings).
	SenderTiming *mailSenderTimingSettings
	// The blocks restarted at the end so each drains, sending its open usage window, before the ledgers.
	FinalDrainBlocks []string
	// The most entries one page of a mailbox's list holds.
	ListPageCount int
}

// The SMTP sender's minutes and hours, shortened for a world on real time: what a prover host's SmtpSender retries
// and takes over on (sidecar.SmtpSenderSettings' fields of the same names).
type mailSenderTimingSettings struct {
	RetryFirstTimeout time.Duration
	RetryMaxTimeout   time.Duration
	// A message's retries' units a UTC day, past the production 2 so chaos and the deposits phase strand no send
	// until tomorrow.
	RetryUnitsPerMessagePerDay int
	OutboxPollInterval         time.Duration
	OutboxProgressInterval     time.Duration
	// Past OutboxProgressInterval, so no live send is taken over.
	OutboxTakeoverTimeout time.Duration
}

func defaultMailSenderTimingSettings() *mailSenderTimingSettings {
	return &mailSenderTimingSettings{
		RetryFirstTimeout:          2 * time.Second,
		RetryMaxTimeout:            8 * time.Second,
		RetryUnitsPerMessagePerDay: 8,
		OutboxPollInterval:         2 * time.Second,
		OutboxProgressInterval:     10 * time.Second,
		OutboxTakeoverTimeout:      30 * time.Second,
	}
}

// Sets settings' fields from these.
func (self *mailSenderTimingSettings) apply(settings *sidecar.SmtpSenderSettings) {
	settings.RetryFirstTimeout = self.RetryFirstTimeout
	settings.RetryMaxTimeout = self.RetryMaxTimeout
	settings.RetryUnitsPerMessagePerDay = self.RetryUnitsPerMessagePerDay
	settings.OutboxPollInterval = self.OutboxPollInterval
	settings.OutboxProgressInterval = self.OutboxProgressInterval
	settings.OutboxTakeoverTimeout = self.OutboxTakeoverTimeout
}

func defaultMailConcurrencySettings() *mailConcurrencySettings {
	timing := defaultMailSenderTimingSettings()
	prover := DefaultWorldMailSenderSettings()
	prover.Sender = timing.apply
	return &mailConcurrencySettings{
		NetworkCount:              3,
		ClientsPerNetwork:         4,
		Aliases:                   []string{"inbox", "orders"},
		Groups:                    []string{"team"},
		OwnGroupSends:             true,
		InternetTo:                "inbox",
		SendsPerDevice:            3,
		InternetDeliveriesPerKind: 3,
		ChaosBlocks:               []string{WorldBlockSmtpedge, WorldBlockMxedge},
		ChaosSender:               true,
		Bursts:                    DefaultWorldBurstSettings(),
		PhaseTimeout:              4 * time.Minute,
		MailboxTimeout:            2 * time.Minute,
		CaseTimeout:               20 * time.Minute,
		AclGrants:                 defaultWorldSmokeMailSettings().AclGrants,
		NetworkAge:                8 * 24 * time.Hour,
		MailAge:                   2 * 24 * time.Hour,
		Mail:                      DefaultWorldMailSettings(),
		Prover:                    prover,
		SenderTiming:              timing,
		FinalDrainBlocks:          []string{WorldBlockSmtpedge, WorldBlockMxedge, WorldBlockSockets, WorldBlockEvs},
		ListPageCount:             200,
	}
}

// A world for the mail cases: M networks of N sidecars and the case's bound.
func newMailWorld(t testing.TB, settings *mailConcurrencySettings) *World {
	worldSettings := DefaultWorldSettings()
	worldSettings.NetworkCount = settings.NetworkCount
	worldSettings.ClientsPerNetwork = settings.ClientsPerNetwork
	hostsSettings := defaultE2eHostsSettings()
	hostsSettings.CaseTimeout = settings.CaseTimeout
	worldSettings.Hosts = hostsSettings
	return NewWorld(t, worldSettings)
}

// The world's networks, each aged past probation before anything reads its standing, verified by a paid Pro balance,
// so it sends internet mail at Pro's caps, and holding the mail grants.
func addMailNetworks(ctx context.Context, t testing.TB, world *World, settings *mailConcurrencySettings) []*WorldNetwork {
	networks, err := world.AddNetworks(ctx, nil)
	if err != nil {
		world.FailProgress(t, "the enrollment", err, nil)
	}
	for _, network := range networks {
		if len(network.Clients) < 3 {
			t.Fatalf("%s has %d sidecars; a mail network is an owner, a prover host and a receiver at the least", network.Name, len(network.Clients))
		}
		if err := worldAgeNetwork(ctx, network.NetworkId, settings.NetworkAge); err != nil {
			t.Fatal(err)
		}
		if err := world.VerifyNetwork(ctx, network, true, nil); err != nil {
			t.Fatalf("%s's paid balance: %v", network.Name, err)
		}
		if _, err := world.SaveAclPolicy(ctx, network, network.Owner(), worldAclPolicy(settings.AclGrants...)); err != nil {
			world.FailProgress(t, network.Name+"'s mail grants", err, nil)
		}
	}
	return networks
}

// One mailbox of a network: its member's, which the network's aliases name, or a group's, which the group's address
// names.
type mailMailbox struct {
	// what the checks name it by: "<network>'s member mailbox" or "<network>'s group <local part>"
	Name    string
	Network *mailNetwork
	RepoId  sidecar.Id
	// the addresses that deliver to it
	Addresses []string
}

// One network of the mail cases as its setup left it. Safe for concurrent use once set up.
type mailNetwork struct {
	*WorldNetwork
	// its zone's apex without its final dot, the mail domain, and its aliases' addresses in the settings' order
	Domain    string
	Addresses []string
	MailboxId sidecar.Id
	// its mailboxes: the member's first, then each group's in the settings' order
	Mailboxes []*mailMailbox
	// what the dns edge answers for the mail domain, which an internet sender delivers by
	MailDns *WorldMailDns
	// the mailbox's reader, the owner, and its receiver
	Reader       *sidecart3.T3Device
	ReaderMail   *sidecar.MailClient
	Receiver     *sidecart3.T3Device
	ReceiverMail *sidecar.MailClient
	// the sidecars that send, the owner first and the prover host second, and their MailClients
	Senders     []*sidecart3.T3Device
	SenderMails []*sidecar.MailClient
	// the stand-in approvals each sender's gate ran, in Senders' order
	SenderGates []*worldDeviceGate

	proverLock sync.Mutex
	prover     *WorldMailSender
	// every composition of the prover host, the first first, the current last
	provers []*WorldMailSender
}

// The member's prover host's composition as it stands.
func (self *mailNetwork) Prover() *WorldMailSender {
	self.proverLock.Lock()
	defer self.proverLock.Unlock()
	return self.prover
}

// Every address of every one of the network's mailboxes, the member's aliases first.
func (self *mailNetwork) AllAddresses() []string {
	addresses := []string{}
	for _, mailbox := range self.Mailboxes {
		addresses = append(addresses, mailbox.Addresses...)
	}
	return addresses
}

// The mailbox address delivers to, nil for an address of none of the network's.
func (self *mailNetwork) MailboxOf(address string) *mailMailbox {
	for _, mailbox := range self.Mailboxes {
		if slices.ContainsFunc(mailbox.Addresses, func(held string) bool { return strings.EqualFold(held, address) }) {
			return mailbox
		}
	}
	return nil
}

// The mailbox whose repo is repoId, nil for a repo of none of the network's.
func (self *mailNetwork) MailboxByRepo(repoId sidecar.Id) *mailMailbox {
	for _, mailbox := range self.Mailboxes {
		if mailbox.RepoId == repoId {
			return mailbox
		}
	}
	return nil
}

// Whether an attempt of one of the prover host's compositions at sendId's domain ended with neither an error-free end
// nor an answer to its message, after it submitted its last part, which its message's bytes follow: the MX may have
// taken the message then, so the sender's retry, which an attempt with no result schedules, may deliver it there again
// (§15.5's 250 follows the deposit; §14.4 and §22.7 claim no exactly-once SMTP). Read once every send has ended.
func (self *mailNetwork) CutAfterMessage(sendId sidecar.Id, domain string) bool {
	provers := func() []*WorldMailSender {
		self.proverLock.Lock()
		defer self.proverLock.Unlock()
		return slices.Clone(self.provers)
	}()
	for _, prover := range provers {
		for _, send := range prover.Prover.Sends() {
			if send.Preamble == nil || !strings.EqualFold(send.Preamble.RecipientDomain, domain) {
				continue
			}
			if attemptSendId, ok := mailSendIdOfMessageId(send.Preamble.MessageId); !ok || attemptSendId != sendId {
				continue
			}
			lastSubmitted := slices.ContainsFunc(send.Parts, func(part *sidecar.SmtpProofPart) bool { return part.Last })
			if send.Err != nil && send.Result == nil && lastSubmitted {
				return true
			}
		}
	}
	return false
}

// How many SMTP attempts the network's prover host's compositions have started, each a connection through smtpedge.
func (self *mailNetwork) AttemptCount() int {
	provers := func() []*WorldMailSender {
		self.proverLock.Lock()
		defer self.proverLock.Unlock()
		return slices.Clone(self.provers)
	}()
	count := 0
	for _, prover := range provers {
		count += len(prover.Prover.Sends())
	}
	return count
}

// What the network's prover host's compositions did with sendId, for a stall's message: each attempt at it, by domain,
// with its last command and reply, its result and its error.
func (self *mailNetwork) DescribeSend(sendId sidecar.Id) string {
	provers := func() []*WorldMailSender {
		self.proverLock.Lock()
		defer self.proverLock.Unlock()
		return slices.Clone(self.provers)
	}()
	lines := []string{}
	for index, prover := range provers {
		for _, send := range prover.Prover.Sends() {
			if send.Preamble == nil {
				continue
			}
			if attemptSendId, ok := mailSendIdOfMessageId(send.Preamble.MessageId); !ok || attemptSendId != sendId {
				continue
			}
			lastCommand, lastReply := "", ""
			if 0 < len(send.Commands) {
				lastCommand = strings.TrimSpace(send.Commands[len(send.Commands)-1])
			}
			if 0 < len(send.Replies) {
				lastReply = send.Replies[len(send.Replies)-1]
			}
			result := "no result"
			if send.Result != nil {
				result = fmt.Sprintf("%+v", *send.Result)
			}
			lines = append(lines, fmt.Sprintf("composition %d: an attempt at %s, %d parts, %d commands, last %q, %d replies, last %q, %s, err %v", index, send.Preamble.RecipientDomain, len(send.Parts), len(send.Commands), lastCommand, len(send.Replies), lastReply, result, send.Err))
		}
	}
	if len(lines) == 0 {
		return "no attempt at it"
	}
	return strings.Join(lines, "; ")
}

// Each domain's outcome of result, for a stall's message.
func mailDescribeOutcomes(result *sidecar.SmtpSendResult) string {
	if result == nil {
		return "no result"
	}
	outcomes := []string{}
	for _, outcome := range result.Domains {
		outcomes = append(outcomes, fmt.Sprintf("%s %s (retry after %s, updated %s, tls %d, reply %d)", outcome.Domain, mailOutcomeName(outcome.Outcome), outcome.RetryAfter, outcome.UpdateTime.Format(time.RFC3339Nano), outcome.TlsLevel, outcome.ReplyCode))
	}
	return fmt.Sprintf("%v, resent %v", outcomes, result.ResentDomains)
}

// §14.5's name of kind, for a message.
func mailOutcomeName(kind sidecar.SmtpOutcomeKind) string {
	switch kind {
	case sidecar.SmtpOutcomeSent:
		return "sent"
	case sidecar.SmtpOutcomeQueued:
		return "queued"
	case sidecar.SmtpOutcomeDeferred:
		return "deferred"
	case sidecar.SmtpOutcomeTooLarge:
		return "too_large"
	case sidecar.SmtpOutcomeRefused:
		return "refused"
	case sidecar.SmtpOutcomeTlsRefused:
		return "tls_refused"
	case sidecar.SmtpOutcomeFailed:
		return "failed"
	}
	return fmt.Sprintf("outcome %d", uint32(kind))
}

// The send a Message-ID names (sidecar.MailOutboxSendIdOf), with or without its angle brackets.
func mailSendIdOfMessageId(messageId string) (sidecar.Id, bool) {
	messageId = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(messageId), "<"), ">")
	return sidecar.MailOutboxSendIdOf([]byte("Message-ID: <" + messageId + ">\r\n\r\n"))
}

// Wakes the prover host's runner, so it reads o/ now.
func (self *mailNetwork) WakeProver() {
	self.Prover().Wake()
}

// Restarts the prover host's composition on its stores (World.RestartMailSender), and answers the effect keys and
// domains its stop left unended in the member's or/, whose sends the new composition may send again
// (SmtpSendResult.ResentDomains).
func (self *mailNetwork) RestartProver(ctx context.Context, world *World, settings *mailConcurrencySettings) (map[string][]string, error) {
	self.proverLock.Lock()
	defer self.proverLock.Unlock()
	self.prover.Stop()
	unended, err := mailUnendedOutcomes(ctx, self.SenderMails[1])
	if err != nil {
		return nil, err
	}
	restarted, err := world.RestartMailSender(ctx, self.prover, self.SenderMails[1], settings.Prover)
	if err != nil {
		return nil, err
	}
	self.prover = restarted
	self.provers = append(self.provers, restarted)
	return unended, nil
}

// The domains of each or/ record of client's member not ended, by effect key as hex.
func mailUnendedOutcomes(ctx context.Context, client *sidecar.MailClient) (map[string][]string, error) {
	mailbox, err := client.SmtpMailbox()
	if err != nil {
		return nil, err
	}
	effectKeys, err := mailbox.ListOutcomes(ctx)
	if err != nil {
		return nil, err
	}
	unended := map[string][]string{}
	for _, effectKey := range effectKeys {
		outcomes, err := mailbox.ReadOutcomes(ctx, effectKey)
		if err != nil {
			return nil, err
		}
		for _, outcome := range outcomes {
			if !mailOutcomeEnded(outcome.Outcome) {
				unended[string(effectKey)] = append(unended[string(effectKey)], outcome.Domain)
			}
		}
	}
	return unended, nil
}

// Whether kind ends a domain's send (smtp_outbox.go's smtpOutcomesEnded).
func mailOutcomeEnded(kind sidecar.SmtpOutcomeKind) bool {
	switch kind {
	case sidecar.SmtpOutcomeSent, sidecar.SmtpOutcomeRefused, sidecar.SmtpOutcomeTooLarge, sidecar.SmtpOutcomeTlsRefused, sidecar.SmtpOutcomeFailed:
		return true
	}
	return false
}

// network's mail (the file's header): its senders' and receiver's gates, mail on through its owner, its enable aged
// before its first preamble, its aliases, its groups, its receiver serving smtp/25/mx with its TLSA live in the dns
// edge's answers, every sender's mail started and holding every address, and its prover host composed.
//
// The owner's device creates each group, as a person would from any of their devices: on a personal network
// group-create grants the group's deposit to the member's devices (autogroup:member; QD-1177), so the receiver, on
// another of them, deposits the group's mail, and with OwnGroupSends so does every sending device, each awaited in its
// own verified state before any mail goes.
func setUpMailNetwork(ctx context.Context, world *World, settings *mailConcurrencySettings, network *WorldNetwork) (*mailNetwork, error) {
	last := len(network.Clients) - 1
	set := &mailNetwork{
		WorldNetwork: network,
		Reader:       network.Owner(),
		Receiver:     network.Clients[last],
		Senders:      slices.Clone(network.Clients[:last]),
	}
	for index, device := range set.Senders {
		approved := []sidecar.SidecarActionKind{sidecar.SidecarActionMail}
		if index == 0 {
			approved = append(approved, sidecar.SidecarActionAliasCreate)
		}
		gate, err := world.InstallDeviceGate(ctx, device, approved...)
		if err != nil {
			return nil, err
		}
		set.SenderGates = append(set.SenderGates, gate)
	}
	// the receiver's listen on smtp/25/mx goes through the stock gate, under the standing grant, and it creates no
	// address, so nothing of it is held for a person
	if _, err := world.InstallApprovalGate(ctx, set.Receiver); err != nil {
		return nil, err
	}
	readerMail, err := world.StartMail(ctx, set.Reader, false, settings.Mail)
	if err != nil {
		return nil, err
	}
	set.ReaderMail = readerMail
	if _, err := world.EnableMail(ctx, set.Reader, readerMail, settings.Mail); err != nil {
		return nil, err
	}
	if err := worldAgeMailEnable(ctx, network.NetworkId, settings.MailAge); err != nil {
		return nil, err
	}
	zone, err := worldDeviceZone(ctx, set.Reader)
	if err != nil {
		return nil, err
	}
	set.Domain = strings.TrimSuffix(zone.Apex(), ".")
	for _, alias := range settings.Aliases {
		if _, err := readerMail.CreateAlias(ctx, &sidecar.MailAliasCreateArgs{LocalPart: alias}); err != nil {
			return nil, fmt.Errorf("%s's alias %s: %w", network.Name, alias, err)
		}
		set.Addresses = append(set.Addresses, alias+"@"+set.Domain)
	}
	// each group, through the owner's device (the function's header): its address and its repo (`ur id
	// group-create`, confirmed as a person confirms it at /dev/tty, with its deposit grant), and the network's member
	// joined to it (`ur id group-add`), so the member's readers list its mail through their r/ references
	groups := []*mailMailbox{}
	for _, group := range settings.Groups {
		repo, err := mailCreateGroup(ctx, readerMail, group, network.MemberId(), settings.Mail)
		if err != nil {
			return nil, fmt.Errorf("%s's group %s: %w", network.Name, group, err)
		}
		groups = append(groups, &mailMailbox{Name: network.Name + "'s group " + group, Network: set, RepoId: repo.RepoId(), Addresses: []string{group + "@" + set.Domain}})
	}
	receiverMail, err := world.StartMail(ctx, set.Receiver, true, settings.Mail)
	if err != nil {
		return nil, err
	}
	set.ReceiverMail = receiverMail
	if _, err := world.AwaitMailReceiver(ctx, set.Receiver, receiverMail, settings.Mail); err != nil {
		return nil, err
	}
	mailboxId, ok := worldMemberMailboxId(set.Receiver)
	if !ok {
		return nil, fmt.Errorf("%s's verified state holds no mailbox of its member", set.Receiver.Name)
	}
	set.MailboxId = mailboxId
	set.Mailboxes = append([]*mailMailbox{{Name: network.Name + "'s member mailbox", Network: set, RepoId: mailboxId, Addresses: set.Addresses}}, groups...)
	for _, mailbox := range set.Mailboxes {
		if err := awaitMailDepositRight(ctx, set.Receiver, mailbox, settings.Mail); err != nil {
			return nil, err
		}
	}
	key, err := set.Receiver.Sidecar().TlsDeviceKey()
	if err != nil {
		return nil, fmt.Errorf("%s's TLS key: %w", set.Receiver.Name, err)
	}
	spkiSha256 := key.SpkiSha256()
	if set.MailDns, err = world.AwaitMailDns(ctx, set.Domain, [][]byte{spkiSha256[:]}, nil); err != nil {
		return nil, err
	}
	set.SenderMails = []*sidecar.MailClient{readerMail}
	for _, device := range set.Senders[1:] {
		client, err := world.StartMail(ctx, device, false, settings.Mail)
		if err != nil {
			return nil, err
		}
		set.SenderMails = append(set.SenderMails, client)
	}
	for index, client := range set.SenderMails {
		if err := awaitMailAliases(ctx, set.Senders[index], client, set.AllAddresses(), settings.Mail); err != nil {
			return nil, err
		}
	}
	// a send to the network's own group deposits from the sending device, by the group's deposit grant, or the owner's
	// device by its write as the group repo's admin (QD-1173)
	if settings.OwnGroupSends {
		for _, device := range set.Senders {
			for _, group := range groups {
				if err := awaitMailDepositRight(ctx, device, group, settings.Mail); err != nil {
					return nil, err
				}
			}
		}
	}
	// the receiver answers RCPT from its own mailcfg (§15.5), 550 5.1.1 for an address it does not hold yet
	if err := awaitMailAliases(ctx, set.Receiver, receiverMail, set.AllAddresses(), settings.Mail); err != nil {
		return nil, err
	}
	prover, err := world.StartMailSender(ctx, set.Senders[1], set.SenderMails[1], settings.Prover)
	if err != nil {
		return nil, err
	}
	set.prover = prover
	set.provers = []*WorldMailSender{prover}
	return set, nil
}

// A group through client (MailClient.CreateGroup, confirmed), and memberId joined to it (MailClient.AddGroupMember),
// each tried again every RetryInterval within Timeout while the client has not bound what it works on yet
// (ErrMailUnbound) or its mailcfg has not synced (ErrMailConfigNotSynced), with no hook a test reads; any other answer
// is the call's. Both are created only if absent, so a retry finds what an earlier try made.
func mailCreateGroup(ctx context.Context, client *sidecar.MailClient, group string, memberId sidecar.Id, settings *WorldMailSettings) (*sidecar.EvsRepo, error) {
	ctx, cancel := context.WithTimeout(ctx, settings.Timeout)
	defer cancel()
	retry := func(err error) error {
		if !errors.Is(err, sidecar.ErrMailUnbound) && !errors.Is(err, sidecar.ErrMailConfigNotSynced) {
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w (%w)", err, ctx.Err())
		case <-time.After(settings.RetryInterval):
			return nil
		}
	}
	for {
		repo, err := client.CreateGroup(ctx, &sidecar.MailCreateGroupArgs{LocalPart: group, Confirmed: true})
		if err == nil {
			for {
				err := client.AddGroupMember(ctx, group, memberId)
				if err == nil {
					return repo, nil
				}
				if err := retry(err); err != nil {
					return nil, fmt.Errorf("the member's join: %w", err)
				}
			}
		}
		if err := retry(err); err != nil {
			return nil, err
		}
	}
}

// Waits until device's verified state grants it deposit on mailbox's repo, which every deposit's repo:<id>:deposit
// token asks (evs_deposit.go), a receiver's and a same-network sender's alike: a group repo's statements reach the
// device's follower with no hook a test reads, so its state is read every RetryInterval within Timeout.
func awaitMailDepositRight(ctx context.Context, device *sidecart3.T3Device, mailbox *mailMailbox, settings *WorldMailSettings) error {
	ctx, cancel := context.WithTimeout(ctx, settings.Timeout)
	defer cancel()
	for {
		access := worldEvsAccess(device, mailbox.RepoId)
		if access.Rights.Has(sidecar.AclEvsDeposit) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s, of %s, never held deposit on %s: it holds %b (%w)", device.Name, mailbox.Network.Name, mailbox.Name, access.Rights, ctx.Err())
		case <-time.After(settings.RetryInterval):
		}
	}
}

// Waits until client's synced mailcfg holds every address of addresses, so its sends may name them as From and its
// deposits and RCPT replies find them: the owner's creations reach another device's mailcfg follower with no hook a
// test reads, so it is read every RetryInterval within Timeout.
func awaitMailAliases(ctx context.Context, device *sidecart3.T3Device, client *sidecar.MailClient, addresses []string, settings *WorldMailSettings) error {
	ctx, cancel := context.WithTimeout(ctx, settings.Timeout)
	defer cancel()
	for {
		aliases, err := client.Aliases(ctx)
		held := []string{}
		for _, alias := range aliases {
			held = append(held, strings.ToLower(alias.Address))
		}
		missing := 0
		for _, address := range addresses {
			if !slices.Contains(held, strings.ToLower(address)) {
				missing += 1
			}
		}
		if err == nil && missing == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s's mailcfg never held its network's %d aliases: %d missing, %v (%w)", device.Name, len(addresses), missing, err, ctx.Err())
		case <-time.After(settings.RetryInterval):
		}
	}
}

// Every network's mail set up at once, one burst a network.
func setUpMailNetworks(ctx context.Context, t testing.TB, world *World, settings *mailConcurrencySettings, networks []*WorldNetwork) []*mailNetwork {
	sets := make([]*mailNetwork, len(networks))
	bursts := []*WorldBurst{}
	for index, network := range networks {
		bursts = append(bursts, &WorldBurst{
			Name:      network.Name + "'s mail",
			StepCount: 1,
			Step: func(ctx context.Context, step int, random *rand.Rand) error {
				set, err := setUpMailNetwork(ctx, world, settings, network)
				sets[index] = set
				return err
			},
		})
	}
	burstSettings := *settings.Bursts
	burstSettings.PhaseTimeout = settings.PhaseTimeout
	if err := RunBursts(ctx, t, "the networks' mail", bursts, &burstSettings); err != nil {
		world.FailProgress(t, "the networks' mail", err, bursts)
	}
	for _, burst := range bursts {
		if errs := burst.Errors(); 0 < len(errs) {
			world.FailProgress(t, burst.Name, errors.Join(errs...), bursts)
		}
	}
	return sets
}

// Send through client, tried again every RetryInterval within Timeout while the client has not bound what it works
// on yet (ErrMailUnbound); any other answer is the send's.
func mailSend(ctx context.Context, client *sidecar.MailClient, args *sidecar.MailSendArgs, settings *WorldMailSettings) (*sidecar.MailSendResult, error) {
	ctx, cancel := context.WithTimeout(ctx, settings.Timeout)
	defer cancel()
	for {
		result, err := client.Send(ctx, args)
		if err == nil || !errors.Is(err, sidecar.ErrMailUnbound) {
			return result, err
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("the send stayed unbound: %w (%w)", err, ctx.Err())
		case <-time.After(settings.RetryInterval):
		}
	}
}

// The subject's prefix every message of the cases carries before its token.
const mailSubjectPrefix = "mail concurrency "

// What one message of the cases must come to (the file's header's checks).
type mailExpected struct {
	Token   string
	Subject string
	From    string
	Body    string
	// every recipient it names
	Recipients []string
	// the mailboxes its recipients name, by name, each to take it once however many of its addresses it names
	Mailboxes []string
	// whether the internet MX must take it once
	Internet bool
	// a network's send: its network, its device and what Send answered, and the internet domains its effect key's
	// or/ outcomes name, sorted
	Network *mailNetwork
	Device  *sidecart3.T3Device
	Sent    *sidecar.MailSendResult
	Domains []string
	// the internet sender's: its kind
	FromInternet bool
	Kind         worldSenderKind
}

// Every expected message by token. Safe for concurrent use.
type mailLedger struct {
	stateLock sync.Mutex
	byToken   map[string]*mailExpected
	tokens    []string
	// the domains a prover host's restart left unended, by effect key, which may take their send twice
	resendable map[string][]string
	// the internet sender's deliveries, by token, one of whose attempts was cut after its message's last byte, before
	// the DATA's end was answered, so the receiver may have deposited it then
	cut map[string]bool
}

func newMailLedger() *mailLedger {
	return &mailLedger{byToken: map[string]*mailExpected{}, resendable: map[string][]string{}, cut: map[string]bool{}}
}

func (self *mailLedger) add(expected *mailExpected) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if _, ok := self.byToken[expected.Token]; ok {
		return fmt.Errorf("token %s twice", expected.Token)
	}
	self.byToken[expected.Token] = expected
	self.tokens = append(self.tokens, expected.Token)
	return nil
}

func (self *mailLedger) get(token string) *mailExpected {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.byToken[token]
}

// Every expected message, in the order added.
func (self *mailLedger) all() []*mailExpected {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	expected := []*mailExpected{}
	for _, token := range self.tokens {
		expected = append(expected, self.byToken[token])
	}
	return expected
}

// Keeps what a prover host's restart left unended as resendable.
func (self *mailLedger) addResendable(unended map[string][]string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for effectKey, domains := range unended {
		self.resendable[effectKey] = append(self.resendable[effectKey], domains...)
	}
}

// Notes that one of token's deliveries was cut after its message's last byte, before the DATA's end was answered.
func (self *mailLedger) addCut(token string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.cut[token] = true
}

// Why expected may have reached domain twice (the file's header's exception), "" where it may not: the internet
// sender's delivery a cut ended after its message's last byte, which the sender retried; a network's send whose domain
// a prover host's restart left unended in or/, which the new composition sends again (SmtpSendResult.ResentDomains); or
// one an attempt of its prover host's was cut at after its message, which the sender retries.
func (self *mailLedger) mayRepeat(expected *mailExpected, domain string) string {
	if expected.FromInternet {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.cut[expected.Token] {
			return "a cut ended one of its deliveries after the message's last byte, before the DATA's end was answered, and the sender retried it (§15.5)"
		}
		return ""
	}
	if expected.Sent == nil || len(expected.Sent.EffectKey) == 0 {
		return ""
	}
	resendable := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return slices.Contains(self.resendable[string(expected.Sent.EffectKey)], domain)
	}()
	switch {
	case resendable:
		return "its prover host's restart cut its send before the result, and the new composition sent it again (§14.4, §22.7)"
	case expected.Network.CutAfterMessage(expected.Sent.SendId, domain):
		return "a cut ended its prover host's attempt after its message, before the MX's answer, and the sender retried it (§14.4, §22.7)"
	}
	return ""
}

// Whether err, a DeliverMail's, ended its session after the message's last byte was written and before an answer to
// the DATA's end: no SMTP reply came, so the receiver may have deposited the message, as 250 follows the deposit
// (§15.5).
func mailCutAfterData(err error) bool {
	return err != nil && strings.Contains(err.Error(), "the DATA's end") && mailSmtpReplyOf(err) == nil
}

// DeliverMail until the DATA's end is answered 250, tried again every RetryInterval within Timeout while it is refused
// (World.AwaitMailDelivery's loop), each attempt cut after its message's last byte noted in ledger under token.
func mailAwaitDelivery(ctx context.Context, world *World, ledger *mailLedger, token string, delivery *WorldMailDelivery, settings *WorldMailSettings) error {
	ctx, cancel := context.WithTimeout(ctx, settings.Timeout)
	defer cancel()
	tries := 0
	for {
		tries += 1
		err := world.DeliverMail(ctx, delivery, settings)
		if err == nil {
			return nil
		}
		if mailCutAfterData(err) {
			ledger.addCut(token)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the delivery to %s, %d tries: %w (%w)", delivery.Dns.MxHost, tries, err, ctx.Err())
		case <-time.After(settings.RetryInterval):
		}
	}
}

// The token a subject carries, false for a subject of no message of the cases.
func mailTokenOf(subject string) (string, bool) {
	token, ok := strings.CutPrefix(strings.TrimSpace(subject), mailSubjectPrefix)
	return token, ok && token != ""
}

// The mailbox of one of networks that address delivers to, nil for an address of none of theirs.
func mailMailboxOf(networks []*mailNetwork, address string) *mailMailbox {
	for _, network := range networks {
		if mailbox := network.MailboxOf(address); mailbox != nil {
			return mailbox
		}
	}
	return nil
}

// The names of the mailboxes of networks that addresses deliver to, each once, in the order first named.
func mailMailboxNames(networks []*mailNetwork, addresses []string) []string {
	names := []string{}
	for _, address := range addresses {
		if mailbox := mailMailboxOf(networks, address); mailbox != nil && !slices.Contains(names, mailbox.Name) {
			names = append(names, mailbox.Name)
		}
	}
	return names
}

// One network's send of the cases: from its first alias to recipients, each an address of one of networks' mailboxes
// or the internet sender's, the expectation kept in ledger and its immediate outcomes checked, its prover host woken
// where it queued internet mail.
func mailNetworkSend(ctx context.Context, world *World, settings *mailConcurrencySettings, ledger *mailLedger, networks []*mailNetwork, network *mailNetwork, senderIndex int, token string, recipients []string) error {
	device, client := network.Senders[senderIndex], network.SenderMails[senderIndex]
	expected := &mailExpected{
		Token:      token,
		Subject:    mailSubjectPrefix + token,
		From:       network.Addresses[0],
		Body:       fmt.Sprintf("%s's message %s, to %s.", device.Name, token, strings.Join(recipients, ", ")),
		Recipients: slices.Clone(recipients),
		Mailboxes:  mailMailboxNames(networks, recipients),
		Network:    network,
		Device:     device,
	}
	// the internet domains: every recipient's outside the sending network's own, a same-network recipient deposited
	// by the sending device itself (§15.10)
	domains := []string{}
	for _, recipient := range recipients {
		_, domain, _ := strings.Cut(strings.ToLower(recipient), "@")
		if strings.EqualFold(domain, world.dnssec.sender.Domain()) {
			expected.Internet = true
		} else if mailMailboxOf(networks, recipient) == nil {
			return fmt.Errorf("%s's send %s names %s, an address of no mailbox of the case's", device.Name, token, recipient)
		}
		if !strings.EqualFold(domain, network.Domain) && !slices.Contains(domains, domain) {
			domains = append(domains, domain)
		}
	}
	sort.Strings(domains)
	expected.Domains = domains
	sent, err := mailSend(ctx, client, &sidecar.MailSendArgs{From: expected.From, To: recipients, Subject: expected.Subject, Text: expected.Body}, settings.Mail)
	if err != nil {
		return fmt.Errorf("%s's send %s: %w", device.Name, token, err)
	}
	expected.Sent = sent
	if err := ledger.add(expected); err != nil {
		return err
	}
	for _, recipient := range sent.Recipients {
		_, domain, _ := strings.Cut(recipient.Address, "@")
		want := sidecar.MailSendQueued
		if strings.EqualFold(domain, network.Domain) {
			want = sidecar.MailSendDelivered
		}
		if recipient.Outcome != want {
			return fmt.Errorf("%s's send %s answered %s for %s, not %s", device.Name, token, recipient.Outcome, recipient.Address, want)
		}
	}
	if !sent.SentCopy {
		return fmt.Errorf("%s's send %s kept no sent copy", device.Name, token)
	}
	if 0 < len(domains) {
		if len(sent.EffectKey) == 0 {
			return fmt.Errorf("%s's send %s queued internet mail under no effect key", device.Name, token)
		}
		network.WakeProver()
	} else if len(sent.EffectKey) != 0 {
		return fmt.Errorf("%s's send %s queued under an effect key with no internet recipient", device.Name, token)
	}
	return nil
}

// The internet sender's delivery of token as kind to addresses of network, through the lb to mxedge's router, tried
// again while it is refused (mailAwaitDelivery), the expectation kept in ledger: one transaction, which the receiver
// deposits once in each mailbox its addresses name (§15.5).
func mailInternetDeliver(ctx context.Context, world *World, settings *mailConcurrencySettings, ledger *mailLedger, kind worldSenderKind, network *mailNetwork, token string, addresses []string) error {
	sender := world.dnssec.sender
	expected := &mailExpected{
		Token:        token,
		Subject:      mailSubjectPrefix + token,
		From:         sender.Address(kind.String()),
		Body:         fmt.Sprintf("The internet sender's %s message %s, to %s.", kind, token, strings.Join(addresses, ", ")),
		Recipients:   slices.Clone(addresses),
		Mailboxes:    mailMailboxNames([]*mailNetwork{network}, addresses),
		FromInternet: true,
		Kind:         kind,
	}
	message, err := sender.Message(ctx, kind, expected.From, strings.Join(addresses, ", "), expected.Subject, token+"@"+sender.Domain(), expected.Body)
	if err != nil {
		return err
	}
	expected.Body = sender.Text(kind, expected.Body)
	if err := ledger.add(expected); err != nil {
		return err
	}
	deliverySettings := *settings.Mail
	deliverySettings.SenderAddress = sender.Source(kind)
	deliverySettings.SenderHelo = sender.settings.MtaHost
	delivery := &WorldMailDelivery{Dns: network.MailDns, From: expected.From, To: addresses, Message: message}
	if err := mailAwaitDelivery(ctx, world, ledger, token, delivery, &deliverySettings); err != nil {
		return fmt.Errorf("the internet sender's %s delivery %s to %s: %w", kind, token, network.Name, err)
	}
	return nil
}

// A receiver that cannot deposit (the file's header's deposits row), at evs's origin in front of the world's lb: while
// armed for a repo, every push or segment upload to it on a token that grants deposit there is answered 500, as an
// evs that cannot take a deposit answers, which a depositor does not retry, so the receiver's session answers 451
// 4.3.0 at once (§15.5); every other request passes. Safe for concurrent use.
type mailDepositFault struct {
	next http.Handler

	stateLock sync.Mutex
	// the armed repos' path prefixes and deposit scopes
	paths  []string
	scopes []string

	refusedCount atomic.Int64
}

// Serves evs's origin through the fault from now on: in place of the world's lb, which serves the block's runs.
func installMailDepositFault(world *World) (*mailDepositFault, error) {
	block := world.Block(WorldBlockEvs)
	if block == nil {
		return nil, errors.New("the world runs no evs")
	}
	fault := &mailDepositFault{next: &worldLb{block: block}}
	api := world.T3().Api()
	if err := api.ServeService(block.service, nil); err != nil {
		return nil, err
	}
	if err := api.ServeService(block.service, fault); err != nil {
		return nil, err
	}
	return fault, nil
}

// Refuses deposits to repoId until Disarm.
func (self *mailDepositFault) Arm(repoId sidecar.Id) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.paths = append(self.paths, "/evs/v1/repos/"+sidecar.EvsKeyId(repoId)+"/")
	self.scopes = append(self.scopes, "repo:"+repoId.String()+":deposit")
}

// Lets every deposit through again.
func (self *mailDepositFault) Disarm() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.paths, self.scopes = nil, nil
}

// How many requests the fault refused.
func (self *mailDepositFault) RefusedCount() int64 {
	return self.refusedCount.Load()
}

func (self *mailDepositFault) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if self.refuses(r) {
		self.refusedCount.Add(1)
		if r.Body != nil {
			r.Body.Close()
		}
		http.Error(w, "the deposit could not be taken", http.StatusInternalServerError)
		return
	}
	self.next.ServeHTTP(w, r)
}

// Whether r is a deposit's push or segment upload to an armed repo: its path's, and a scope of its bearer's, which the
// exchange minted as a JWT, read without a check, since the fault only chooses which requests fail.
func (self *mailDepositFault) refuses(r *http.Request) bool {
	paths, scopes := func() ([]string, []string) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return slices.Clone(self.paths), slices.Clone(self.scopes)
	}()
	if len(paths) == 0 {
		return false
	}
	armed := -1
	for index, path := range paths {
		if strings.HasPrefix(r.URL.Path, path) {
			armed = index
		}
	}
	if armed < 0 {
		return false
	}
	rest := strings.TrimPrefix(r.URL.Path, paths[armed])
	if !(r.Method == http.MethodPost && rest == "push") && !(r.Method == http.MethodPut && strings.HasPrefix(rest, "segments/")) {
		return false
	}
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return false
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	claims := &struct {
		Scope string `json:"scope"`
	}{}
	if err := json.Unmarshal(payload, claims); err != nil {
		return false
	}
	return slices.Contains(strings.Fields(claims.Scope), scopes[armed])
}

// The SMTP reply err carries, nil for none.
func mailSmtpReplyOf(err error) *textproto.Error {
	var reply *textproto.Error
	if errors.As(err, &reply) {
		return reply
	}
	return nil
}

// One mailbox's every entry, paged, in the list's order.
func mailListAll(ctx context.Context, client *sidecar.MailClient, pageCount int) ([]*sidecar.MailViewEntry, error) {
	entries := []*sidecar.MailViewEntry{}
	var after *sidecar.MailListCursor
	for {
		page, err := client.List(ctx, &sidecar.MailListArgs{Folder: sidecar.MailFolderAll, After: after, Limit: pageCount})
		if err != nil {
			return nil, err
		}
		entries = append(entries, page.Entries...)
		if page.Next == nil {
			return entries, nil
		}
		after = page.Next
	}
}

// Whether entry is a sent copy: labeled sent, or the id of a send of ledger's.
func mailSentCopy(entry *sidecar.MailViewEntry, expected *mailExpected) bool {
	if entry.Flags != nil && entry.Flags.HasLabel(sidecar.MailLabelSent) {
		return true
	}
	if expected == nil || expected.Sent == nil {
		return false
	}
	id, err := sidecar.IdFromBytes(entry.Entry.GetId())
	return err == nil && id == expected.Sent.SendId
}

// The entries of the cases a network's reader lists, by token: what each of its mailboxes received, and the sent
// copies. The member's view lists its mailbox's entries and the group messages its r/ references name (MailView), each
// entry naming the repo it is in.
type mailMailboxCounts struct {
	// by token, then by the name of the mailbox the entry's repo is
	received map[string]map[string][]*sidecar.MailViewEntry
	sent     map[string][]*sidecar.MailViewEntry
	// entries whose subject carries no token of the cases'
	foreign []*sidecar.MailViewEntry
	// entries in a repo that is none of the network's mailboxes
	stray []*sidecar.MailViewEntry
}

func mailCountMailbox(network *mailNetwork, entries []*sidecar.MailViewEntry, ledger *mailLedger) *mailMailboxCounts {
	counts := &mailMailboxCounts{received: map[string]map[string][]*sidecar.MailViewEntry{}, sent: map[string][]*sidecar.MailViewEntry{}}
	for _, entry := range entries {
		token, ok := mailTokenOf(entry.Entry.GetSubject())
		if !ok {
			counts.foreign = append(counts.foreign, entry)
			continue
		}
		mailbox := network.MailboxByRepo(entry.RepoId)
		switch {
		case mailbox == nil:
			counts.stray = append(counts.stray, entry)
		case mailSentCopy(entry, ledger.get(token)):
			counts.sent[token] = append(counts.sent[token], entry)
		default:
			if counts.received[token] == nil {
				counts.received[token] = map[string][]*sidecar.MailViewEntry{}
			}
			counts.received[token][mailbox.Name] = append(counts.received[token][mailbox.Name], entry)
		}
	}
	return counts
}

// Waits until network's reader lists every message ledger names each of its mailboxes for, received there, and its
// sent copies, then answers its entries: the reader's ingest runs at each deposit's hint, with no hook a test reads, so
// the list is read every RetryInterval within timeout, the network's prover host woken each time. Past timeout, the
// last complete listing's entries, with what it lacked and how many listings failed, the last one's error among them.
func awaitMailMailbox(ctx context.Context, network *mailNetwork, ledger *mailLedger, settings *mailConcurrencySettings, timeout time.Duration) ([]*sidecar.MailViewEntry, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var listed []*sidecar.MailViewEntry
	missing := []string{}
	listCount, failCount := 0, 0
	var lastErr error
	for {
		entries, err := mailListAll(ctx, network.ReaderMail, settings.ListPageCount)
		listCount += 1
		if err != nil {
			failCount += 1
			lastErr = err
		} else {
			listed = entries
			missing = []string{}
			counts := mailCountMailbox(network, entries, ledger)
			for _, expected := range ledger.all() {
				for _, mailbox := range network.Mailboxes {
					if slices.Contains(expected.Mailboxes, mailbox.Name) && len(counts.received[expected.Token][mailbox.Name]) == 0 {
						missing = append(missing, expected.Token+" in "+mailbox.Name)
					}
				}
				if expected.Network == network && len(counts.sent[expected.Token]) == 0 {
					missing = append(missing, expected.Token+" (its sent copy)")
				}
			}
			if len(missing) == 0 {
				return entries, nil
			}
		}
		network.WakeProver()
		select {
		case <-ctx.Done():
			if listed == nil {
				return nil, fmt.Errorf("%s's reader never completed a listing of its mailboxes: %d of %d listings failed, the last with %v (%w)", network.Name, failCount, listCount, lastErr, ctx.Err())
			}
			return listed, fmt.Errorf("%s's reader never listed %d messages %v, at its last complete listing; %d of %d listings failed, the last failure %v (%w)", network.Name, len(missing), missing, failCount, listCount, lastErr, ctx.Err())
		case <-time.After(settings.Mail.RetryInterval):
		}
	}
}

// Waits until the internet MX has taken every message ledger names it for, and answers its transactions.
func awaitMailInternetMx(ctx context.Context, world *World, ledger *mailLedger, networks []*mailNetwork, settings *mailConcurrencySettings, timeout time.Duration) ([]*mailMxMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		messages, err := mailMxMessages(world)
		missing := []string{}
		if err == nil {
			taken := map[string]int{}
			for _, message := range messages {
				taken[message.Token] += 1
			}
			for _, expected := range ledger.all() {
				if expected.Internet && taken[expected.Token] == 0 {
					missing = append(missing, expected.Token)
				}
			}
			if len(missing) == 0 {
				return messages, nil
			}
		}
		for _, network := range networks {
			network.WakeProver()
		}
		select {
		case <-ctx.Done():
			return messages, fmt.Errorf("the internet MX never took %d messages %v: %v (%w)", len(missing), missing, err, ctx.Err())
		case <-time.After(settings.Mail.RetryInterval):
		}
	}
}

// One message the internet MX took, as its transaction holds it.
type mailMxMessage struct {
	Token       string
	Transaction *sidecartest.MxTransaction
	Header      mail.Header
}

// Every transaction the internet MX took, each parsed; an error for one that does not parse.
func mailMxMessages(world *World) ([]*mailMxMessage, error) {
	messages := []*mailMxMessage{}
	for _, transaction := range world.dnssec.sender.Mx().Transactions() {
		parsed, err := mail.ReadMessage(bytes.NewReader(transaction.Message))
		if err != nil {
			return nil, fmt.Errorf("a message the internet MX took: %w", err)
		}
		token, _ := mailTokenOf(parsed.Header.Get("Subject"))
		messages = append(messages, &mailMxMessage{Token: token, Transaction: transaction, Header: parsed.Header})
	}
	return messages, nil
}

// Waits until every send of ledger's that queued internet mail holds a sent outcome for each of its domains in its
// member's or/, as SendResult reads it, and answers each by token.
func awaitMailOutcomes(ctx context.Context, world *World, ledger *mailLedger, settings *mailConcurrencySettings) (map[string]*sidecar.SmtpSendResult, error) {
	results := map[string]*sidecar.SmtpSendResult{}
	for _, expected := range ledger.all() {
		if expected.Sent == nil || len(expected.Domains) == 0 {
			continue
		}
		result, err := world.AwaitMailSendResult(ctx, expected.Network.Prover(), expected.Network.SenderMails[0], expected.Sent.SendId, func(result *sidecar.SmtpSendResult) bool {
			if len(result.Domains) != len(expected.Domains) {
				return false
			}
			for _, outcome := range result.Domains {
				if outcome.Outcome != sidecar.SmtpOutcomeSent {
					return false
				}
			}
			return true
		}, settings.Mail)
		if err != nil {
			logLines := expected.Network.Prover().LogLines(world)
			return results, fmt.Errorf("%s's send %s to %v: %w; its outcomes %s; its prover host's attempts: %s; its prover host's newest log lines:\n    | %s", expected.Device.Name, expected.Token, expected.Domains, err, mailDescribeOutcomes(result), expected.Network.DescribeSend(expected.Sent.SendId), strings.Join(logLines, "\n    | "))
		}
		results[expected.Token] = result
	}
	return results, nil
}

// A message read back from a mailbox.
type mailRead struct {
	Entry *sidecar.MailViewEntry
	Read  *sidecar.MailReadResult
}

func (self *mailRead) Header(name string) string {
	for _, header := range self.Read.Headers {
		if strings.EqualFold(header.Name, name) {
			return header.Value
		}
	}
	return ""
}

// entry read back through client.
func mailReadEntry(ctx context.Context, client *sidecar.MailClient, entry *sidecar.MailViewEntry) (*mailRead, error) {
	id, err := sidecar.IdFromBytes(entry.Entry.GetId())
	if err != nil {
		return nil, err
	}
	read, err := client.Read(ctx, &sidecar.MailReadArgs{Month: sidecar.MailYearMonthOf(entry.Entry.GetTime()), Id: id})
	if err != nil {
		return nil, err
	}
	return &mailRead{Entry: entry, Read: read}, nil
}

// The verdicts the receiver must seal for the internet sender's kind, and how a reader files it (the file's header).
type mailKindVerdicts struct {
	Spf   protocol.SpfVerdict
	Dkim  protocol.DkimVerdict
	Dmarc protocol.DmarcVerdict
	// the forward-confirmed PTR ingest's Authentication-Results names, "" for none
	Ptr  string
	Spam bool
}

func mailVerdictsOf(world *World, kind worldSenderKind) *mailKindVerdicts {
	mta := world.dnssec.sender.settings.MtaHost
	switch kind {
	case worldSenderSpfFail:
		return &mailKindVerdicts{Spf: protocol.SpfVerdict_SPF_FAIL, Dkim: protocol.DkimVerdict_DKIM_PASS, Dmarc: protocol.DmarcVerdict_DMARC_PASS}
	case worldSenderDkimFail:
		return &mailKindVerdicts{Spf: protocol.SpfVerdict_SPF_PASS, Dkim: protocol.DkimVerdict_DKIM_FAIL, Dmarc: protocol.DmarcVerdict_DMARC_PASS, Ptr: mta}
	case worldSenderDmarcFail:
		return &mailKindVerdicts{Spf: protocol.SpfVerdict_SPF_FAIL, Dkim: protocol.DkimVerdict_DKIM_FAIL, Dmarc: protocol.DmarcVerdict_DMARC_FAIL, Spam: true}
	}
	return &mailKindVerdicts{Spf: protocol.SpfVerdict_SPF_PASS, Dkim: protocol.DkimVerdict_DKIM_PASS, Dmarc: protocol.DmarcVerdict_DMARC_PASS, Ptr: mta}
}

// Holds read, a received message of expected's, to what it must read as: its text and sender as sent, outside mail at
// its reader, and its verdicts, the internet sender's kind's or, for a network's internet mail, DKIM's pass under the
// sending network's key; a same-network deposit's from a member, with no reception evidence.
func mailCheckRead(world *World, network *mailNetwork, expected *mailExpected, read *mailRead) error {
	where := fmt.Sprintf("%s's copy of %s", network.Name, expected.Token)
	if text := strings.TrimSpace(read.Read.Text); text != expected.Body {
		return fmt.Errorf("%s reads its text as %q, not %q", where, text, expected.Body)
	}
	if from := read.Header("From"); !strings.Contains(strings.ToLower(from), strings.ToLower(expected.From)) {
		return fmt.Errorf("%s reads its From as %q, not %s", where, from, expected.From)
	}
	results := read.Entry.Entry.GetAuthResults()
	switch {
	case expected.FromInternet:
		want := mailVerdictsOf(world, expected.Kind)
		authenticationResults := read.Header("Authentication-Results")
		switch {
		case read.Read.Trust != sidecar.MailTrustUntrusted:
			return fmt.Errorf("%s, from the internet, reads as %s", where, read.Read.Trust)
		case results == nil:
			return fmt.Errorf("%s, from the internet, has no reception verdicts", where)
		case results.GetSpf() != want.Spf || results.GetDkim() != want.Dkim || results.GetDmarc() != want.Dmarc:
			return fmt.Errorf("%s, from the internet sender's %s, reads spf %s, dkim %s, dmarc %s, not %s, %s, %s", where, expected.Kind, results.GetSpf(), results.GetDkim(), results.GetDmarc(), want.Spf, want.Dkim, want.Dmarc)
		case results.GetChecksIncomplete() || results.GetEvidenceUnavailable():
			return fmt.Errorf("%s's checks read incomplete %t, evidence unavailable %t", where, results.GetChecksIncomplete(), results.GetEvidenceUnavailable())
		case want.Ptr != "" && !strings.Contains(authenticationResults, "iprev=pass") || want.Ptr != "" && !strings.Contains(authenticationResults, "("+want.Ptr+")"):
			return fmt.Errorf("%s's Authentication-Results %q names no forward-confirmed PTR %s", where, authenticationResults, want.Ptr)
		case want.Ptr == "" && strings.Contains(authenticationResults, "iprev="):
			return fmt.Errorf("%s's Authentication-Results %q names a PTR its forged address has none of", where, authenticationResults)
		case read.Entry.Spam != want.Spam || read.Read.Spam != want.Spam:
			return fmt.Errorf("%s, the %s kind, files as spam %t (list %t, score %d), not %t", where, expected.Kind, read.Read.Spam, read.Entry.Spam, read.Entry.Entry.GetSpamScore(), want.Spam)
		}
	case expected.Network == network:
		if read.Read.Trust != sidecar.MailTrustMember || results != nil {
			return fmt.Errorf("%s, a same-network deposit, reads as %s with verdicts %v", where, read.Read.Trust, results)
		}
	default:
		if read.Read.Trust != sidecar.MailTrustUntrusted || results == nil || results.GetDkim() != protocol.DkimVerdict_DKIM_PASS || results.GetChecksIncomplete() {
			return fmt.Errorf("%s, internet mail from %s, reads as %s with verdicts %v, not DKIM's pass under its network's key", where, expected.Network.Name, read.Read.Trust, results)
		}
		if authenticationResults := read.Header("Authentication-Results"); !strings.Contains(authenticationResults, "header.d="+expected.Network.Domain) {
			return fmt.Errorf("%s's Authentication-Results %q names no DKIM signature of %s", where, authenticationResults, expected.Network.Domain)
		}
	}
	return nil
}

// The receiver's own checks of a message the internet MX took (MailAuthChecker over the world's DNS): DKIM must pass
// under the sending network's key, d= its domain.
func mailCheckMxDkim(ctx context.Context, world *World, expected *mailExpected, message *mailMxMessage) error {
	authSettings := sidecar.DefaultMailAuthSettings()
	checker := sidecar.NewMailAuthChecker(sidecar.NewMailDnsStub(world.dnssec, nil, authSettings), nil, authSettings)
	hostsSettings := world.HostsSettings()
	outcome, err := checker.Check(ctx, &sidecar.MailAuthInput{
		Message:  bytes.NewReader(message.Transaction.Message),
		Size:     int64(len(message.Transaction.Message)),
		SenderIp: hostsSettings.SmtpAddress,
		MailFrom: []byte(strings.Trim(message.Transaction.Sender, "<>")),
		EhloName: hostsSettings.SmtpHeloName,
	})
	if err != nil {
		return err
	}
	for _, dkim := range outcome.Dkim {
		if dkim.GetResult() == protocol.DkimVerdict_DKIM_PASS && strings.EqualFold(dkim.GetDomain(), expected.Network.Domain) {
			return nil
		}
	}
	return fmt.Errorf("the internet MX's copy of %s carries no DKIM signature of %s that passes: %v", expected.Token, expected.Network.Domain, outcome.Dkim)
}

// The internet sender's verdicts, one delivery a kind into one network, each read back with its kind's verdicts and
// filing (sub-case 2 of the case's brief), and the other way: the network's mail to the internet sender's domain,
// which smtpedge sends to its MX under TL1, DANE held to the TLSA the world signs, and which the MX takes once,
// signed by the network's DKIM key.
func TestE2eMailInternetSenderVerdicts(t *testing.T) {
	settings := defaultMailConcurrencySettings()
	settings.NetworkCount = 1
	settings.ClientsPerNetwork = 3
	// the verdicts are the member mailbox's to read; the main case holds the groups
	settings.Groups = nil
	e2eTestEnv().Run(t, func(t testing.TB) {
		world := newMailWorld(t, settings)
		defer world.Close()
		ctx, cancel := world.StepContext(t.Context(), settings.PhaseTimeout)
		networks := addMailNetworks(ctx, t, world, settings)
		cancel()
		ctx, cancel = world.StepContext(t.Context(), settings.PhaseTimeout)
		defer cancel()
		network := setUpMailNetworks(ctx, t, world, settings, networks)[0]
		ledger := newMailLedger()
		for _, kind := range worldSenderKinds() {
			if err := mailInternetDeliver(ctx, world, settings, ledger, kind, network, "verdicts-"+kind.String(), network.Addresses[:1]); err != nil {
				world.FailProgress(t, "the "+kind.String()+" delivery", err, nil)
			}
		}
		if err := mailNetworkSend(ctx, world, settings, ledger, []*mailNetwork{network}, network, 0, "verdicts-outbound", []string{world.dnssec.sender.Address(settings.InternetTo)}); err != nil {
			t.Fatal(err)
		}
		entries, err := awaitMailMailbox(ctx, network, ledger, settings, settings.PhaseTimeout)
		if err != nil {
			world.FailProgress(t, network.Name+"'s mailbox", err, nil)
		}
		counts := mailCountMailbox(network, entries, ledger)
		for _, kind := range worldSenderKinds() {
			token := "verdicts-" + kind.String()
			received := counts.received[token][network.Mailboxes[0].Name]
			if len(received) != 1 {
				t.Fatalf("%s's mailbox holds %d copies of the %s delivery", network.Name, len(received), kind)
			}
			read, err := mailReadEntry(ctx, network.ReaderMail, received[0])
			if err != nil {
				t.Fatalf("%s's read of %s: %v", network.Reader.Name, token, err)
			}
			if err := mailCheckRead(world, network, ledger.get(token), read); err != nil {
				t.Fatal(err)
			}
			t.Logf("the %s delivery reads %v, spam score %d, %q", kind, read.Entry.Entry.GetAuthResults(), read.Entry.Entry.GetSpamScore(), read.Header("Authentication-Results"))
		}
		outcomes, err := awaitMailOutcomes(ctx, world, ledger, settings)
		if err != nil {
			world.FailProgress(t, "the send to the internet", err, nil)
		}
		outcome := outcomes["verdicts-outbound"].Domains[0]
		if outcome.Domain != world.dnssec.sender.Domain() || outcome.TlsLevel != sidecar.SmtpTlsLevelDane {
			t.Fatalf("the send to the internet ended %+v, not sent under DANE to %s", *outcome, world.dnssec.sender.Domain())
		}
		messages, err := awaitMailInternetMx(ctx, world, ledger, []*mailNetwork{network}, settings, settings.PhaseTimeout)
		if err != nil {
			t.Fatal(err)
		}
		if len(messages) != 1 || messages[0].Token != "verdicts-outbound" {
			t.Fatalf("the internet MX took %d messages, not the one send", len(messages))
		}
		if recipients := messages[0].Transaction.Recipients; len(recipients) != 1 || !strings.EqualFold(strings.Trim(recipients[0], "<>"), world.dnssec.sender.Address(settings.InternetTo)) {
			t.Fatalf("the internet MX took the send for %v", recipients)
		}
		if err := mailCheckMxDkim(ctx, world, ledger.get("verdicts-outbound"), messages[0]); err != nil {
			t.Fatal(err)
		}
	})
}

// The case (the file's header).
func TestE2eMailConcurrency(t *testing.T) {
	settings := defaultMailConcurrencySettings()
	e2eTestEnv().Run(t, func(t testing.TB) {
		world := newMailWorld(t, settings)
		defer world.Close()
		fault, err := installMailDepositFault(world)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := world.StepContext(t.Context(), settings.PhaseTimeout)
		worldNetworks := addMailNetworks(ctx, t, world, settings)
		cancel()
		ctx, cancel = world.StepContext(t.Context(), settings.PhaseTimeout)
		networks := setUpMailNetworks(ctx, t, world, settings, worldNetworks)
		cancel()
		ledger := newMailLedger()

		// the sends, the internet sender's deliveries and the chaos, all at once: the sends at their bursts' own pace, and
		// each delivery and each restart once the prover hosts have started its share of the SMTP attempts the sends
		// make, so every restart falls while connections are in flight and the deliveries spread across them
		attemptCount := 0
		// the send bursts, and how many have ended, each at its last step or its first error
		sendBurstCount := 0
		sendBurstsEnded := atomic.Int64{}
		bursts := []*WorldBurst{}
		for networkIndex, network := range networks {
			others := []*mailNetwork{}
			for _, other := range networks {
				if other != network {
					others = append(others, other)
				}
			}
			for senderIndex, device := range network.Senders {
				// one attempt at the least for each domain a send names outside its own network
				for index := range settings.SendsPerDevice {
					if 0 < len(others) {
						attemptCount += 1
					}
					if index%2 == 0 {
						attemptCount += 1
					}
				}
				sendBurstCount += 1
				bursts = append(bursts, &WorldBurst{
					Name:      device.Name + "'s sends",
					StepCount: settings.SendsPerDevice,
					Step: func(ctx context.Context, index int, random *rand.Rand) error {
						// an address of one of its own network's mailboxes, its aliases' or, with OwnGroupSends, a
						// group's too, one of another network's, alias or group, and every other send the internet
						// sender's inbox
						ownAddresses := network.Addresses
						if settings.OwnGroupSends {
							ownAddresses = network.AllAddresses()
						}
						recipients := []string{ownAddresses[random.IntN(len(ownAddresses))]}
						if 0 < len(others) {
							other := others[(networkIndex+senderIndex+index)%len(others)]
							otherAddresses := other.AllAddresses()
							recipients = append(recipients, otherAddresses[random.IntN(len(otherAddresses))])
						}
						if index%2 == 0 {
							recipients = append(recipients, world.dnssec.sender.Address(settings.InternetTo))
						}
						token := fmt.Sprintf("%s-%d", device.Name, index)
						err := mailNetworkSend(ctx, world, settings, ledger, networks, network, senderIndex, token, recipients)
						if err != nil || index == settings.SendsPerDevice-1 {
							sendBurstsEnded.Add(1)
						}
						return err
					},
				})
			}
		}
		// the attempts every prover host has started, which have no hook to wait on, so they are counted every
		// RetryInterval: a share of those the sends plan while any send burst runs, and once every one has ended, of
		// those the sends queued, which a send that failed before it queued never makes
		attempts := func() int {
			count := 0
			for _, network := range networks {
				count += network.AttemptCount()
			}
			return count
		}
		plannedAttempts := func() int {
			if int(sendBurstsEnded.Load()) < sendBurstCount {
				return attemptCount
			}
			queued := 0
			for _, expected := range ledger.all() {
				if expected.Sent != nil {
					queued += len(expected.Domains)
				}
			}
			return min(attemptCount, queued)
		}
		awaitAttemptShare := func(ctx context.Context, numerator int, denominator int) error {
			for attempts() < numerator*plannedAttempts()/denominator {
				select {
				case <-ctx.Done():
					return fmt.Errorf("the prover hosts started %d SMTP attempts, short of %d/%d of the %d planned: %w", attempts(), numerator, denominator, plannedAttempts(), ctx.Err())
				case <-time.After(settings.Mail.RetryInterval):
				}
			}
			return nil
		}
		kinds := worldSenderKinds()
		deliveryCount := len(kinds) * settings.InternetDeliveriesPerKind
		for kindIndex, kind := range kinds {
			bursts = append(bursts, &WorldBurst{
				Name:      "the internet sender's " + kind.String() + " deliveries",
				StepCount: settings.InternetDeliveriesPerKind,
				Step: func(ctx context.Context, index int, random *rand.Rand) error {
					if err := awaitAttemptShare(ctx, index*len(kinds)+kindIndex, deliveryCount+1); err != nil {
						return err
					}
					network := networks[(kindIndex+index)%len(networks)]
					allAddresses := network.AllAddresses()
					addresses := []string{allAddresses[random.IntN(len(allAddresses))]}
					if index%2 == 1 {
						// every address of the network: one deposit in each of its mailboxes, the member's once
						// however many of its aliases the transaction names
						addresses = allAddresses
					}
					return mailInternetDeliver(ctx, world, settings, ledger, kind, network, fmt.Sprintf("internet-%s-%d", kind, index), addresses)
				},
			})
		}
		chaosCount := len(settings.ChaosBlocks)
		if settings.ChaosSender {
			chaosCount += 1
		}
		chaos := &WorldBurst{
			Name:      "the chaos",
			StepCount: chaosCount,
			Step: func(ctx context.Context, index int, random *rand.Rand) error {
				if err := awaitAttemptShare(ctx, index+1, chaosCount+1); err != nil {
					return err
				}
				if index < len(settings.ChaosBlocks) {
					t.Logf("chaos restarts %s once the prover hosts have started %d SMTP attempts", settings.ChaosBlocks[index], attempts())
					return world.Restart(settings.ChaosBlocks[index])
				}
				unended, err := networks[0].RestartProver(ctx, world, settings)
				if err != nil {
					return fmt.Errorf("%s's prover host's restart: %w", networks[0].Name, err)
				}
				ledger.addResendable(unended)
				t.Logf("%s's prover host restarted with %d sends unended in its member's or/, the prover hosts having started %d SMTP attempts", networks[0].Name, len(unended), attempts())
				return nil
			},
		}
		bursts = append(bursts, chaos)
		ctx, cancel = world.StepContext(t.Context(), settings.PhaseTimeout)
		if err := RunBursts(ctx, t, "the mail", bursts, settings.Bursts); err != nil {
			cancel()
			world.FailProgress(t, "the mail", err, bursts)
		}
		cancel()
		FailBurstErrors(t, bursts)
		if t.Failed() {
			t.FailNow()
		}
		for _, name := range settings.ChaosBlocks {
			if runs := world.Block(name).RunCount(); runs != 2 {
				t.Fatalf("%s ran %d times through the chaos, not 2", name, runs)
			}
		}

		// every send's internet mail ended sent, and the internet MX took what names it
		ctx, cancel = world.StepContext(t.Context(), settings.PhaseTimeout)
		if _, err := awaitMailOutcomes(ctx, world, ledger, settings); err != nil {
			cancel()
			world.FailProgress(t, "the sends' outcomes", err, nil)
		}
		if _, err := awaitMailInternetMx(ctx, world, ledger, networks, settings, settings.PhaseTimeout); err != nil {
			cancel()
			world.FailProgress(t, "the internet MX", err, nil)
		}
		cancel()
		// and every mailbox, each network's wait bounded on its own, so a reader that never lists a message it should
		// fails the case without stopping the rest: its checks below name each copy it lacks
		stalled := map[string]bool{}
		for _, network := range networks {
			mailboxCtx, cancelMailbox := world.StepContext(t.Context(), settings.MailboxTimeout)
			if _, err := awaitMailMailbox(mailboxCtx, network, ledger, settings, settings.MailboxTimeout); err != nil {
				t.Errorf("%v", err)
				stalled[network.Name] = true
			}
			cancelMailbox()
		}

		// a receiver that cannot deposit answers 451 4.3.0, and the internet sender's retry and a network's land once
		// it can again
		ctx, cancel = world.StepContext(t.Context(), settings.PhaseTimeout)
		mailCheckDepositRefusal(ctx, t, world, settings, ledger, fault, networks)
		cancel()

		// the checks
		ctx, cancel = world.StepContext(t.Context(), settings.PhaseTimeout)
		defer cancel()
		outcomes, err := awaitMailOutcomes(ctx, world, ledger, settings)
		if err != nil {
			world.FailProgress(t, "the sends' outcomes", err, nil)
		}
		mxMessages, err := awaitMailInternetMx(ctx, world, ledger, networks, settings, settings.PhaseTimeout)
		if err != nil {
			world.FailProgress(t, "the internet MX", err, nil)
		}
		mxByteCounts, smtpByteCounts := map[string]int64{}, map[string]int64{}
		for _, network := range networks {
			var entries []*sidecar.MailViewEntry
			if stalled[network.Name] {
				// its stall failed the case above: read as it stands, for the checks to name what it lacks
				entries, err = mailListAll(ctx, network.ReaderMail, settings.ListPageCount)
			} else {
				mailboxCtx, cancelMailbox := world.StepContext(t.Context(), settings.MailboxTimeout)
				entries, err = awaitMailMailbox(mailboxCtx, network, ledger, settings, settings.MailboxTimeout)
				cancelMailbox()
			}
			if err != nil {
				t.Errorf("%s's mailboxes at the checks: %v", network.Name, err)
			}
			mailCheckMailbox(ctx, t, world, network, ledger, entries, mxByteCounts, smtpByteCounts)
		}
		mailCheckInternetMx(ctx, t, world, ledger, world.dnssec.sender.Address(settings.InternetTo), mxMessages, smtpByteCounts)
		for _, network := range networks {
			mailCheckOutbox(ctx, t, network, ledger, outcomes)
		}

		// the ledgers, once every block that metered the mail has drained, whatever the checks above found
		for _, name := range settings.FinalDrainBlocks {
			if err := world.Restart(name); err != nil {
				world.FailProgress(t, name+"'s restart for its drain", err, nil)
			}
		}
		ledgers, err := world.ReadLedgers(ctx, time.Time{})
		if err != nil {
			t.Fatalf("the ledgers: %v", err)
		}
		for _, network := range networks {
			quota := ledgers.Networks[network.NetworkId]
			meters := ledgers.Meters
			switch {
			case quota == nil:
				t.Fatalf("%s has no quota ledger", network.Name)
			case quota.Spent || quota.BalanceByteCount < 0:
				t.Fatalf("%s's balance at the end: %+v", network.Name, quota)
			case meters.TotalByteCount(network.NetworkId) != quota.DebitedByteCount():
				t.Fatalf("%s's balances were debited %d, its meters: %s", network.Name, quota.DebitedByteCount(), meters.Describe(network.NetworkId))
			case meters.ByteCount(network.NetworkId, sidecar.QuotaMeterMx) < mxByteCounts[network.Name]:
				t.Fatalf("%s's mx meter holds %d, under the %d bytes of the mail it took through mxedge: %s", network.Name, meters.ByteCount(network.NetworkId, sidecar.QuotaMeterMx), mxByteCounts[network.Name], meters.Describe(network.NetworkId))
			case meters.ByteCount(network.NetworkId, sidecar.QuotaMeterSmtp) < smtpByteCounts[network.Name]:
				t.Fatalf("%s's smtp meter holds %d, under the %d bytes of the mail it sent through smtpedge: %s", network.Name, meters.ByteCount(network.NetworkId, sidecar.QuotaMeterSmtp), smtpByteCounts[network.Name], meters.Describe(network.NetworkId))
			case mxByteCounts[network.Name] == 0 || smtpByteCounts[network.Name] == 0:
				t.Fatalf("%s took %d bytes through mxedge and sent %d through smtpedge: the case moved none", network.Name, mxByteCounts[network.Name], smtpByteCounts[network.Name])
			}
			t.Logf("%s's meters at the end: %s; its mail took %d bytes through mxedge and sent %d through smtpedge", network.Name, meters.Describe(network.NetworkId), mxByteCounts[network.Name], smtpByteCounts[network.Name])
		}
	})
}

// The deposits phase (the file's header): the fault armed on the second network's mailbox, the internet sender's
// delivery there answered 451 4.3.0 and the first network's send there deferred with its 451, then the fault lifted,
// and both landing on their retries, each kept in ledger for the final checks.
func mailCheckDepositRefusal(ctx context.Context, t testing.TB, world *World, settings *mailConcurrencySettings, ledger *mailLedger, fault *mailDepositFault, networks []*mailNetwork) {
	if len(networks) < 2 {
		t.Fatalf("the deposits phase sends from one network to another, and there are %d", len(networks))
	}
	from, target := networks[0], networks[1]
	fault.Arm(target.MailboxId)
	armed := true
	defer func() {
		if armed {
			fault.Disarm()
		}
	}()
	sender := world.dnssec.sender
	kind := worldSenderAligned
	token := "deposits-internet"
	expected := &mailExpected{
		Token:        token,
		Subject:      mailSubjectPrefix + token,
		From:         sender.Address(kind.String()),
		Body:         "The internet sender's delivery a receiver could not deposit at first.",
		Recipients:   target.Addresses[:1],
		Mailboxes:    mailMailboxNames([]*mailNetwork{target}, target.Addresses[:1]),
		FromInternet: true,
		Kind:         kind,
	}
	message, err := sender.Message(ctx, kind, expected.From, target.Addresses[0], expected.Subject, token+"@"+sender.Domain(), expected.Body)
	if err != nil {
		t.Fatal(err)
	}
	expected.Body = sender.Text(kind, expected.Body)
	if err := ledger.add(expected); err != nil {
		t.Fatal(err)
	}
	deliverySettings := *settings.Mail
	deliverySettings.SenderAddress = sender.Source(kind)
	deliverySettings.SenderHelo = sender.settings.MtaHost
	delivery := &WorldMailDelivery{Dns: target.MailDns, From: expected.From, To: target.Addresses[:1], Message: message}
	// the session reaches DATA's end and is answered 451 4.3.0, the receiver's deposit refused; any other refusal on
	// the way is tried again, as a sender retries it
	refusalCtx, cancelRefusal := context.WithTimeout(ctx, settings.Mail.Timeout)
	defer cancelRefusal()
	for {
		err := world.DeliverMail(refusalCtx, delivery, &deliverySettings)
		if err == nil {
			t.Fatalf("%s's receiver answered 250 while its mailbox took no deposit (%d requests refused)", target.Name, fault.RefusedCount())
		}
		if reply := mailSmtpReplyOf(err); reply != nil && reply.Code == 451 && strings.HasPrefix(reply.Msg, "4.3.0") && strings.Contains(err.Error(), "the DATA's end") {
			t.Logf("%s's receiver answered the delivery it could not deposit %d %s", target.Name, reply.Code, reply.Msg)
			break
		}
		select {
		case <-refusalCtx.Done():
			t.Fatalf("%s's receiver never answered 451 4.3.0 while it could not deposit: %v (%v)", target.Name, err, refusalCtx.Err())
		case <-time.After(settings.Mail.RetryInterval):
		}
	}
	// the first network's send there: the receiver's session answers its DATA's end 451 4.3.0 as it answered the
	// internet sender's, its deposit refused, and its SmtpSender holds the domain deferred for a retry
	refusedBefore := fault.RefusedCount()
	if err := mailNetworkSend(ctx, world, settings, ledger, networks, from, 0, "deposits-network", []string{target.Addresses[0]}); err != nil {
		t.Fatal(err)
	}
	sent := ledger.get("deposits-network").Sent
	deferred, err := world.AwaitMailSendResult(ctx, from.Prover(), from.SenderMails[0], sent.SendId, func(result *sidecar.SmtpSendResult) bool {
		return len(result.Domains) == 1 && result.Domains[0].Outcome == sidecar.SmtpOutcomeDeferred && refusedBefore < fault.RefusedCount()
	}, settings.Mail)
	if err != nil {
		world.FailProgress(t, "the send a receiver could not deposit", fmt.Errorf("%w; the fault refused %d requests, %d before the send", err, fault.RefusedCount(), refusedBefore), nil)
	}
	fault.Disarm()
	armed = false
	t.Logf("%s's send to %s was deferred %+v once its deposit was refused; the fault refused %d requests before it lifted", from.Name, target.Name, *deferred.Domains[0], fault.RefusedCount())
	if err := mailAwaitDelivery(ctx, world, ledger, token, delivery, &deliverySettings); err != nil {
		world.FailProgress(t, "the internet sender's retry", err, nil)
	}
	if _, err := world.AwaitMailSendResult(ctx, from.Prover(), from.SenderMails[0], sent.SendId, func(result *sidecar.SmtpSendResult) bool {
		return len(result.Domains) == 1 && result.Domains[0].Outcome == sidecar.SmtpOutcomeSent
	}, settings.Mail); err != nil {
		world.FailProgress(t, "the network's retry", err, nil)
	}
}

// Holds network's mailboxes to ledger (the file's header): each message received exactly once in each mailbox it
// names, the one exception aside, and in no other, nothing in a repo that is none of the network's mailboxes, every
// sent copy its own member's send's and each such send's kept once, and every received copy read back and verified;
// adds the bytes of each message the network took through mxedge to its count, once however many of its mailboxes took
// it, and of each a network sent it through smtpedge to the sender's.
func mailCheckMailbox(ctx context.Context, t testing.TB, world *World, network *mailNetwork, ledger *mailLedger, entries []*sidecar.MailViewEntry, mxByteCounts map[string]int64, smtpByteCounts map[string]int64) {
	counts := mailCountMailbox(network, entries, ledger)
	for _, entry := range counts.foreign {
		t.Errorf("%s's reader lists %q, which no sender of the case sent", network.Name, entry.Entry.GetSubject())
	}
	for _, entry := range counts.stray {
		t.Errorf("%s's reader lists %q in repo %s, none of the network's mailboxes", network.Name, entry.Entry.GetSubject(), entry.RepoId)
	}
	for _, expected := range ledger.all() {
		var taken *sidecar.MailViewEntry
		for _, mailbox := range network.Mailboxes {
			received := counts.received[expected.Token][mailbox.Name]
			names := slices.Contains(expected.Mailboxes, mailbox.Name)
			switch {
			case !names && 0 < len(received):
				t.Errorf("%s received %d copies of %s, which names only %v: another mailbox's mail", mailbox.Name, len(received), expected.Token, expected.Mailboxes)
			case names && len(received) == 2 && expected.Network != network && ledger.mayRepeat(expected, network.Domain) != "":
				t.Logf("%s received %s twice: %s", mailbox.Name, expected.Token, ledger.mayRepeat(expected, network.Domain))
			case names && len(received) != 1:
				t.Errorf("%s received %d copies of %s, not 1", mailbox.Name, len(received), expected.Token)
			}
			if !names || len(received) == 0 {
				continue
			}
			taken = received[0]
			read, err := mailReadEntry(ctx, network.ReaderMail, received[0])
			if err != nil {
				t.Errorf("%s's read of %s in %s: %v", network.Reader.Name, expected.Token, mailbox.Name, err)
				continue
			}
			if err := mailCheckRead(world, network, expected, read); err != nil {
				t.Errorf("%s: %v", mailbox.Name, err)
			}
		}
		sentCopies := counts.sent[expected.Token]
		switch {
		case expected.Network == network && len(sentCopies) != 1:
			t.Errorf("%s's member mailbox holds %d sent copies of its send %s, not 1", network.Name, len(sentCopies), expected.Token)
		case expected.Network == network && sentCopies[0].RepoId != network.MailboxId:
			t.Errorf("%s keeps its send %s's sent copy in repo %s, not its member's mailbox", network.Name, expected.Token, sentCopies[0].RepoId)
		case expected.Network != network && 0 < len(sentCopies):
			t.Errorf("%s's member mailbox holds %d sent copies of %s, which another sender sent", network.Name, len(sentCopies), expected.Token)
		}
		if taken == nil {
			continue
		}
		size := int64(taken.Entry.GetSize())
		switch {
		case expected.FromInternet:
			mxByteCounts[network.Name] += size
		case expected.Network != network:
			mxByteCounts[network.Name] += size
			smtpByteCounts[expected.Network.Name] += size
		}
	}
}

// Holds the internet MX to ledger: each send naming the internet sender's domain taken exactly once, the one
// exception aside, from its network's bounce address under the network's DKIM key, and nothing else; adds each
// message's bytes to its sender's smtpedge count.
func mailCheckInternetMx(ctx context.Context, t testing.TB, world *World, ledger *mailLedger, inbox string, messages []*mailMxMessage, smtpByteCounts map[string]int64) {
	taken := map[string][]*mailMxMessage{}
	for _, message := range messages {
		expected := ledger.get(message.Token)
		if expected == nil || !expected.Internet {
			t.Errorf("the internet MX took %q, which no send named it for", message.Header.Get("Subject"))
			continue
		}
		taken[message.Token] = append(taken[message.Token], message)
	}
	for _, expected := range ledger.all() {
		if !expected.Internet {
			continue
		}
		messages := taken[expected.Token]
		switch {
		case len(messages) == 2 && ledger.mayRepeat(expected, world.dnssec.sender.Domain()) != "":
			t.Logf("the internet MX took %s twice: %s", expected.Token, ledger.mayRepeat(expected, world.dnssec.sender.Domain()))
		case len(messages) != 1:
			t.Errorf("the internet MX took %s %d times, not once", expected.Token, len(messages))
			continue
		}
		message := messages[0]
		if recipients := message.Transaction.Recipients; len(recipients) != 1 || !strings.EqualFold(strings.Trim(recipients[0], "<>"), inbox) {
			t.Errorf("the internet MX took %s for %v, not %s", expected.Token, recipients, inbox)
		}
		if mailFrom := strings.ToLower(strings.Trim(message.Transaction.Sender, "<>")); !strings.HasSuffix(mailFrom, "@b."+expected.Network.Domain) {
			t.Errorf("the internet MX took %s from %s, not from %s's bounce domain b.%s (§14.6)", expected.Token, mailFrom, expected.Network.Name, expected.Network.Domain)
		}
		if err := mailCheckMxDkim(ctx, world, expected, message); err != nil {
			t.Errorf("%v", err)
		}
		smtpByteCounts[expected.Network.Name] += int64(len(message.Transaction.Message))
	}
}

// Holds network's member's outbox to ledger: one or/ record a send that queued internet mail, its effect key the one
// the send was answered, each of its domains sent, and no record for any other key; and each o/ entry still kept a
// send's, signed by the device that sent it, naming that send and exactly its internet recipients.
func mailCheckOutbox(ctx context.Context, t testing.TB, network *mailNetwork, ledger *mailLedger, outcomes map[string]*sidecar.SmtpSendResult) {
	mailbox, err := network.SenderMails[0].SmtpMailbox()
	if err != nil {
		t.Errorf("%s's mailbox for its outbox: %v", network.Name, err)
		return
	}
	effectKeys, err := mailbox.ListOutcomes(ctx)
	if err != nil {
		t.Errorf("%s's or/: %v", network.Name, err)
		return
	}
	expectedKeys := map[string]*mailExpected{}
	for _, expected := range ledger.all() {
		if expected.Network == network && expected.Sent != nil && 0 < len(expected.Domains) {
			expectedKeys[string(expected.Sent.EffectKey)] = expected
		}
	}
	held := map[string]bool{}
	for _, effectKey := range effectKeys {
		held[string(effectKey)] = true
		expected := expectedKeys[string(effectKey)]
		if expected == nil {
			t.Errorf("%s's or/ holds a record under an effect key no send was answered", network.Name)
			continue
		}
		recorded, err := mailbox.ReadOutcomes(ctx, effectKey)
		if err != nil {
			t.Errorf("%s's or/ record of %s: %v", network.Name, expected.Token, err)
			continue
		}
		domains := []string{}
		for _, outcome := range recorded {
			domains = append(domains, outcome.Domain)
			if outcome.Outcome != sidecar.SmtpOutcomeSent {
				t.Errorf("%s's or/ holds %s's domain %s %s, not sent", network.Name, expected.Token, outcome.Domain, mailOutcomeName(outcome.Outcome))
			}
		}
		sort.Strings(domains)
		if !slices.Equal(domains, expected.Domains) {
			t.Errorf("%s's or/ holds %s's outcomes for %v, not its domains %v", network.Name, expected.Token, domains, expected.Domains)
		}
		if result := outcomes[expected.Token]; result == nil || !bytes.Equal(result.EffectKey, expected.Sent.EffectKey) {
			t.Errorf("%s's send %s reads its result under another effect key than it was answered", network.Name, expected.Token)
		}
	}
	for effectKey, expected := range expectedKeys {
		if !held[effectKey] {
			t.Errorf("%s's or/ holds no record of its send %s", network.Name, expected.Token)
		}
	}
	outbox, err := mailbox.ListOutbox(ctx)
	if err != nil {
		t.Errorf("%s's o/: %v", network.Name, err)
		return
	}
	for _, effectKey := range outbox {
		expected := expectedKeys[string(effectKey)]
		if expected == nil {
			t.Errorf("%s's o/ holds an entry under an effect key no send was answered", network.Name)
			continue
		}
		file, err := mailbox.ReadOutbox(ctx, effectKey)
		if errors.Is(err, sidecar.ErrSmtpMailboxNotFound) {
			// removed since its listing, sent and past its approval's window
			continue
		}
		if err != nil {
			t.Errorf("%s's o/ entry of %s: %v", network.Name, expected.Token, err)
			continue
		}
		verified, err := sidecar.VerifySmtpOutboxEntry(file, func(sidecarId sidecar.Id) ed25519.PublicKey {
			return worldConsentedMemberKey(network.Reader, sidecarId)
		})
		if err != nil {
			t.Errorf("%s's o/ entry of %s does not verify: %v", network.Name, expected.Token, err)
			continue
		}
		internet := []string{}
		for _, recipient := range expected.Recipients {
			if _, domain, _ := strings.Cut(recipient, "@"); !strings.EqualFold(domain, network.Domain) {
				internet = append(internet, strings.ToLower(recipient))
			}
		}
		recipients := []string{}
		for _, recipient := range verified.Message.Recipients {
			recipients = append(recipients, strings.ToLower(recipient))
		}
		sort.Strings(internet)
		sort.Strings(recipients)
		switch {
		case verified.SignerId != expected.Device.Sidecar().Identity().SidecarId:
			t.Errorf("%s's o/ entry of %s is signed by %s, not by %s, which sent it", network.Name, expected.Token, verified.SignerId, expected.Device.Name)
		case verified.Message.SendId != expected.Sent.SendId:
			t.Errorf("%s's o/ entry of %s names send %s, not %s", network.Name, expected.Token, verified.Message.SendId, expected.Sent.SendId)
		case !slices.Equal(recipients, internet):
			t.Errorf("%s's o/ entry of %s names %v, not its internet recipients %v", network.Name, expected.Token, recipients, internet)
		}
	}
}
