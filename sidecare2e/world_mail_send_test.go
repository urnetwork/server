//go:build sidecar_e2e && sidecar_pending && sidecar_localstack

// The world's mail sender (world_test.go): one device of a network as the member's prover host, composed as a desktop's
// ur daemon composes it (sidecar's mail_component_smtp.go): its SmtpProverHost writing p/, its SmtpSender over
// NewSidecarSmtpEdgeDialer, which Finds ur/smtpedge in urnetwork-edge through sockets and opens the Service-mode stream
// to smtpedge's platform client, its SmtpDkimKeys publishing through POST /smtp/dkim-keys, and its SmtpOutboxRunner
// taking the member's o/ entries, each over the member's own mailbox (MailClient.SmtpMailbox). Its prover is
// sidecartest's FakeSmtpProver, a plain crypto/tls client, since smtpproof's keys come from a setup that has not run
// (smtpproof's SPEC.md section 12), and smtpedge verifies its parts with FakeSmtpProofVerifier (the World's header).
//
// Three parts a desktop's composition takes from the internet stand in, each named in the package header: TL1's DANE
// validates under the world's own DNS anchor (world_mail_dns_test.go) rather than IANA's root keys, which sign no path
// to the world's root; the device's own DoH, which TL2's and TL3's lookups and DKIM's answers take, asks the world's
// dns edge rather than public resolvers, which know no .example name; and the DKIM keys' keychain is a map in the
// test, since a Sidecar's own keychain is not reachable outside its package.

package sidecare2e

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server/sidecart3"
)

// What a world's sending device runs with (QD-702).
type WorldMailSenderSettings struct {
	// The verbosity the composition's logger keeps, beside its info, warnings and errors, in the world's dump.
	LogLevel int32
	// The prover's measure as its host reports it in p/ (§14.5, D-SMTP-20): the DATA octets a connection proves and
	// at what rate, and the shape of a unit, AES-128-GCM's.
	DataProofByteCount    uint64
	DataBytesPerSecond    uint64
	UnitTime              time.Duration
	HeadByteCount         uint64
	HeaderRecordByteCount uint64
	EnvelopeGroupCount    uint64
	// Changes to sidecar.DefaultSmtpSenderSettings the composition takes: a scenario on real time shortens the
	// retries' minutes and hours and the outbox's takeover there. nil takes the defaults.
	Sender func(settings *sidecar.SmtpSenderSettings)
}

func DefaultWorldMailSenderSettings() *WorldMailSenderSettings {
	return &WorldMailSenderSettings{
		LogLevel:              2,
		DataProofByteCount:    1024 * 1024,
		DataBytesPerSecond:    512,
		UnitTime:              5 * time.Second,
		HeadByteCount:         831,
		HeaderRecordByteCount: 1199,
		EnvelopeGroupCount:    13,
	}
}

// A logger that keeps each line among each of names' newest (World.SidecarLogLines), so a stalled step's dump shows
// what the composition said: info, warnings and errors, and the guarded levels up to Level.
type worldTeeLogger struct {
	world *World
	// the device's name, whose lines a stall's dump shows, and the composition's own (WorldMailSender.LogLines), which
	// the device's other lines do not crowd out
	names []string
	level int32
}

var _ sidecar.Logger = (*worldTeeLogger)(nil)

func (self *worldTeeLogger) keep(line string) {
	for _, name := range self.names {
		self.world.keepSidecarLogLine(name, strings.TrimRight(line, "\n"))
	}
}

func (self *worldTeeLogger) Info(args ...any) {
	self.keep(fmt.Sprint(args...))
}

func (self *worldTeeLogger) Infof(format string, args ...any) {
	self.keep(fmt.Sprintf(format, args...))
}

func (self *worldTeeLogger) Warningf(format string, args ...any) {
	self.keep("warning: " + fmt.Sprintf(format, args...))
}

func (self *worldTeeLogger) Errorf(format string, args ...any) {
	self.keep("error: " + fmt.Sprintf(format, args...))
}

func (self *worldTeeLogger) V(level int32) sidecar.Verbose {
	return &worldTeeVerbose{logger: self, enabled: level <= self.level}
}

type worldTeeVerbose struct {
	logger  *worldTeeLogger
	enabled bool
}

func (self *worldTeeVerbose) Enabled() bool {
	return self.enabled
}

func (self *worldTeeVerbose) Info(args ...any) {
	if self.enabled {
		self.logger.Info(args...)
	}
}

func (self *worldTeeVerbose) Infof(format string, args ...any) {
	if self.enabled {
		self.logger.Infof(format, args...)
	}
}

