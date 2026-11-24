//go:build sidecar_e2e && sidecar_pending && sidecar_localstack

// The mail case's report sub-case (e2e-report-key-route; §14.8's sealed report copy, §15.4's platform mail, §15.8's
// network mail key, plan-SMTP-report-seal-key, QD-1229): DMARC aggregates a reporter sends to each network's report
// token reach mxedge's platform listener through the world's lb, and its intake (world_report_intake_test.go) ties
// each to its network by the zone's token, counts it, and seals its raw bytes to the network mail key, which it reads
// from api's report-key route on the platform listener's own bearer, api answering from gkm's drop-key route under the
// deposit token it mints itself; then each network's owner reads its reports back through GET /smtp/reports and opens
// each copy with its mailbox's drop key. On the mail case's world (mail_concurrency_test.go), each network aged,
// verified and holding the mail grants (addMailNetworks):
//
//	setup     each network's owner turns its mail on, which creates its member's mailbox, a personal network's mail
//	          key repo (AclState.NetworkMailKeyRepo); the network's log is synced to every gate, api's and gkm's among
//	          them, and the owner's key client answers the mailbox's drop key, refreshing its generations from gkm as
//	          the reader it is, so api's first read finds the repo in its state and gkm serving its generation, where
//	          an earlier read would seal to none, and keep gkm's none for its 5 minutes (controller.SmtpReportKeySource);
//	          the network's report token is read from its zone's row, since the world's zones name no rua (the World's
//	          header). No receiver or prover host runs, as no report asks one
//	reports   BurstsPerNetwork bursts a network, one goroutine each with random pauses, each a reporter's connections
//	          sending aggregates of its own through the world's lb to the platform's interface on 25 under STARTTLS,
//	          each signed by the internet sender's DKIM key, which the intake verifies from the world's DNS; a delivery
//	          refused for now (451 while api or gkm cannot tell the key, a dial while the platform restarts) is tried
//	          again as an MTA retries, and every DuplicateEvery-th report is sent again once taken, as a reporter whose
//	          250 was lost sends it
//	chaos     one burst of two steps, each once its share of the deliveries is answered: the platform listener
//	          restarted on its stores, falling on the deliveries in flight, its new run building its intake again;
//	          then, between two tries, gkm stopped and api restarted, its report key source holding nothing, and gkm
//	          started again only once api's gate holds each network and a delivery has been answered 451 for want of
//	          gkm: a key api cannot tell now is a report its reporter sends again, never one sealed to none
//
// The checks, strict:
//   - each network's reports page lists exactly its reports' aggregate rows, one a report, each with its report's
//     counts, its domain, its reporter and the UTC day its range begins; and exactly one sealed rua copy a report,
//     whatever was sent twice; no row or copy of another network's;
//   - each copy opens under one generation of the network's mailbox drop key, as the owner's key client derives it,
//     bound to the network and to its arrival as the page answers it, to the bytes of exactly one of its reports, and
//     opens bound to no other network;
//   - the platform's intakes counted each report once, answered nothing but counted and duplicate past calls they
//     could not finish (451s, which the retries answered), and every report sent twice met a duplicate.
//
// Run it through the build's wrapper: -tags 'sidecar_e2e sidecar_pending sidecar_localstack'.

package sidecare2e

import (
	"bytes"
	"context"
	"crypto/hpke"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/netip"
	"net/smtp"
	"net/textproto"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	moxdkim "github.com/mjl-/mox/dkim"
	moxdns "github.com/mjl-/mox/dns"
	moxsmtp "github.com/mjl-/mox/smtp"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/smtpedgeservice"

	"github.com/urnetwork/server/model"
)

