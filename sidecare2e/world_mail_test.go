//go:build sidecar_e2e && sidecar_pending && sidecar_localstack

// The world's mail helpers (world_test.go): a device's mail as ur daemon composes it once its follower stands, the evs
// component it binds on and its mail component (StartMail); mail turned on for a network by its owner's MailClient,
// which ensures mailcfg, the member's mailbox and the owner's RepoAdmin of it, then POST /mail/enable, which writes
// §12.7's mail records and projects the network's {mxr_} routes; an alias; a receiver that serves smtp/25/mx through
// sockets once its TLSA, posted through /dns/key-record, is live; the dns edge's signed MX, A and TLSA for the
// network's mail host; an internet sender's delivery through the world's lb to mxedge's router on 25, its STARTTLS
// and a DANE-EE check of the receiver's key against the edge's TLSA; and the message read back from the member's
// mailbox once its reader has ingested it.

package sidecare2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"net/smtp"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"

	"github.com/urnetwork/server/sidecart3"
)

// What the mail helpers run with (QD-702).
type WorldMailSettings struct {
	// The receiver's TLSA timing: a key serves TlsaLiveTtlCount TlsaTtls after its POST /dns/key-record took it, 2 ×
	// 300 s in production (RFC 7671 §8.1). A world's sidecars run on real time, so a step shortens the wait.
	TlsaTtl          time.Duration
	TlsaLiveTtlCount int
	// How soon a receiver asks again after a refused listen or key record (MailReceiverSettings' retries), and how often
	// the mail component passes while it is bound and while it is not: production's are minutes, and a world's
	// sidecars run on real time.
	ReceiverRetryInterval time.Duration
	CheckInterval         time.Duration
	UnboundCheckInterval  time.Duration
	// How often a wait asks again (the component's binding, the receiver's serving, the dns edge's answers, a delivery
	// refused for now, the message in the mailbox), and the bound on each wait.
	RetryInterval time.Duration
	Timeout       time.Duration
	// The internet sender: the address the lb's PROXY header names, a documentation address, its EHLO name, and one
	// delivery's bound: its dial, the dialog, STARTTLS's handshake and the message.
	SenderAddress   netip.AddrPort
	SenderHelo      string
	DeliveryTimeout time.Duration
	// The most entries one read of the mailbox lists.
	ListEntryCount int
	// How long a reader's ingest may take to run on a deposit's hint alone (§11.10's hints; §19.8's head-to-hint
	// deadline is 2 s), past which a wait wakes the ingest itself, as an app's wake does (MailClient.Resync), and says
	// the hint did not.
	IngestHintTimeout time.Duration
}

func DefaultWorldMailSettings() *WorldMailSettings {
	return &WorldMailSettings{
		TlsaTtl:               time.Second,
		TlsaLiveTtlCount:      1,
		ReceiverRetryInterval: time.Second,
		CheckInterval:         2 * time.Second,
		UnboundCheckInterval:  time.Second,
		RetryInterval:         250 * time.Millisecond,
		Timeout:               2 * time.Minute,
		SenderAddress:         netip.MustParseAddrPort("198.51.100.25:40025"),
		SenderHelo:            "mta.sender.example",
		DeliveryTimeout:       30 * time.Second,
		ListEntryCount:        50,
		IngestHintTimeout:     20 * time.Second,
	}
}

// The port mail reaches an MX on.
const worldMailPort = 25