// One sending device's composition (the file's header), running until the world closes or Stop.
type WorldMailSender struct {
	Device *sidecart3.T3Device
	Prover *sidecartest.FakeSmtpProver
	Host   *sidecar.SmtpProverHost
	Dkim   *sidecar.SmtpDkimKeys
	Sender *sidecar.SmtpSender
	Runner *sidecar.SmtpOutboxRunner

	wake chan struct{}
	// the DKIM keys' keychain, which a restart's composition keeps, as the device's own keychain outlives its daemon
	secrets *worldMemorySecrets
	stop    sync.Once
	end     func()

	stateLock sync.Mutex
	// every mail_send event the sender raised
	events []sidecar.SidecarEvent
}

// Starts device as its member's prover host (the file's header) over client, its started MailClient, which the world
// closes, once its socket component stands, since the sender dials ur/smtpedge through it and a send that cannot is
// held for the sender's first retry, 30 minutes on.
func (self *World) StartMailSender(ctx context.Context, device *sidecart3.T3Device, client *sidecar.MailClient, settings *WorldMailSenderSettings) (*WorldMailSender, error) {
	return self.startMailSender(ctx, device, client, settings, &worldMemorySecrets{values: map[string][]byte{}})
}

// Stops sender's composition, as a daemon's restart stops it: its runner and host end, and its sender cancels every
// send in flight and returns once they have ended. Its stores stay: the device's local store, the member's mailbox and
// the DKIM keys' keychain. Safe to call again.
func (self *WorldMailSender) Stop() {
	self.stop.Do(self.end)
}

// Stops sender (Stop) and composes its device's prover host again over client on the same stores and DKIM keychain,
// as a daemon's restart does: the new composition reads the member's o/ and or/ where the old one left them.
func (self *World) RestartMailSender(ctx context.Context, sender *WorldMailSender, client *sidecar.MailClient, settings *WorldMailSenderSettings) (*WorldMailSender, error) {
	sender.Stop()
	return self.startMailSender(ctx, sender.Device, client, settings, sender.secrets)
}