// What the report sub-case runs with (QD-702).
type mailReportSealSettings struct {
	// The mail case's world and networks (newMailWorld, addMailNetworks): their counts, ages, grants and the mail
	// helpers' bounds, of which the owner's mail and the reporter's address, its EHLO name and its MTA, are read here.
	Mail *mailConcurrencySettings
	// Each network's bursts, each a reporter's connections sending aggregates of its own, and each burst's reports.
	BurstsPerNetwork int
	ReportsPerBurst  int
	// Every DuplicateEvery-th report of a burst is sent again once taken; non-positive sends none twice.
	DuplicateEvery int
	// What a report's one record counts: RecordBaseCount plus the report's place among its network's, so each report's
	// counts are its own; and its disposition, Dispositions' in turn by that place.
	RecordBaseCount int
	Dispositions    []string
	// The DKIM selector the record says the network's mail was signed under: a network key's 26 letters.
	RecordDkimSelector string
	// The local part the reporter's aggregates come From, sidecartest.BuildSmtpAggregateMessage's.
	ReporterLocalPart string
	// Whether chaos restarts the platform listener on its stores once its share of the deliveries is answered, falling
	// on the deliveries in flight.
	ChaosPlatform bool
	// Whether chaos then takes gkm away once its share is answered: between two deliveries it stops gkm and restarts
	// api, whose report key source starts again with nothing cached, waits until api's gate holds each network, and
	// starts gkm again only once a delivery has been answered 451 for want of it.
	ChaosGkmOutage bool
	// One delivery's bound (its dial, the dialog, STARTTLS's handshake and the message), how soon a delivery refused
	// for now is tried again, as an MTA retries a 4xx, and how long a report may be refused before its burst fails.
	DeliveryTimeout       time.Duration
	DeliveryRetryInterval time.Duration
	DeliveryRetryTimeout  time.Duration
	// How long a network's setup waits for its mailbox in the owner's verified state and for its drop key at gkm, and
	// how often it asks, since neither has a hook a test reads.
	SetupTimeout      time.Duration
	SetupPollInterval time.Duration
	// How long a reports read is tried again while api answers 503, as after its restart before its gate cache holds
	// the network, and how often.
	ReadTimeout       time.Duration
	ReadRetryInterval time.Duration
	// The bursts' pauses and seed, and each phase's bound.
	Bursts       *WorldBurstSettings
	PhaseTimeout time.Duration
}

func defaultMailReportSealSettings() *mailReportSealSettings {
	mail := defaultMailConcurrencySettings()
	// two networks, so each one's reports are kept from the other's, of the mail case's least sidecars, its owner
	// reading; the groups are the main case's, and the conventions' 10 minutes bound the case
	mail.NetworkCount = 2
	mail.ClientsPerNetwork = 3
	mail.Groups = nil
	mail.CaseTimeout = 10 * time.Minute
	return &mailReportSealSettings{
		Mail:                  mail,
		BurstsPerNetwork:      2,
		ReportsPerBurst:       4,
		DuplicateEvery:        2,
		RecordBaseCount:       3,
		Dispositions:          []string{"none", "quarantine", "reject"},
		RecordDkimSelector:    "abcdefghijklmnopqrstuvwxyz",
		ReporterLocalPart:     "noreply-dmarc",
		ChaosPlatform:         true,
		ChaosGkmOutage:        true,
		DeliveryTimeout:       30 * time.Second,
		DeliveryRetryInterval: 250 * time.Millisecond,
		DeliveryRetryTimeout:  2 * time.Minute,
		SetupTimeout:          2 * time.Minute,
		SetupPollInterval:     200 * time.Millisecond,
		ReadTimeout:           2 * time.Minute,
		ReadRetryInterval:     250 * time.Millisecond,
		Bursts:                DefaultWorldBurstSettings(),
		PhaseTimeout:          4 * time.Minute,
	}
}

// One network of the sub-case as its setup left it. Safe for concurrent use once set up.
type mailReportNetwork struct {
	*WorldNetwork
	// its zone's apex without its final dot, every report's header_from
	Domain string
	// its member's mailbox, the network mail key's repo
	MailboxId sidecar.Id
	// <token>@<report host>, where its reports go
	Recipient string

	stateLock sync.Mutex
	reports   []*mailReport
}

// Keeps report as one sent to the network.
func (self *mailReportNetwork) add(report *mailReport) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.reports = append(self.reports, report)
}

// Every report sent to the network, by report id.
func (self *mailReportNetwork) reportIdReports() map[string]*mailReport {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	reportIdReports := map[string]*mailReport{}
	for _, report := range self.reports {
		reportIdReports[report.ReportId] = report
	}
	return reportIdReports
}