// device's mail as ur daemon composes it once its follower stands: its socket component, which a receiver's listen and
// a sender's dial of ur/smtpedge run on (StartSockets), the evs component on the device's EvsClient, which mail binds
// on (cli's daemon_clients.go), then its MailClient on the world's mail settings and the mail component
// (Sidecar.StartMail). receive false keeps the device from running a receiver even where policy grants it the listen
// on smtp/25/mx. The sidecar removes the components at its close.
func (self *World) StartMail(ctx context.Context, device *sidecart3.T3Device, receive bool, settings *WorldMailSettings) (*sidecar.MailClient, error) {
	if settings == nil {
		settings = DefaultWorldMailSettings()
	}
	if err := self.StartSockets(ctx, device); err != nil {
		return nil, err
	}
	running := device.Sidecar()
	if running.EvsClient() == nil {
		client, err := device.EvsClient()
		if err != nil {
			return nil, fmt.Errorf("%s's evs client: %w", device.Name, err)
		}
		if _, err := running.AddComponent(sidecar.NewEvsComponent(client)); err != nil {
			return nil, fmt.Errorf("%s's evs component: %w", device.Name, err)
		}
	}
	mailSettings := sidecar.DefaultMailSettings()
	mailSettings.CheckInterval = settings.CheckInterval
	mailSettings.UnboundCheckInterval = settings.UnboundCheckInterval
	mailSettings.Receive.Enabled = receive
	receiver := mailSettings.Receive.Receiver
	receiver.TlsaTtl = settings.TlsaTtl
	receiver.TlsaLiveTtlCount = settings.TlsaLiveTtlCount
	receiver.ListenRetryInterval = settings.ReceiverRetryInterval
	receiver.PublishRetryInterval = settings.ReceiverRetryInterval
	receiver.ApprovalRetryInterval = settings.ReceiverRetryInterval
	if _, err := running.MailWith(mailSettings); err != nil {
		return nil, fmt.Errorf("%s's mail: %w", device.Name, err)
	}
	client, err := running.StartMail()
	if err != nil {
		return nil, fmt.Errorf("%s's mail component: %w", device.Name, err)
	}
	return client, nil
}

// Turns mail on for device's network through its MailClient, as `ur mail enable` does: mailcfg, the member's mailbox
// and the device's RepoAdmin of it, then POST /mail/enable. Tried again every RetryInterval within Timeout while the
// component has not bound what it works on yet (ErrMailUnbound), with no hook a test reads.
func (self *World) EnableMail(ctx context.Context, device *sidecart3.T3Device, client *sidecar.MailClient, settings *WorldMailSettings) (*sidecar.MailEnabled, error) {
	if settings == nil {
		settings = DefaultWorldMailSettings()
	}
	ctx, cancel := context.WithTimeout(ctx, settings.Timeout)
	defer cancel()
	for {
		enabled, err := client.Enable(ctx)
		if err == nil || !errors.Is(err, sidecar.ErrMailUnbound) {
			if err != nil {
				return nil, fmt.Errorf("%s's mail enable: %w", device.Name, err)
			}
			return enabled, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("%s's mail stayed unbound: %w (%w)", device.Name, err, ctx.Err())
		case <-time.After(settings.RetryInterval):
		}
	}
}

// Waits until device's receiver serves smtp/25/mx, its registration held and a key live, and answers its status; the
// last status past Timeout. The component starts the receiver at a pass and the receiver serves once its key is live,
// with no hook a test reads, so the status is read every RetryInterval.
func (self *World) AwaitMailReceiver(ctx context.Context, device *sidecart3.T3Device, client *sidecar.MailClient, settings *WorldMailSettings) (*sidecar.MailClientStatus, error) {
	if settings == nil {
		settings = DefaultWorldMailSettings()
	}
	ctx, cancel := context.WithTimeout(ctx, settings.Timeout)
	defer cancel()
	for {
		status, err := client.Status(ctx)
		if err == nil && status.Receiver.Running && status.Receiver.Listening && status.Receiver.Serving {
			return status, nil
		}
		select {
		case <-ctx.Done():
			return status, fmt.Errorf("%s's receiver never served: status %+v, %v (%w)", device.Name, status, err, ctx.Err())
		case <-time.After(settings.RetryInterval):
		}
	}
}

// What the dns edge answers for a network's mail domain, each answer signed: its MX's host, that host's addresses and
// its TLSA records' SPKI digests.
type WorldMailDns struct {
	Domain    string
	MxHost    string
	Addresses []netip.Addr
	// each 3 1 1 record's data: the SHA-256 of a receiver key's SubjectPublicKeyInfo
	TlsaSpkiSha256s [][]byte
}