// StartMailSender with the DKIM keys in secrets.
func (self *World) startMailSender(ctx context.Context, device *sidecart3.T3Device, client *sidecar.MailClient, settings *WorldMailSenderSettings, secrets *worldMemorySecrets) (*WorldMailSender, error) {
	if settings == nil {
		settings = DefaultWorldMailSenderSettings()
	}
	if err := self.AwaitSockets(ctx, device, nil); err != nil {
		return nil, err
	}
	running := device.Sidecar()
	identity := running.Identity()
	if identity == nil {
		return nil, fmt.Errorf("%s has no identity", device.Name)
	}
	mailbox, err := client.SmtpMailbox()
	if err != nil {
		return nil, fmt.Errorf("%s's mailbox: %w", device.Name, err)
	}
	store, err := running.LocalStore().Directory("smtp", "smtp")
	if err != nil {
		return nil, err
	}
	zone, err := worldDeviceZone(ctx, device)
	if err != nil {
		return nil, err
	}
	senderSettings := sidecar.DefaultSmtpSenderSettings()
	if settings.Sender != nil {
		settings.Sender(senderSettings)
	}
	clock := sidecar.RealClock{}
	log := &worldTeeLogger{world: self, names: []string{device.Name, worldMailSenderLogName(device)}, level: settings.LogLevel}
	anchors, err := self.dnssec.Anchors()
	if err != nil {
		return nil, err
	}
	pins := sidecar.NewSmtpPinStore(store, senderSettings, clock, log)
	policy := sidecar.NewSmtpTlsPolicy(
		sidecar.NewSmtpDane(anchors, clock, nil),
		// no MTA-STS policy is served for a .example name: the fetch finds none, as RFC 8461 §3.3's sender does
		sidecar.NewSmtpMtaSts(self.dnssec, worldNoMtaStsTransport{}, store, senderSettings, clock),
		pins, sidecar.NewSmtpTlsAllowanceStore(store, senderSettings, clock), running, self.dnssec, nil, senderSettings, clock)
	proverSettings := sidecartest.SmtpSendWorldProver(settings.DataProofByteCount)
	proverSettings.Measure = &sidecar.SmtpProverMeasure{
		DataProofBytes:        settings.DataProofByteCount,
		DataBytesPerSecond:    settings.DataBytesPerSecond,
		MeasureTime:           time.Now(),
		UnitTime:              settings.UnitTime,
		HeadByteCount:         settings.HeadByteCount,
		HeaderRecordByteCount: settings.HeaderRecordByteCount,
		EnvelopeGroupCount:    settings.EnvelopeGroupCount,
	}
	sender := &WorldMailSender{Device: device, Prover: sidecartest.NewFakeSmtpProver(proverSettings), wake: make(chan struct{}, 1), secrets: secrets}
	sender.Host, err = sidecar.NewSmtpProverHost(&sidecar.SmtpProverHostArgs{
		Prover: sender.Prover, Mailbox: mailbox, SidecarId: identity.SidecarId, MailEnabled: func() bool { return true },
		Settings: senderSettings, Clock: clock, Log: log,
	})
	if err != nil {
		return nil, err
	}
	// its p/, which the runner's pick reads
	if err := sender.Host.Idle(ctx); err != nil {
		return nil, fmt.Errorf("%s's p/: %w", device.Name, err)
	}
	sender.Dkim = sidecar.NewSmtpDkimKeys(secrets, &worldDkimPublisher{world: self, device: device}, senderSettings, clock, log)
	sender.Dkim.SetDomainAnswers(&sidecar.SmtpDkimDomainAnswers{Doh: self.dnssec, Root: strings.TrimSuffix(zone.Root, ".")})
	memberKey := func(sidecarId sidecar.Id) ed25519.PublicKey {
		return worldConsentedMemberKey(device, sidecarId)
	}
	sender.Sender, err = sidecar.NewSmtpSender(&sidecar.SmtpSenderArgs{
		Dialer:       sidecar.NewSidecarSmtpEdgeDialer(running, senderSettings),
		Prover:       sender.Prover,
		Policy:       policy,
		Pins:         pins,
		Dkim:         sender.Dkim,
		Mailbox:      mailbox,
		MemberDevice: func(sidecarId sidecar.Id) bool { return memberKey(sidecarId) != nil },
		Measure:      sender.Host.Measure,
		ReadyKeySets: sender.Host.ReadyKeySets,
		SendHold:     sender.Host.HoldForSend,
		Envelope:     client.BounceSender,
		Events: func(event sidecar.SidecarEvent) {
			sender.stateLock.Lock()
			defer sender.stateLock.Unlock()
			sender.events = append(sender.events, event)
		},
		Settings: senderSettings,
		Clock:    clock,
		Log:      log,
	})
	if err != nil {
		return nil, err
	}
	sender.Runner, err = sidecar.NewSmtpOutboxRunner(&sidecar.SmtpOutboxRunnerArgs{
		Sender: sender.Sender, Mailbox: mailbox, SidecarId: identity.SidecarId, MemberDeviceKey: memberKey,
		Consented: func() bool { return memberKey(identity.SidecarId) != nil }, OwnCap: sender.Host.Cap,
		Settings: senderSettings, Clock: clock, Log: log,
	})
	if err != nil {
		sender.Sender.Close()
		return nil, err
	}
	runCtx, cancel := context.WithCancel(self.t3.Context())
	var workers sync.WaitGroup
	workers.Go(func() { sender.Host.Run(runCtx) })
	workers.Go(func() { sender.Runner.Run(runCtx, sender.wake) })
	sender.end = func() {
		cancel()
		sender.Sender.Close()
		workers.Wait()
	}
	self.t3.OnClose(sender.Stop)
	return sender, nil
}

// Wakes the runner, so it reads the member's o/ now rather than at its next pass.
func (self *WorldMailSender) Wake() {
	select {
	case self.wake <- struct{}{}:
	default:
	}
}

// The name a device's prover host's compositions keep their log lines under, beside the device's.
func worldMailSenderLogName(device *sidecart3.T3Device) string {
	return device.Name + " smtp"
}

// The newest lines the device's prover host's compositions logged, this one's and any before it on the device, oldest
// first.
func (self *WorldMailSender) LogLines(world *World) []string {
	return world.SidecarLogLines(worldMailSenderLogName(self.Device))
}

// Every mail_send event the sender raised so far.
func (self *WorldMailSender) Events() []sidecar.SidecarEvent {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.events)
}

// The identity key of sidecarId where device's verified state holds it a consented device of device's own member
// (§8.5; owner decision 126), as the daemon's composition reads it, nil otherwise.
func worldConsentedMemberKey(device *sidecart3.T3Device, sidecarId sidecar.Id) ed25519.PublicKey {
	follower := device.Follower()
	identity := device.Sidecar().Identity()
	if follower == nil || identity == nil {
		return nil
	}
	state := follower.VerifiedState()
	if state == nil {
		return nil
	}
	acl, err := state.Acl()
	if err != nil {
		return nil
	}
	own := state.Sidecar(identity.SidecarId)
	other := state.Sidecar(sidecarId)
	if own == nil || other == nil || own.MemberId == (sidecar.Id{}) {
		return nil
	}
	if !acl.ConsentedFor(identity.SidecarId, own.MemberId) || !acl.ConsentedFor(sidecarId, own.MemberId) {
		return nil
	}
	key := other.Row.GetIdentityPublicKey()
	if len(key) != ed25519.PublicKeySize {
		return nil
	}
	return ed25519.PublicKey(slices.Clone(key))
}