// One aggregate the sub-case sent: its report id, its bytes as signed, and its one record's count and disposition.
type mailReport struct {
	ReportId    string
	Raw         []byte
	Count       int
	Disposition string
}

// Turns mail on for network through its owner's MailClient, which ensures mailcfg, the member's mailbox and the owner's
// RepoAdmin of it, then POST /mail/enable, and waits until that mailbox, the network mail key's repo, can be sealed to:
// in the owner's verified state, the network's log at the owner's position at every gate, and its drop key answered by
// the owner's key client from the generations gkm serves (the file's header). Then the network's report token, from its
// zone's row.
func setUpMailReportNetwork(ctx context.Context, world *World, settings *mailReportSealSettings, network *WorldNetwork) (*mailReportNetwork, error) {
	owner := network.Owner()
	mailSettings := settings.Mail.Mail
	client, err := world.StartMail(ctx, owner, false, mailSettings)
	if err != nil {
		return nil, err
	}
	if _, err := world.EnableMail(ctx, owner, client, mailSettings); err != nil {
		return nil, err
	}
	zone, err := worldDeviceZone(ctx, owner)
	if err != nil {
		return nil, err
	}
	set := &mailReportNetwork{WorldNetwork: network, Domain: strings.TrimSuffix(zone.Apex(), ".")}
	waitCtx, cancel := context.WithTimeout(ctx, settings.SetupTimeout)
	defer cancel()
	for {
		mailboxId, ok := worldMemberMailboxId(owner)
		if ok {
			set.MailboxId = mailboxId
			break
		}
		select {
		case <-waitCtx.Done():
			return nil, fmt.Errorf("%s's verified state never held its member's mailbox: %w", owner.Name, waitCtx.Err())
		case <-time.After(settings.SetupPollInterval):
		}
	}
	if err := network.Sync(ctx, owner.Follower().Client().Pin().GetVersion()); err != nil {
		return nil, fmt.Errorf("%s's mailbox at the gates: %w", network.Name, err)
	}
	for {
		_, err := owner.Gkm.DropKey(waitCtx, set.MailboxId)
		if err == nil {
			break
		}
		select {
		case <-waitCtx.Done():
			return nil, fmt.Errorf("%s reads no drop key of its mailbox from gkm: %v (%w)", owner.Name, err, waitCtx.Err())
		case <-time.After(settings.SetupPollInterval):
		}
	}
	hostsSettings := world.HostsSettings()
	token, err := worldNetworkReportToken(ctx, network.NetworkId, hostsSettings.DnsRoot)
	if err != nil {
		return nil, err
	}
	set.Recipient = token + "@" + hostsSettings.SmtpReportHost
	return set, nil
}

// report's bytes as the internet sender's reporting MTA sends them: a gzip DMARC aggregate from its domain to the
// network's report address, its one record for the network's domain from the world's sending address, DKIM-aligned and
// SPF-unaligned as the edge's mail is (D-SMTP-5), signed under the sender's selector key as its mail is
// (worldInternetSender.Message).
func mailReportBytes(ctx context.Context, world *World, settings *mailReportSealSettings, network *mailReportNetwork, reportId string, begin time.Time, count int, disposition string) ([]byte, error) {
	sender := world.dnssec.sender
	if sender == nil {
		return nil, errors.New("the world serves no internet sender to report")
	}
	xml := sidecartest.BuildSmtpDmarcXml(sender.Domain(), reportId, begin, []*sidecartest.SmtpFixtureDmarcRecord{{
		SourceIp:     world.HostsSettings().SmtpAddress.String(),
		Count:        count,
		HeaderFrom:   network.Domain,
		Disposition:  disposition,
		DkimAligned:  "pass",
		SpfAligned:   "fail",
		DkimDomain:   network.Domain,
		DkimSelector: settings.RecordDkimSelector,
		DkimResult:   "pass",
	}})
	message := sidecartest.BuildSmtpAggregateMessage(sender.Domain(), network.Recipient, "application/gzip", "report.xml.gz", sidecartest.SmtpFixtureGzip(xml))
	domain, err := moxdns.ParseDomain(sender.Domain())
	if err != nil {
		return nil, err
	}
	selector, err := moxdns.ParseDomain(sender.settings.DkimSelector)
	if err != nil {
		return nil, err
	}
	headers, err := moxdkim.Sign(ctx, slog.New(slog.DiscardHandler), moxsmtp.Localpart(settings.ReporterLocalPart), domain, []moxdkim.Selector{{
		Hash:          "sha256",
		HeaderRelaxed: true,
		BodyRelaxed:   true,
		Headers:       sender.settings.DkimHeaders,
		PrivateKey:    sender.dkimKey,
		Domain:        selector,
	}}, false, bytes.NewReader(message))
	if err != nil {
		return nil, err
	}
	return append([]byte(headers), message...), nil
}