// Waits until the dns edge answers domain's MX, signed, with one host, that host's A records, signed, holding the
// world's lb interface, and its TLSA at _25._tcp, signed, holding a 3 1 1 record of each of spkiSha256s: what a DANE
// sender resolves before it dials (RFC 7672).
func (self *World) AwaitMailDns(ctx context.Context, domain string, spkiSha256s [][]byte, settings *WorldDnsSettings) (*WorldMailDns, error) {
	found := &WorldMailDns{Domain: domain}
	answer, err := self.AwaitDnsAnswer(ctx, domain, dns.TypeMX, func(answer *dns.Msg) bool {
		hosts := []string{}
		for _, rr := range answer.Answer {
			if mx, ok := rr.(*dns.MX); ok && dns.CanonicalName(mx.Hdr.Name) == dns.CanonicalName(domain) {
				hosts = append(hosts, strings.TrimSuffix(dns.CanonicalName(mx.Mx), "."))
			}
		}
		if len(hosts) != 1 || !dnsAnswerSigned(answer, dns.TypeMX) {
			return false
		}
		found.MxHost = hosts[0]
		return true
	}, settings)
	if err != nil {
		return nil, fmt.Errorf("%s's MX: %w: %v", domain, err, answer)
	}
	answer, err = self.AwaitDnsAnswer(ctx, found.MxHost, dns.TypeA, func(answer *dns.Msg) bool {
		addresses := []netip.Addr{}
		for _, rr := range answer.Answer {
			if a, ok := rr.(*dns.A); ok {
				if address, ok := netip.AddrFromSlice(a.A.To4()); ok {
					addresses = append(addresses, address)
				}
			}
		}
		if !slices.Contains(addresses, self.settings.LbAddress) || !dnsAnswerSigned(answer, dns.TypeA) {
			return false
		}
		found.Addresses = addresses
		return true
	}, settings)
	if err != nil {
		return nil, fmt.Errorf("%s's addresses: %w: %v", found.MxHost, err, answer)
	}
	tlsaName := "_25._tcp." + found.MxHost
	answer, err = self.AwaitDnsAnswer(ctx, tlsaName, dns.TypeTLSA, func(answer *dns.Msg) bool {
		digests := [][]byte{}
		for _, rr := range answer.Answer {
			tlsa, ok := rr.(*dns.TLSA)
			if !ok || tlsa.Usage != 3 || tlsa.Selector != 1 || tlsa.MatchingType != 1 {
				continue
			}
			digest, err := hex.DecodeString(tlsa.Certificate)
			if err == nil {
				digests = append(digests, digest)
			}
		}
		for _, want := range spkiSha256s {
			if !slices.ContainsFunc(digests, func(digest []byte) bool { return bytes.Equal(digest, want) }) {
				return false
			}
		}
		if len(digests) == 0 || !dnsAnswerSigned(answer, dns.TypeTLSA) {
			return false
		}
		found.TlsaSpkiSha256s = digests
		return true
	}, settings)
	if err != nil {
		return nil, fmt.Errorf("%s's TLSA: %w: %v", tlsaName, err, answer)
	}
	return found, nil
}

// Whether answer carries an RRSIG over its rrtype records.
func dnsAnswerSigned(answer *dns.Msg, rrtype uint16) bool {
	for _, rr := range answer.Answer {
		if signature, ok := rr.(*dns.RRSIG); ok && signature.TypeCovered == rrtype {
			return true
		}
	}
	return false
}

// One internet delivery: the mail domain's MX as the dns edge answered it, the envelope and the message.
type WorldMailDelivery struct {
	Dns     *WorldMailDns
	From    string
	To      []string
	Message []byte
}