// device's network's zone, as its DnsClient reads it.
func worldDeviceZone(ctx context.Context, device *sidecart3.T3Device) (*sidecar.DnsZone, error) {
	dnsClient, err := device.Sidecar().Dns()
	if err != nil {
		return nil, fmt.Errorf("%s's dns client: %w", device.Name, err)
	}
	zone, err := dnsClient.Zone(ctx)
	if err != nil || zone.Apex() == "" {
		return nil, fmt.Errorf("%s's zone: %+v, %v", device.Name, zone, err)
	}
	return zone, nil
}

// POST /smtp/dkim-keys as the device's SidecarApi sends it, through T3Api on the device's token.
type worldDkimPublisher struct {
	world  *World
	device *sidecart3.T3Device
}

var _ sidecar.SmtpDkimPublisher = (*worldDkimPublisher)(nil)

func (self *worldDkimPublisher) SmtpDkimKeys(ctx context.Context, args *sidecar.SmtpDkimKeysArgs) (*sidecar.SmtpDkimKeysResult, error) {
	status, answer, err := self.world.t3.Api().Call(ctx, http.MethodPost, "/smtp/dkim-keys", self.device.Bearer(), args)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("POST /smtp/dkim-keys answered %d: %s", status, answer)
	}
	result := &sidecar.SmtpDkimKeysResult{}
	if err := decodeJson(strings.NewReader(string(answer)), result); err != nil {
		return nil, err
	}
	return result, nil
}

// A keychain namespace in memory, for the DKIM keys of a device whose own keychain the test cannot reach.
type worldMemorySecrets struct {
	stateLock sync.Mutex
	values    map[string][]byte
}

var _ sidecar.SidecarSecrets = (*worldMemorySecrets)(nil)

func (self *worldMemorySecrets) Get(name string) ([]byte, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	value, ok := self.values[name]
	if !ok {
		return nil, sidecar.ErrSidecarStoreAbsent
	}
	return slices.Clone(value), nil
}

func (self *worldMemorySecrets) Put(name string, value []byte) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.values[name] = slices.Clone(value)
	return nil
}

func (self *worldMemorySecrets) Delete(name string) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	delete(self.values, name)
	return nil
}

func (self *worldMemorySecrets) List(prefix string) ([]string, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	names := []string{}
	for name := range self.values {
		if strings.HasPrefix(name, prefix) {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names, nil
}

// The sender's MTA-STS fetch, which no world name serves a policy for: every fetch answered 404, as a domain that
// publishes none.
type worldNoMtaStsTransport struct{}

func (worldNoMtaStsTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.Body != nil {
		request.Body.Close()
	}
	return &http.Response{Request: request, StatusCode: http.StatusNotFound, Header: http.Header{}, Body: http.NoBody}, nil
}

// Waits until sendId's outcome for domain, as the member's or/ holds it and SendResult reads it, is one done takes,
// and answers it; the last result otherwise. The sender writes or/ as each domain's send ends, with no hook a test
// reads, so it is read every RetryInterval within Timeout, the runner woken each time.
func (self *World) AwaitMailSendResult(ctx context.Context, sender *WorldMailSender, client *sidecar.MailClient, sendId sidecar.Id, done func(result *sidecar.SmtpSendResult) bool, settings *WorldMailSettings) (*sidecar.SmtpSendResult, error) {
	if settings == nil {
		settings = DefaultWorldMailSettings()
	}
	ctx, cancel := context.WithTimeout(ctx, settings.Timeout)
	defer cancel()
	var last *sidecar.SmtpSendResult
	var lastErr error
	for {
		sender.Wake()
		result, err := client.SendResult(ctx, sendId)
		if err == nil && done(result) {
			return result, nil
		}
		if err != nil && !errors.Is(err, context.DeadlineExceeded) {
			lastErr = err
		}
		if result != nil {
			last = result
		}
		select {
		case <-ctx.Done():
			return last, fmt.Errorf("%s's send %s never ended as wanted: %+v, %v (%w)", sender.Device.Name, sendId, last, lastErr, ctx.Err())
		case <-time.After(settings.RetryInterval):
		}
	}
}