// Whether err is an SMTP reply of 5xx: a refusal no retry mends, which an MTA bounces.
func mailReportPermanent(err error) bool {
	var reply *textproto.Error
	return errors.As(err, &reply) && 500 <= reply.Code
}

// One delivery of raw to recipient as the reporter's MTA makes it, through the world's lb to the platform listener's
// interface on 25 (world_lb_test.go): the lb's PROXY header naming the MTA's address, the banner, EHLO, STARTTLS under
// the report host's name with the platform's certificate verified against the world's own, then EHLO, MAIL, RCPT and
// DATA inside TLS. nil once the DATA's end was answered 250.
func mailReportDeliver(ctx context.Context, world *World, settings *mailReportSealSettings, recipient string, raw []byte) error {
	ctx, cancel := context.WithTimeout(ctx, settings.DeliveryTimeout)
	defer cancel()
	hostsSettings := world.HostsSettings()
	mailSettings := settings.Mail.Mail
	conn, err := world.DialLb(ctx, mailSettings.SenderAddress, netip.AddrPortFrom(world.Settings().PlatformLbAddress, worldMailPort))
	if err != nil {
		return err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		conn.SetDeadline(deadline)
	}
	client, err := smtp.NewClient(conn, hostsSettings.MxReportHost)
	if err != nil {
		return fmt.Errorf("the banner: %w", err)
	}
	defer client.Close()
	if err := client.Hello(mailSettings.SenderHelo); err != nil {
		return fmt.Errorf("EHLO: %w", err)
	}
	if ok, _ := client.Extension("STARTTLS"); !ok {
		return errors.New("the platform listener offers no STARTTLS")
	}
	roots := x509.NewCertPool()
	roots.AddCert(world.credentials.platformCertificate.Leaf)
	if err := client.StartTLS(&tls.Config{ServerName: hostsSettings.MxReportHost, RootCAs: roots, MinVersion: tls.VersionTLS12}); err != nil {
		return fmt.Errorf("STARTTLS: %w", err)
	}
	if err := client.Mail(world.dnssec.sender.Address(settings.ReporterLocalPart)); err != nil {
		return fmt.Errorf("MAIL: %w", err)
	}
	if err := client.Rcpt(recipient); err != nil {
		return fmt.Errorf("RCPT: %w", err)
	}
	writer, err := client.Data()
	if err != nil {
		return fmt.Errorf("DATA: %w", err)
	}
	if _, err := writer.Write(raw); err != nil {
		return fmt.Errorf("the message: %w", err)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("the DATA's end: %w", err)
	}
	// the report is taken at the 250, so a QUIT a cut connection never answers changes nothing
	client.Quit()
	return nil
}