// An internet sender's delivery of delivery as RFC 7672's DANE sender makes it, through the world's lb: TCP to the
// MX's address on 25, which the lb hands mxedge's router behind its PROXY header naming the sender's address, the
// router's banner and EHLO, STARTTLS with the MX host as SNI, a handshake whose leaf must be on a key the TLSA records
// name (DANE-EE, 3 1 1), then EHLO, MAIL, RCPT, DATA and QUIT inside TLS, answered by the receiver the router spliced
// the connection to. nil once the DATA's end was answered 250, the deposit durable.
func (self *World) DeliverMail(ctx context.Context, delivery *WorldMailDelivery, settings *WorldMailSettings) error {
	if settings == nil {
		settings = DefaultWorldMailSettings()
	}
	ctx, cancel := context.WithTimeout(ctx, settings.DeliveryTimeout)
	defer cancel()
	index := slices.Index(delivery.Dns.Addresses, self.settings.LbAddress)
	if index < 0 {
		return fmt.Errorf("%s answers none of the lb's addresses", delivery.Dns.MxHost)
	}
	conn, err := self.DialLb(ctx, settings.SenderAddress, netip.AddrPortFrom(delivery.Dns.Addresses[index], worldMailPort))
	if err != nil {
		return err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		conn.SetDeadline(deadline)
	}
	client, err := smtp.NewClient(conn, delivery.Dns.MxHost)
	if err != nil {
		return fmt.Errorf("the banner: %w", err)
	}
	defer client.Close()
	if err := client.Hello(settings.SenderHelo); err != nil {
		return fmt.Errorf("EHLO: %w", err)
	}
	if ok, _ := client.Extension("STARTTLS"); !ok {
		return errors.New("the MX offers no STARTTLS")
	}
	digests := delivery.Dns.TlsaSpkiSha256s
	config := &tls.Config{
		ServerName: delivery.Dns.MxHost,
		MinVersion: tls.VersionTLS12,
		// DANE-EE authenticates the leaf's key alone (RFC 7672 §3.1.1), checked below, never a chain
		InsecureSkipVerify: true,
		VerifyConnection: func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) == 0 {
				return errors.New("no certificate")
			}
			spki, err := x509.MarshalPKIXPublicKey(state.PeerCertificates[0].PublicKey)
			if err != nil {
				return err
			}
			digest := sha256.Sum256(spki)
			if !slices.ContainsFunc(digests, func(want []byte) bool { return bytes.Equal(want, digest[:]) }) {
				return fmt.Errorf("the leaf's key %x matches no TLSA record", digest)
			}
			return nil
		},
	}
	if err := client.StartTLS(config); err != nil {
		return fmt.Errorf("STARTTLS: %w", err)
	}
	if err := client.Mail(delivery.From); err != nil {
		return fmt.Errorf("MAIL: %w", err)
	}
	for _, to := range delivery.To {
		if err := client.Rcpt(to); err != nil {
			return fmt.Errorf("RCPT: %w", err)
		}
	}
	writer, err := client.Data()
	if err != nil {
		return fmt.Errorf("DATA: %w", err)
	}
	if _, err := writer.Write(delivery.Message); err != nil {
		return fmt.Errorf("the message: %w", err)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("the DATA's end: %w", err)
	}
	return client.Quit()
}

// DeliverMail until the DATA's end is answered 250, tried again every RetryInterval within Timeout while it is
// refused: the router's {mxr_} route, the receiver's registration and its Find reach mxedge with no hook a test reads.
// The last refusal otherwise.
func (self *World) AwaitMailDelivery(ctx context.Context, delivery *WorldMailDelivery, settings *WorldMailSettings) error {
	if settings == nil {
		settings = DefaultWorldMailSettings()
	}
	ctx, cancel := context.WithTimeout(ctx, settings.Timeout)
	defer cancel()
	tries := 0
	for {
		tries += 1
		err := self.DeliverMail(ctx, delivery, settings)
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the delivery to %s, %d tries: %w (%w)", delivery.Dns.MxHost, tries, err, ctx.Err())
		case <-time.After(settings.RetryInterval):
		}
	}
}

// device's member's mailbox in its verified state: the mailbox repo owned by the member device is a device of, false
// before one stands.
func worldMemberMailboxId(device *sidecart3.T3Device) (sidecar.Id, bool) {
	follower := device.Follower()
	identity := device.Sidecar().Identity()
	if follower == nil || identity == nil {
		return sidecar.Id{}, false
	}
	state := follower.VerifiedState()
	if state == nil {
		return sidecar.Id{}, false
	}
	own := state.Sidecar(identity.SidecarId)
	acl, err := state.Acl()
	if own == nil || err != nil {
		return sidecar.Id{}, false
	}
	for _, repoId := range acl.RepoIds() {
		repo := acl.Repo(repoId)
		if repo == nil || repo.GetKind() != protocol.RepoKind_REPO_KIND_MAILBOX {
			continue
		}
		if ownerId, err := sidecar.IdFromBytes(repo.GetOwnerMemberId()); err == nil && ownerId == own.MemberId {
			return repoId, true
		}
	}
	return sidecar.Id{}, false
}

// What device's verified state gives it on repoId for an evs token: the rights the exchange grants its scopes from
// (AclExchangeScopes).
func worldEvsAccess(device *sidecart3.T3Device, repoId sidecar.Id) sidecar.AclEvsAccess {
	follower := device.Follower()
	identity := device.Sidecar().Identity()
	if follower == nil || identity == nil || follower.VerifiedState() == nil {
		return sidecar.AclEvsAccess{}
	}
	acl, err := follower.VerifiedState().Acl()
	if err != nil {
		return sidecar.AclEvsAccess{}
	}
	return acl.EvsAccess(sidecar.AclSidecarSubject(identity.SidecarId), repoId, sidecar.AclTokenAudienceEvs, time.Now())
}

// The changes device's EvsClient raises for one repo's subscribed refs: what a hint's read brings (evs_subscribe.go's
// changes row), which tells a hint that never woke a reader's ingest from one whose read raised a change the ingest did
// not take. Counted from its making until Freeze, which a wait calls before it wakes the ingest itself.
type worldRepoChanges struct {
	count  atomic.Int64
	frozen atomic.Bool
	remove func()
}

// Counts repoId's changes at device's EvsClient until Freeze; Close stops listening.
func newWorldRepoChanges(device *sidecart3.T3Device, repoId sidecar.Id) *worldRepoChanges {
	self := &worldRepoChanges{remove: func() {}}
	client := device.Sidecar().EvsClient()
	if client == nil {
		self.count.Store(-1)
		self.frozen.Store(true)
		return self
	}
	self.remove = client.AddRepoChangeListener(&repoId, func(change *sidecar.EvsRepoChange) {
		if !self.frozen.Load() {
			self.count.Add(1)
		}
	})
	return self
}

func (self *worldRepoChanges) Freeze() {
	self.frozen.Store(true)
}

// The changes counted, -1 where the device had no EvsClient.
func (self *worldRepoChanges) Count() int64 {
	return self.count.Load()
}

func (self *worldRepoChanges) Close() {
	self.remove()
}

// A message in a mailbox as a reader reads it back, and whether the deposit's hint alone brought its ingest.
type WorldMailMessage struct {
	Entry  *sidecar.MailViewEntry
	Read   *sidecar.MailReadResult
	ByHint bool
}

// The header's value in read, "" where it has none.
func (self *WorldMailMessage) Header(name string) string {
	for _, header := range self.Read.Headers {
		if strings.EqualFold(header.Name, name) {
			return header.Value
		}
	}
	return ""
}

// Waits until client's mailbox lists a message whose subject is subject, ingested by the member's reader, and reads
// it. The reader's ingest runs at the deposit's hint, or its hourly resync, with no hook a test reads, so the mailbox is
// listed every RetryInterval within Timeout; past IngestHintTimeout each list wakes the ingest first, as an app's wake
// does (MailClient.Resync), changes, where given, frozen before the first wake. The message's ByHint says whether the
// hint alone brought it.
func (self *World) AwaitMailMessage(ctx context.Context, device *sidecart3.T3Device, client *sidecar.MailClient, subject string, changes *worldRepoChanges, settings *WorldMailSettings) (*WorldMailMessage, error) {
	if settings == nil {
		settings = DefaultWorldMailSettings()
	}
	ctx, cancel := context.WithTimeout(ctx, settings.Timeout)
	defer cancel()
	var lastErr error
	listed := 0
	hintEnd := time.Now().Add(settings.IngestHintTimeout)
	woken := false
	for {
		if !time.Now().Before(hintEnd) {
			if !woken && changes != nil {
				changes.Freeze()
			}
			client.Resync()
			woken = true
		}
		page, err := client.List(ctx, &sidecar.MailListArgs{Limit: settings.ListEntryCount})
		if err == nil {
			listed = len(page.Entries)
			for _, entry := range page.Entries {
				if entry.Entry.GetSubject() != subject {
					continue
				}
				id, err := sidecar.IdFromBytes(entry.Entry.GetId())
				if err != nil {
					return nil, err
				}
				read, err := client.Read(ctx, &sidecar.MailReadArgs{Month: sidecar.MailYearMonthOf(entry.Entry.GetTime()), Id: id})
				if err != nil {
					return nil, fmt.Errorf("%s's read of %q: %w", device.Name, subject, err)
				}
				return &WorldMailMessage{Entry: entry, Read: read, ByHint: !woken}, nil
			}
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("%s's mailbox never listed %q: %d listed, %v (%w)", device.Name, subject, listed, lastErr, ctx.Err())
		case <-time.After(settings.RetryInterval):
		}
	}
}