// mailReportDeliver until the DATA's end is answered 250, tried again every DeliveryRetryInterval within
// DeliveryRetryTimeout while it is refused for now, as an MTA retries; a 5xx ends it at once, as it bounces. The last
// refusal otherwise. Each try holds attempts for reading, so chaos holding it for writing falls between tries.
func mailReportAwaitDelivery(ctx context.Context, world *World, settings *mailReportSealSettings, attempts *sync.RWMutex, recipient string, raw []byte) error {
	ctx, cancel := context.WithTimeout(ctx, settings.DeliveryRetryTimeout)
	defer cancel()
	tries := 0
	for {
		tries += 1
		err := func() error {
			attempts.RLock()
			defer attempts.RUnlock()
			return mailReportDeliver(ctx, world, settings, recipient, raw)
		}()
		if err == nil {
			return nil
		}
		if mailReportPermanent(err) {
			return fmt.Errorf("the delivery to %s, try %d, was refused: %w", recipient, tries, err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the delivery to %s, %d tries: %w (%w)", recipient, tries, err, ctx.Err())
		case <-time.After(settings.DeliveryRetryInterval):
		}
	}
}

// Every page of GET /smtp/reports from since on as network's owner reads it with its token, through api in process,
// each page tried again every ReadRetryInterval within ReadTimeout while api answers 503.
func mailReportsRead(ctx context.Context, world *World, settings *mailReportSealSettings, network *mailReportNetwork, since time.Time) (*model.SmtpReportsResult, error) {
	ctx, cancel := context.WithTimeout(ctx, settings.ReadTimeout)
	defer cancel()
	owner := network.Owner()
	all := &model.SmtpReportsResult{}
	cursor := ""
	for {
		path := fmt.Sprintf("/smtp/reports?since=%d", since.UnixMilli())
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		status, answer, err := world.T3().Api().Call(ctx, http.MethodGet, path, owner.Bearer(), nil)
		if err != nil {
			return nil, err
		}
		if status == http.StatusServiceUnavailable {
			select {
			case <-ctx.Done():
				return nil, fmt.Errorf("%s's reports stayed unreadable: %s (%w)", owner.Name, answer, ctx.Err())
			case <-time.After(settings.ReadRetryInterval):
			}
			continue
		}
		if status != http.StatusOK {
			return nil, fmt.Errorf("%s's GET /smtp/reports answered %d: %s", owner.Name, status, answer)
		}
		page := &model.SmtpReportsResult{}
		if err := json.Unmarshal(answer, page); err != nil {
			return nil, err
		}
		all.Complaints = append(all.Complaints, page.Complaints...)
		all.Aggregates = append(all.Aggregates, page.Aggregates...)
		if page.Cursor == "" {
			return all, nil
		}
		cursor = page.Cursor
	}
}

// The drop key pairs of every generation of network's mailbox its owner holds, the newest's generation last: what a
// copy sealed to the network mail key at any of them opens under.
func mailReportDropKeys(ctx context.Context, network *mailReportNetwork) ([]hpke.PrivateKey, error) {
	owner := network.Owner()
	newest, err := owner.Gkm.DropKey(ctx, network.MailboxId)
	if err != nil {
		return nil, fmt.Errorf("%s's newest drop key of its mailbox: %w", owner.Name, err)
	}
	privateKeys := []hpke.PrivateKey{}
	for generation := uint64(1); generation <= newest.Generation; generation += 1 {
		key, err := owner.Gkm.RepoKey(ctx, network.MailboxId, generation)
		if err != nil {
			return nil, fmt.Errorf("%s's generation %d of its mailbox: %w", owner.Name, generation, err)
		}
		privateKey, err := key.DropKeyPair()
		if err != nil {
			return nil, err
		}
		privateKeys = append(privateKeys, privateKey)
	}
	return privateKeys, nil
}

// See the file's header.
func TestE2eMailReportSeal(t *testing.T) {
	settings := defaultMailReportSealSettings()
	e2eTestEnv().Run(t, func(t testing.TB) {
		world := newMailWorld(t, settings.Mail)
		defer world.Close()
		if world.dnssec.sender == nil {
			t.Fatal("the world serves no internet sender to report")
		}
		senderDomain := world.dnssec.sender.Domain()
		ctx, cancel := world.StepContext(t.Context(), settings.PhaseTimeout)
		worldNetworks := addMailNetworks(ctx, t, world, settings.Mail)
		cancel()

		// each network's mail on and its mailbox sealable, one burst a network
		networks := make([]*mailReportNetwork, len(worldNetworks))
		setUps := []*WorldBurst{}
		for index, network := range worldNetworks {
			setUps = append(setUps, &WorldBurst{
				Name:      network.Name + "'s mail key",
				StepCount: 1,
				Step: func(ctx context.Context, step int, random *rand.Rand) error {
					set, err := setUpMailReportNetwork(ctx, world, settings, network)
					networks[index] = set
					return err
				},
			})
		}
		ctx, cancel = world.StepContext(t.Context(), settings.PhaseTimeout)
		setUpSettings := *settings.Bursts
		setUpSettings.PhaseTimeout = settings.PhaseTimeout
		if err := RunBursts(ctx, t, "the networks' mail keys", setUps, &setUpSettings); err != nil {
			cancel()
			world.FailProgress(t, "the networks' mail keys", err, setUps)
		}
		cancel()
		for _, burst := range setUps {
			if errs := burst.Errors(); 0 < len(errs) {
				world.FailProgress(t, burst.Name, errors.Join(errs...), setUps)
			}
		}

		// the reports and the chaos, all at once, every report's range beginning on the UTC day of the reads' since
		since := time.Now()
		year, month, day := since.UTC().Date()
		begin := time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
		duplicatesPerBurst := 0
		if 0 < settings.DuplicateEvery {
			duplicatesPerBurst = settings.ReportsPerBurst / settings.DuplicateEvery
		}
		deliveryCount := len(networks) * settings.BurstsPerNetwork * (settings.ReportsPerBurst + duplicatesPerBurst)
		duplicateCount := len(networks) * settings.BurstsPerNetwork * duplicatesPerBurst
		// the deliveries answered 250, which the chaos waits on, and what each try holds for reading
		answered := sidecar.NewMonitorValue(0)
		attempts := &sync.RWMutex{}
		deliver := func(ctx context.Context, network *mailReportNetwork, raw []byte) error {
			if err := mailReportAwaitDelivery(ctx, world, settings, attempts, network.Recipient, raw); err != nil {
				return err
			}
			answered.Update(func(count int) int {
				return count + 1
			})
			return nil
		}
		bursts := []*WorldBurst{}
		for _, network := range networks {
			for burstIndex := range settings.BurstsPerNetwork {
				bursts = append(bursts, &WorldBurst{
					Name:      fmt.Sprintf("%s's reporter %d", network.Name, burstIndex),
					StepCount: settings.ReportsPerBurst,
					Step: func(ctx context.Context, index int, random *rand.Rand) error {
						place := burstIndex*settings.ReportsPerBurst + index
						report := &mailReport{
							ReportId:    fmt.Sprintf("%s-report-%d", network.Name, place),
							Count:       settings.RecordBaseCount + place,
							Disposition: settings.Dispositions[place%len(settings.Dispositions)],
						}
						raw, err := mailReportBytes(ctx, world, settings, network, report.ReportId, begin, report.Count, report.Disposition)
						if err != nil {
							return err
						}
						report.Raw = raw
						network.add(report)
						if err := deliver(ctx, network, raw); err != nil {
							return fmt.Errorf("%s: %w", report.ReportId, err)
						}
						if 0 < settings.DuplicateEvery && (index+1)%settings.DuplicateEvery == 0 {
							if err := deliver(ctx, network, raw); err != nil {
								return fmt.Errorf("%s sent again: %w", report.ReportId, err)
							}
						}
						return nil
					},
				})
			}
		}
		chaosSteps := []func(ctx context.Context) error{}
		if settings.ChaosPlatform {
			chaosSteps = append(chaosSteps, func(ctx context.Context) error {
				t.Logf("chaos restarts mxedgeplatform once %d deliveries are answered", answered.Value())
				return world.Restart(WorldBlockMxedgePlatform)
			})
		}
		if settings.ChaosGkmOutage {
			chaosSteps = append(chaosSteps, func(ctx context.Context) error {
				// between two tries: gkm gone and api restarted, its report key source holding nothing, and api's gate
				// holding each network, so a 451 from here on is gkm's absence and never api's own load
				failureCount, err := func() (int, error) {
					attempts.Lock()
					defer attempts.Unlock()
					t.Logf("chaos stops gkm and restarts api once %d deliveries are answered", answered.Value())
					if err := world.Stop(WorldBlockGkm); err != nil {
						return 0, err
					}
					if err := world.T3().RestartApi(); err != nil {
						return 0, err
					}
					for _, network := range networks {
						if err := world.T3().SyncGates(ctx, network.NetworkId, network.Owner().Follower().Client().Pin().GetVersion()); err != nil {
							return 0, err
						}
					}
					failureCount, _ := world.PlatformReportFailures()
					return failureCount, nil
				}()
				if err != nil {
					return err
				}
				for {
					count, update := world.PlatformReportFailures()
					if failureCount < count {
						break
					}
					select {
					case <-ctx.Done():
						return fmt.Errorf("no delivery met gkm gone: %w", ctx.Err())
					case <-update:
					}
				}
				t.Logf("chaos starts gkm again once a delivery met it gone, %d deliveries answered", answered.Value())
				return world.Start(WorldBlockGkm)
			})
		}
		if 0 < len(chaosSteps) {
			bursts = append(bursts, &WorldBurst{
				Name:      "the chaos",
				StepCount: len(chaosSteps),
				Step: func(ctx context.Context, index int, random *rand.Rand) error {
					// each step once its share of the deliveries is answered
					share := (index + 1) * deliveryCount / (len(chaosSteps) + 1)
					for {
						count, update := answered.Get()
						if share <= count {
							break
						}
						select {
						case <-ctx.Done():
							return fmt.Errorf("%d of %d deliveries answered, short of %d: %w", count, deliveryCount, share, ctx.Err())
						case <-update:
						}
					}
					return chaosSteps[index](ctx)
				},
			})
		}
		ctx, cancel = world.StepContext(t.Context(), settings.PhaseTimeout)
		burstSettings := *settings.Bursts
		burstSettings.PhaseTimeout = settings.PhaseTimeout
		if err := RunBursts(ctx, t, "the reports", bursts, &burstSettings); err != nil {
			cancel()
			t.Logf("the platform's intakes answered %v", world.PlatformReportOutcomes())
			world.FailProgress(t, "the reports", err, bursts)
		}
		cancel()
		FailBurstErrors(t, bursts)
		if t.Failed() {
			t.Logf("the platform's intakes answered %v", world.PlatformReportOutcomes())
			t.FailNow()
		}
		chaosBlocks := []string{}
		if settings.ChaosPlatform {
			chaosBlocks = append(chaosBlocks, WorldBlockMxedgePlatform)
		}
		if settings.ChaosGkmOutage {
			chaosBlocks = append(chaosBlocks, WorldBlockGkm)
		}
		for _, name := range chaosBlocks {
			if runs := world.Block(name).RunCount(); runs != 2 {
				t.Fatalf("%s ran %d times through the chaos, not 2", name, runs)
			}
		}
		if answeredCount := answered.Value(); answeredCount != deliveryCount {
			t.Fatalf("%d deliveries were answered 250, not the %d sent", answeredCount, deliveryCount)
		}

		// the intakes counted each report once, every duplicate met one, and nothing else was answered but the 451s the
		// retries took
		reportCount := 0
		for _, network := range networks {
			reportCount += len(network.reportIdReports())
		}
		outcomes := world.PlatformReportOutcomes()
		t.Logf("the platform's intakes answered %v to %d deliveries of %d reports", outcomes, deliveryCount, reportCount)
		if outcomes[smtpedgeservice.SmtpReportOutcomeCounted] != reportCount {
			t.Errorf("the platform's intakes counted %d reports, not the %d sent", outcomes[smtpedgeservice.SmtpReportOutcomeCounted], reportCount)
		}
		if outcomes[smtpedgeservice.SmtpReportOutcomeDuplicate] < duplicateCount {
			t.Errorf("the platform's intakes met %d duplicates, short of the %d reports sent twice", outcomes[smtpedgeservice.SmtpReportOutcomeDuplicate], duplicateCount)
		}
		for outcome, count := range outcomes {
			switch outcome {
			case smtpedgeservice.SmtpReportOutcomeCounted, smtpedgeservice.SmtpReportOutcomeDuplicate, worldReportOutcomeFailed:
			default:
				t.Errorf("the platform's intakes answered %s %d times", outcome, count)
			}
		}

		// each network's reports read back by its owner, its copies opened under its mailbox's drop key
		ctx, cancel = world.StepContext(t.Context(), settings.PhaseTimeout)
		defer cancel()
		for _, network := range networks {
			page, err := mailReportsRead(ctx, world, settings, network, since)
			if err != nil {
				world.FailProgress(t, network.Name+"'s reports", err, nil)
			}
			reportIdReports := network.reportIdReports()
			aggregated := map[string]bool{}
			for _, aggregate := range page.Aggregates {
				report := reportIdReports[aggregate.ReportId]
				if report == nil {
					t.Errorf("%s's reports list an aggregate row of %s, none of its own", network.Name, aggregate.ReportId)
					continue
				}
				if aggregated[aggregate.ReportId] {
					t.Errorf("%s's reports list %s's aggregate row twice", network.Name, aggregate.ReportId)
				}
				aggregated[aggregate.ReportId] = true
				count := int64(report.Count)
				quarantineCount, rejectCount := int64(0), int64(0)
				switch report.Disposition {
				case "quarantine":
					quarantineCount = count
				case "reject":
					rejectCount = count
				}
				if aggregate.Reporter != senderDomain || aggregate.Kind != smtpedgeservice.SmtpAggregateKindDmarc ||
					aggregate.Domain != network.Domain || aggregate.Day != begin.UnixMilli() || aggregate.MessageCount != count ||
					aggregate.DkimPassCount != count || aggregate.SpfPassCount != 0 || aggregate.QuarantineCount != quarantineCount ||
					aggregate.RejectCount != rejectCount {
					t.Errorf("%s's aggregate row of %s is %+v, not %d messages of %s from %s, %s, on %s", network.Name, aggregate.ReportId, *aggregate, count, network.Domain, senderDomain, report.Disposition, begin.Format(time.DateOnly))
				}
			}
			for reportId := range reportIdReports {
				if !aggregated[reportId] {
					t.Errorf("%s's reports list no aggregate row of %s", network.Name, reportId)
				}
			}

			privateKeys, err := mailReportDropKeys(ctx, network)
			if err != nil {
				t.Fatal(err)
			}
			openedReportIds := map[string]bool{}
			for _, complaint := range page.Complaints {
				if complaint.Type != smtpedgeservice.SmtpComplaintTypeRua || complaint.Reporter != senderDomain || complaint.Domain != network.Domain || len(complaint.SealedReport) == 0 {
					t.Errorf("%s's reports list a copy of type %s from %s for %s of %d bytes, not a sealed rua copy from %s for %s", network.Name, complaint.Type, complaint.Reporter, complaint.Domain, len(complaint.SealedReport), senderDomain, network.Domain)
					continue
				}
				arrival := time.UnixMilli(complaint.ArrivalTime)
				var plain []byte
				var openErr error
				for _, privateKey := range privateKeys {
					opened, err := sidecartest.OpenSmtpSealedReportFixture(privateKey, sidecar.Id(network.NetworkId), arrival, complaint.SealedReport)
					if err == nil {
						plain = opened
						break
					}
					openErr = err
				}
				if plain == nil {
					t.Errorf("%s's copy %s opens under none of its mailbox's %d generations: %v", network.Name, complaint.ComplaintId, len(privateKeys), openErr)
					continue
				}
				var report *mailReport
				for _, sent := range reportIdReports {
					if bytes.Equal(sent.Raw, plain) {
						report = sent
					}
				}
				if report == nil {
					t.Errorf("%s's copy %s opens to %d bytes, none of its reports'", network.Name, complaint.ComplaintId, len(plain))
					continue
				}
				if openedReportIds[report.ReportId] {
					t.Errorf("%s's reports hold %s's copy twice", network.Name, report.ReportId)
				}
				openedReportIds[report.ReportId] = true
				// a copy is bound to its row's network: under the same key and arrival it opens as no other network's
				for _, other := range networks {
					if other == network {
						continue
					}
					for _, privateKey := range privateKeys {
						if _, err := sidecartest.OpenSmtpSealedReportFixture(privateKey, sidecar.Id(other.NetworkId), arrival, complaint.SealedReport); err == nil {
							t.Errorf("%s's copy of %s opens bound to %s", network.Name, report.ReportId, other.Name)
						}
					}
				}
			}
			for reportId := range reportIdReports {
				if !openedReportIds[reportId] {
					t.Errorf("%s's reports hold no copy of %s sealed to its mail key", network.Name, reportId)
				}
			}
			t.Logf("%s's owner read %d aggregate rows and %d sealed copies back, each opening under its mailbox's %d generations", network.Name, len(page.Aggregates), len(page.Complaints), len(privateKeys))
		}
	})
}