// §16.6's stock gate on one device with the two stand-ins the world needs in front of it (the World's header), each
// counted so a step can say what stood in:
//
//   - a listen the stock gate refuses as an action with no effect, the product finding that socketListenAction builds
//     no Effect, passes where the device's verified policy grants the device the listen on the action's socket name
//     (AclState.MayListen): the stock gate, given an effect, would hold the listen for a person's approval (I-8), since
//     its action names no subject or allow a standing grant could cover, and the stand-in is that person approving it;
//   - an action of a kind in approved that the stock gate holds for a person's decision (*ApprovalPendingError) runs
//     its effect as that decision would let it: the world composes no approver device, whose person signs a decision
//     on another device (`ur approve`), so the stand-in is that person approving the one use. Such a kind has no
//     standing-grant form: an alias's creation names no grant fields, so no rule covers it.
//
// Every other answer is the stock gate's.
type worldDeviceGate struct {
	device   *sidecart3.T3Device
	next     sidecar.SidecarGate
	approved []sidecar.SidecarActionKind
	// listens the stock gate refused for no effect, passed here, and actions it held for a person, run here
	listenPassedCount atomic.Int64
	approvedCount     atomic.Int64
}

// Installs §16.6's stock gate behind device's Authorize (World.InstallApprovalGate) with the stand-ins of
// worldDeviceGate in front of it, approving the kinds in approved as a person would.
func (self *World) InstallDeviceGate(ctx context.Context, device *sidecart3.T3Device, approved ...sidecar.SidecarActionKind) (*worldDeviceGate, error) {
	stock, err := self.InstallApprovalGate(ctx, device)
	if err != nil {
		return nil, err
	}
	gate := &worldDeviceGate{device: device, next: stock, approved: approved}
	remove := device.Sidecar().InstallGate(gate)
	self.t3.OnClose(remove)
	return gate, nil
}

func (self *worldDeviceGate) Authorize(ctx context.Context, action *sidecar.SidecarAction, decision *bridge.Signed) error {
	err := self.next.Authorize(ctx, action, decision)
	if err == nil || action == nil {
		return err
	}
	var pending *sidecar.ApprovalPendingError
	switch {
	case action.Kind == sidecar.SidecarActionListen && action.Effect == nil && errors.Is(err, sidecar.ErrSidecarActionInvalid):
		follower := self.device.Follower()
		identity := self.device.Sidecar().Identity()
		if follower == nil || follower.AclEngine() == nil || identity == nil || action.Detail == nil {
			return err
		}
		state := follower.AclEngine().State()
		socket := sidecar.AclSocketName{PublicName: action.Detail.SocketName}
		if state == nil || !state.MayListen(sidecar.AclSidecarSubject(identity.SidecarId), socket, time.Now()).Allowed {
			return fmt.Errorf("%w; and the device's policy grants it no listen on %s", err, action.Detail.SocketName)
		}
		self.listenPassedCount.Add(1)
		return nil
	case slices.Contains(self.approved, action.Kind) && action.Effect != nil && errors.As(err, &pending):
		// the one use a person's decision lets through, its effect run as the stock gate's spend runs it
		effectRef, runErr := action.Effect.Run(ctx, nil)
		switch {
		case runErr == nil:
			action.Outcome = &sidecar.SidecarOutcome{State: sidecar.ApprovalOutcomeDone, EffectRef: bytes.Clone(effectRef)}
		case errors.Is(runErr, sidecar.ErrSidecarEffectNotDone):
			action.Outcome = &sidecar.SidecarOutcome{State: sidecar.ApprovalOutcomeNotDone, Err: runErr}
		default:
			action.Outcome = &sidecar.SidecarOutcome{State: sidecar.ApprovalOutcomeUncertain, Err: runErr}
		}
		self.approvedCount.Add(1)
		return nil
	}
	return err
}

// How many listens the stock gate refused for no effect and this passed.
func (self *worldDeviceGate) ListenPassedCount() int64 {
	return self.listenPassedCount.Load()
}

// How many actions the stock gate held for a person and this ran as approved.
func (self *worldDeviceGate) ApprovedCount() int64 {
	return self.approvedCount.Load()
}
