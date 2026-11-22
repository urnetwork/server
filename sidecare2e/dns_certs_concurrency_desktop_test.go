//go:build sidecar_e2e && sidecar_pending && sidecar_localstack

// An ordering desktop's parts for the delegated phase of the dns and certificates case (dns_certs_concurrency_test.go;
// TLS-M6-05, DESIGN-DETAILS 17.2.7 and 17.2.9): what ur daemon gives Sidecar.TlsCustomOrdererDeps beside the Sidecar
// itself, as cli/'s tls_acme.go composes it.
//
//	ca        sidecar.TlsAcmeClient on acmez's acme package against the case's Pebble, as cli/'s urTlsAcmeClient is
//	          (cli/ is package main, which no test imports), one request a method, acmez retrying a badNonce
//	resolver  the case's resolver (dnsCertsResolver), which answers each delegated _acme-challenge with its CNAME and
//	          the edge's records of the target, in place of the desktop's system resolver
//	keys      the secret repos' key files, which cli/ reads and writes through the network's evs: a map the case's
//	          desktops share, a stand-in named in the case's header, since the case is about the delegated DNS-01
//	          path, and the e2e evs and gkm cases hold secret repos to their own rules

package sidecare2e

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mholt/acmez/v3/acme"
	"github.com/miekg/dns"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/certsservice"
	"github.com/urnetwork/sidecar/dnsservice"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/sidecart3"
)

// RFC 8555's problem type prefix.
const dnsCertsAcmeProblemPrefix = "urn:ietf:params:acme:error:"

var _ sidecar.TlsAcmeClient = (*dnsCertsAcmeClient)(nil)

// See the file header.
type dnsCertsAcmeClient struct {
	client *acme.Client
}

// A client of the CA at directoryUrl over httpClient, its polls the orderer's own OrderPollInterval.
func newDnsCertsAcmeClient(directoryUrl string, httpClient *http.Client, settings *sidecar.TlsCustomSettings) *dnsCertsAcmeClient {
	return &dnsCertsAcmeClient{client: &acme.Client{
		Directory:    directoryUrl,
		HTTPClient:   httpClient,
		UserAgent:    "ur-e2e",
		PollInterval: settings.OrderPollInterval,
		PollTimeout:  settings.OrderPollInterval,
	}}
}

// err as 17.7's types: a problem, gone, or an outcome unknown.
func dnsCertsAcmeError(err error) error {
	if err == nil {
		return nil
	}
	var problem acme.Problem
	if errors.As(err, &problem) {
		if problem.Status == http.StatusNotFound {
			return sidecar.ErrTlsAcmeGone
		}
		return &sidecar.TlsAcmeProblem{Type: strings.TrimPrefix(problem.Type, dnsCertsAcmeProblemPrefix), Status: problem.Status}
	}
	return err
}

func (self *dnsCertsAcmeClient) account(key crypto.Signer, accountUri string) acme.Account {
	return acme.Account{Location: accountUri, PrivateKey: key}
}

func (self *dnsCertsAcmeClient) NewAccount(ctx context.Context, key crypto.Signer, onlyReturnExisting bool) (string, error) {
	if onlyReturnExisting {
		account, err := self.client.GetAccount(ctx, acme.Account{PrivateKey: key})
		if err != nil {
			return "", dnsCertsAcmeError(err)
		}
		return account.Location, nil
	}
	account, err := self.client.NewAccount(ctx, acme.Account{PrivateKey: key, TermsOfServiceAgreed: true})
	if err != nil {
		return "", dnsCertsAcmeError(err)
	}
	return account.Location, nil
}

func (self *dnsCertsAcmeClient) Account(ctx context.Context, key crypto.Signer, accountUri string) error {
	account, err := self.client.GetAccount(ctx, acme.Account{PrivateKey: key})
	if err != nil {
		return dnsCertsAcmeError(err)
	}
	if account.Location != accountUri {
		return &sidecar.TlsAcmeProblem{Type: sidecar.TlsAcmeProblemUnauthorized, Status: http.StatusForbidden}
	}
	return nil
}

func (self *dnsCertsAcmeClient) KeyChange(ctx context.Context, accountUri string, oldKey crypto.Signer, newKey crypto.Signer) error {
	_, err := self.client.AccountKeyRollover(ctx, self.account(oldKey, accountUri), newKey)
	return dnsCertsAcmeError(err)
}

func (self *dnsCertsAcmeClient) Deactivate(ctx context.Context, key crypto.Signer, accountUri string) error {
	account := self.account(key, accountUri)
	account.Status = acme.StatusDeactivated
	_, err := self.client.UpdateAccount(ctx, account)
	return dnsCertsAcmeError(err)
}

func dnsCertsAcmeOrder(order acme.Order) *sidecar.TlsAcmeOrder {
	return &sidecar.TlsAcmeOrder{
		Url:            order.Location,
		Status:         order.Status,
		Authorizations: order.Authorizations,
		Finalize:       order.Finalize,
		Certificate:    order.Certificate,
	}
}

func (self *dnsCertsAcmeClient) NewOrder(ctx context.Context, key crypto.Signer, accountUri string, name string, profile string, replacesLeaf []byte) (*sidecar.TlsAcmeOrder, error) {
	order := acme.Order{
		Identifiers: []acme.Identifier{{Type: "dns", Value: name}},
		Profile:     profile,
	}
	if 0 < len(replacesLeaf) {
		leaf, err := x509.ParseCertificate(replacesLeaf)
		if err != nil {
			return nil, err
		}
		certificateId, err := acme.ARIUniqueIdentifier(leaf)
		if err != nil {
			return nil, err
		}
		order.Replaces = certificateId
	}
	created, err := self.client.NewOrder(ctx, self.account(key, accountUri), order)
	if err != nil {
		return nil, dnsCertsAcmeError(err)
	}
	return dnsCertsAcmeOrder(created), nil
}

func (self *dnsCertsAcmeClient) Order(ctx context.Context, key crypto.Signer, accountUri string, orderUrl string) (*sidecar.TlsAcmeOrder, error) {
	order, err := self.client.GetOrder(ctx, self.account(key, accountUri), acme.Order{Location: orderUrl})
	if err != nil {
		return nil, dnsCertsAcmeError(err)
	}
	return dnsCertsAcmeOrder(order), nil
}

func (self *dnsCertsAcmeClient) Authorization(ctx context.Context, key crypto.Signer, accountUri string, url string) (*sidecar.TlsAcmeAuthorization, error) {
	authorization, err := self.client.GetAuthorization(ctx, self.account(key, accountUri), url)
	if err != nil {
		return nil, dnsCertsAcmeError(err)
	}
	result := &sidecar.TlsAcmeAuthorization{Url: url, Status: authorization.Status}
	for _, challenge := range authorization.Challenges {
		result.Challenges = append(result.Challenges, &sidecar.TlsAcmeChallenge{Type: challenge.Type, Url: challenge.URL, Token: challenge.Token, Status: challenge.Status})
		if challenge.Error != nil {
			result.ProblemType = strings.TrimPrefix(challenge.Error.Type, dnsCertsAcmeProblemPrefix)
		}
	}
	return result, nil
}

func (self *dnsCertsAcmeClient) Challenge(ctx context.Context, key crypto.Signer, accountUri string, url string) error {
	_, err := self.client.InitiateChallenge(ctx, self.account(key, accountUri), acme.Challenge{URL: url})
	return dnsCertsAcmeError(err)
}

func (self *dnsCertsAcmeClient) Finalize(ctx context.Context, key crypto.Signer, accountUri string, order *sidecar.TlsAcmeOrder, csr []byte) (*sidecar.TlsAcmeOrder, error) {
	finalized, err := self.client.FinalizeOrder(ctx, self.account(key, accountUri), acme.Order{Location: order.Url, Finalize: order.Finalize}, csr)
	if err != nil {
		// a poll that ran out reads the order again on the orderer's own clock
		if finalized.Status != "" && (errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "timed out")) {
			return dnsCertsAcmeOrder(finalized), nil
		}
		return nil, dnsCertsAcmeError(err)
	}
	return dnsCertsAcmeOrder(finalized), nil
}

func (self *dnsCertsAcmeClient) Certificate(ctx context.Context, key crypto.Signer, accountUri string, url string) ([][]byte, error) {
	chains, err := self.client.GetCertificateChain(ctx, self.account(key, accountUri), url)
	if err != nil {
		return nil, dnsCertsAcmeError(err)
	}
	if len(chains) == 0 {
		return nil, errors.New("the CA answered no chain")
	}
	der := [][]byte{}
	rest := chains[0].ChainPEM
	for {
		block, remaining := pem.Decode(rest)
		if block == nil {
			break
		}
		der = append(der, block.Bytes)
		rest = remaining
	}
	if len(der) == 0 {
		return nil, errors.New("the CA's chain holds no certificate")
	}
	return der, nil
}

func (self *dnsCertsAcmeClient) RenewalInfo(ctx context.Context, leaf []byte) (time.Time, time.Time, error) {
	certificate, err := x509.ParseCertificate(leaf)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	info, err := self.client.GetRenewalInfo(ctx, certificate)
	if err != nil {
		return time.Time{}, time.Time{}, dnsCertsAcmeError(err)
	}
	return info.SuggestedWindow.Start, info.SuggestedWindow.End, nil
}

func (self *dnsCertsAcmeClient) Revoke(ctx context.Context, key crypto.Signer, accountUri string, leaf []byte, reason int) error {
	certificate, err := x509.ParseCertificate(leaf)
	if err != nil {
		return err
	}
	return dnsCertsAcmeError(self.client.RevokeCertificate(ctx, self.account(key, accountUri), certificate, key, reason))
}

func (self *dnsCertsAcmeClient) DeactivateAuthorization(ctx context.Context, key crypto.Signer, accountUri string, url string) error {
	_, err := self.client.DeactivateAuthorization(ctx, self.account(key, accountUri), url)
	return dnsCertsAcmeError(err)
}

// RFC 8555 §8.1's key authorization: the token and the account key's RFC 7638 thumbprint.
func (self *dnsCertsAcmeClient) KeyAuthorization(key crypto.Signer, token string) (string, error) {
	publicKey, ok := key.Public().(*ecdsa.PublicKey)
	if !ok {
		return "", errors.New("an account key that is not P-256")
	}
	return token + "." + base64.RawURLEncoding.EncodeToString(sidecar.TlsCustomKeyThumbprint(publicKey)), nil
}

// The secret repos' key files the case's desktops share (see the file header). Safe for concurrent use.
type dnsCertsKeyFiles struct {
	stateLock sync.Mutex
	// by repo id and domain
	files map[sidecar.Id]map[string][]byte
}

var _ sidecar.TlsCustomRepoFiles = (*dnsCertsKeyFiles)(nil)

func newDnsCertsKeyFiles() *dnsCertsKeyFiles {
	return &dnsCertsKeyFiles{files: map[sidecar.Id]map[string][]byte{}}
}

func (self *dnsCertsKeyFiles) ReadKeyFile(ctx context.Context, repoId sidecar.Id, domain string) ([]byte, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	data, ok := self.files[repoId][domain]
	if !ok {
		return nil, sidecar.ErrTlsCustomKeyUnavailable
	}
	return append([]byte{}, data...), nil
}

func (self *dnsCertsKeyFiles) WriteKeyFile(ctx context.Context, repoId sidecar.Id, domain string, data []byte) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.files[repoId] == nil {
		self.files[repoId] = map[string][]byte{}
	}
	self.files[repoId][domain] = append([]byte{}, data...)
	return nil
}

// One custom domain of the delegated phase: its network and the publisher whose device serves its host, the host and
// its acme name, and the desktops that order it, each with its orderer and its passes' reports.
type dnsCertsCustomDomain struct {
	Network   *dnsCertsNetwork
	Publisher *dnsCertsPublisher
	Domain    string
	Host      string
	// <acme_token>.acme.<root>, canonical
	AcmeName string
	Desktops []*sidecart3.T3Device
	Orderers []*sidecar.TlsCustomDomainOrderer

	stateLock sync.Mutex
	// each desktop's issued rows, by its place in Desktops
	issued map[int][]sidecar.Id
	// the newOrders each desktop's passes granted
	newOrders map[int]int
}

func (self *dnsCertsCustomDomain) passed(desktop int, report *sidecar.TlsCustomPassReport) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.issued[desktop] = append(self.issued[desktop], report.Issued...)
	self.newOrders[desktop] += report.NewOrders
}

// The custom row of the domain's host its publisher's device queued, nil before.
func (self *dnsCertsCustomDomain) row(rows []*sidecar.TlsCertRecord) *sidecar.TlsCertRecord {
	for _, row := range rows {
		if row.CustomHost == self.Host {
			return row
		}
	}
	return nil
}

// The delegated phase (see the case's header): custom domains' certificates ordered by desktops under each domain's
// own ACME account at Pebble, by the delegated DNS-01 challenge, several domains at once and two desktops on the first,
// the certs worker restarting while a challenge's command holds its write.
func dnsCertsDelegatedPhase(t testing.TB, stack *dnsCertsStack, settings *dnsCertsSettings, networks []*dnsCertsNetwork, publishers []*dnsCertsPublisher) {
	world := stack.World
	ctx, cancel := world.StepContext(t.Context(), settings.DelegatedPhaseTimeout)
	defer cancel()
	customSettings := sidecar.DefaultTlsCustomSettings()
	customSettings.DirectoryUrl = stack.Pebble.DirectoryUrl
	customSettings.Profile = sidecartest.PebbleProfileTlsServer
	customSettings.OrderPollInterval = settings.CustomOrderPollInterval
	acme := newDnsCertsAcmeClient(stack.Pebble.DirectoryUrl, stack.Pebble.HttpClient, customSettings)
	files := newDnsCertsKeyFiles()
	domains := []*dnsCertsCustomDomain{}
	for index, network := range networks[:min(settings.CustomDomainCount, len(networks))] {
		var publisher *dnsCertsPublisher
		for _, candidate := range publishers {
			if candidate.Network == network {
				publisher = candidate
				break
			}
		}
		if publisher == nil {
			t.Fatalf("%s has no publisher to serve its custom host", network.Name)
		}
		domain := fmt.Sprintf(settings.CustomDomainFormat, network.Name)
		custom := &dnsCertsCustomDomain{
			Network:   network,
			Publisher: publisher,
			Domain:    domain,
			Host:      settings.CustomHostLabel + "." + domain,
			issued:    map[int][]sidecar.Id{},
			newOrders: map[int]int{},
		}
		acmeToken, err := dnsCertsAddCustomDomain(ctx, network.NetworkId, domain, custom.Host, network.Root, network.OwnerName(settings.TlsHost))
		if err != nil {
			t.Fatal(err)
		}
		custom.AcmeName = dns.CanonicalName(acmeToken + ".acme." + network.Root)
		// the customer's one-time CNAME of the host's _acme-challenge to the domain's acme name (§12.8 step 3)
		stack.Resolver.Delegate(custom.Host, custom.AcmeName)
		desktopCount := 1
		if index == 0 {
			desktopCount = settings.SameDomainDesktopCount
		}
		custom.Desktops = network.Clients[:min(desktopCount, len(network.Clients))]
		for _, desktop := range custom.Desktops {
			deps, _, err := desktop.Sidecar().TlsCustomOrdererDeps(ctx, acme, stack.Resolver, files)
			if err != nil {
				t.Fatalf("%s's orderer: %v", desktop.Name, err)
			}
			orderer, err := sidecar.NewTlsCustomDomainOrderer(deps, customSettings)
			if err != nil {
				t.Fatalf("%s's orderer: %v", desktop.Name, err)
			}
			custom.Orderers = append(custom.Orderers, orderer)
		}
		// the account, made by the network's owner device, an admin of its personal network (17.2.13), its key file in
		// the repo files the desktops share
		if _, err := custom.Orderers[0].CreateAccount(ctx, domain, sidecar.NewId(), 0); err != nil {
			t.Fatalf("%s's account for %s: %v", custom.Desktops[0].Name, domain, err)
		}
		domains = append(domains, custom)
	}

	// each publisher's pass queues one CSR for its custom host, which its zone now maps (17.2.5)
	for _, custom := range domains {
		if err := custom.Publisher.Component.CertManager().Sync(ctx); err != nil {
			t.Errorf("%s's certificate pass: %v", custom.Publisher.Device.Name, err)
		}
	}
	if err := dnsCertsAwaitCustomRows(ctx, domains, settings, func(custom *dnsCertsCustomDomain, row *sidecar.TlsCertRecord) bool {
		return row != nil
	}); err != nil {
		world.FailProgress(t, "the custom hosts' CSRs", err, nil)
	}

	// chaos: the certs worker restarts while the first challenge command holds its write (controller's barrier
	// before the write): the command's run is cut before the write is sent, its dispatch left unheard, and the
	// restarted worker settles it from the journal before it writes again (QD-1165)
	restart := &dnsCertsBarrierRestart{worker: stack.Worker}
	previousBarrier := controller.Testing_TlsCustomChallengeBarrier
	controller.Testing_TlsCustomChallengeBarrier = restart.barrier
	defer func() {
		controller.Testing_TlsCustomChallengeBarrier = previousBarrier
	}()

	// every desktop's passes at once, until its domain's row is issued
	bursts := []*WorldBurst{}
	for _, custom := range domains {
		for desktopIndex, orderer := range custom.Orderers {
			burst := &WorldBurst{Name: "ordering desktop " + custom.Desktops[desktopIndex].Name, StepCount: settings.CustomPassMaxCount}
			burst.Step = func(ctx context.Context, index int, random *rand.Rand) error {
				rows, err := dnsCertsCustomCerts(ctx, custom.Network.NetworkId)
				if err != nil {
					return err
				}
				if row := custom.row(rows); row != nil && row.State == sidecar.TlsCertStateCustomIssued {
					return dnsCertsStop
				}
				report, err := orderer.RunOnce(ctx)
				if report != nil {
					custom.passed(desktopIndex, report)
				}
				if err != nil {
					return fmt.Errorf("%s's pass: %w", custom.Desktops[desktopIndex].Name, err)
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(settings.CustomPassInterval):
				}
				return nil
			}
			bursts = append(bursts, burst)
		}
	}
	if err := RunBursts(ctx, t, "the delegated orders", bursts, settings.Bursts); err != nil {
		world.FailProgress(t, "the delegated orders", err, bursts)
	}
	for _, burst := range bursts {
		for _, err := range burst.Errors() {
			if !errors.Is(err, dnsCertsStop) {
				t.Errorf("%v", err)
			}
		}
	}
	if err := restart.Err(); err != nil {
		t.Errorf("the certs worker's restart mid-command: %v", err)
	}
	if !restart.Done() {
		t.Errorf("no challenge command reached its write, so the certs worker never restarted mid-command")
	}
	t.Logf("the delegated orders: the certs worker restarted at run %d while receipt %s held its write", stack.Worker.RunCount(), restart.ChallengeId())

	rows, err := dnsCertsJournal(ctx, networks[0].Root, 0, settings.JournalPageCount)
	if err != nil {
		t.Fatal(err)
	}
	for _, custom := range domains {
		dnsCertsCheckCustomDomain(t, ctx, stack, custom, rows)
	}
}

// What one domain's delegated orders must leave (see the case's header).
func dnsCertsCheckCustomDomain(t testing.TB, ctx context.Context, stack *dnsCertsStack, custom *dnsCertsCustomDomain, rows []dnsservice.DnsJournalRow) {
	t.Helper()
	certs, err := dnsCertsCustomCerts(ctx, custom.Network.NetworkId)
	if err != nil {
		t.Fatal(err)
	}
	row := custom.row(certs)
	if row == nil || row.State != sidecar.TlsCertStateCustomIssued || len(row.Chain) == 0 {
		t.Errorf("%s's custom row did not issue: %s", custom.Host, dnsCertsDescribeRows(certs))
		return
	}
	leaf, err := dnsCertsVerifyChain(row.Chain, stack.Pebble.Roots, []string{custom.Host}, time.Now())
	if err != nil {
		t.Errorf("%s's custom certificate: %v", custom.Host, err)
	} else {
		spkiSha256, err := worldTlsKeySpkiSha256(custom.Publisher.Device)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(dnsCertsSpkiSha256(leaf), spkiSha256) {
			t.Errorf("%s's custom certificate is on a key not %s's", custom.Host, custom.Publisher.Device.Name)
		}
	}
	// one desktop registered the chain, once, whatever the others did meanwhile
	custom.stateLock.Lock()
	issuedBy := []string{}
	newOrders := 0
	for desktopIndex, certIds := range custom.issued {
		for range certIds {
			issuedBy = append(issuedBy, custom.Desktops[desktopIndex].Name)
		}
	}
	for _, count := range custom.newOrders {
		newOrders += count
	}
	custom.stateLock.Unlock()
	if len(issuedBy) != 1 {
		t.Errorf("%s's chain was registered by %v, where one claim wins it once", custom.Host, issuedBy)
	}
	// each receipt one domain and claim, written at the serial of the journal's one commit of its digest at the acme
	// name, every commit of the name a receipt's
	receipts, err := dnsCertsChallengeReceipts(ctx, custom.Network.NetworkId)
	if err != nil {
		t.Fatal(err)
	}
	commits := map[string][]uint64{}
	for _, journalRow := range rows {
		for _, upsert := range journalRow.Upserts {
			if dns.CanonicalName(upsert.Owner) != custom.AcmeName {
				continue
			}
			values, err := dnsCertsRdataValues(upsert)
			if err != nil {
				t.Errorf("serial %d: %v", journalRow.Serial, err)
				continue
			}
			for _, value := range values {
				commits[value] = append(commits[value], journalRow.Serial)
			}
		}
	}
	claims := map[int64]bool{}
	written := map[string]bool{}
	for _, receipt := range receipts {
		if receipt.Domain != custom.Domain {
			continue
		}
		if claims[receipt.ClaimSeq] {
			t.Errorf("%s holds two receipts of claim %d", custom.Domain, receipt.ClaimSeq)
		}
		claims[receipt.ClaimSeq] = true
		if dns.CanonicalName(receipt.Name) != custom.AcmeName {
			t.Errorf("%s's receipt %s names %s, not the domain's acme name", custom.Domain, receipt.ChallengeId, receipt.Name)
		}
		if receipt.State != model.TlsCustomChallengeStateWritten {
			t.Errorf("%s's receipt %s ended %s (%s) after %d dispatches", custom.Domain, receipt.ChallengeId, receipt.State, receipt.Refusal, receipt.DispatchCount)
			continue
		}
		serials := commits[receipt.Value]
		if len(serials) != 1 || serials[0] != receipt.Serial {
			t.Errorf("%s's receipt %s was written at %d, and the journal holds its digest at %v: written more than once, or not at its serial", custom.Domain, receipt.ChallengeId, receipt.Serial, serials)
		}
		written[receipt.Value] = true
	}
	for value, serials := range commits {
		if !written[value] {
			t.Errorf("the journal holds a digest at %s, at %v, that no written receipt of %s's holds", custom.AcmeName, serials, custom.Domain)
		}
	}
	if len(written) == 0 {
		t.Errorf("%s's order issued with no receipt written", custom.Domain)
	}
	// the customer's CA budget: each newOrder a desktop's pass granted is one spend of the account's
	spends, err := dnsCertsCustomSpends(ctx, custom.Network.NetworkId, certsservice.CertsCustomSpendNewOrder)
	if err != nil {
		t.Fatal(err)
	}
	if spends != newOrders {
		t.Errorf("%s's account holds %d newOrder spends, for %d newOrders its desktops' passes granted", custom.Domain, spends, newOrders)
	}
}

// Ends a desktop's burst once its domain's row issued: a step's error, which the phase reads as done.
var dnsCertsStop = errors.New("the domain's row issued")

// The barrier that restarts the certs worker once, while the first challenge command it sees holds its write: the
// restart runs on a goroutine of its own, and the command waits until the run it belongs to has ended, so the drain
// cuts it before the write is sent. Safe for concurrent use.
type dnsCertsBarrierRestart struct {
	worker *dnsCertsWorkerBlock

	stateLock   sync.Mutex
	started     bool
	done        bool
	challengeId server.Id
	err         error
}

func (self *dnsCertsBarrierRestart) barrier(name string, challengeId server.Id) {
	if name != controller.TlsCustomChallengeBarrierWrite {
		return
	}
	runCtx := self.worker.RunContext()
	start := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.started || runCtx == nil {
			return false
		}
		self.started = true
		self.challengeId = challengeId
		return true
	}()
	if !start {
		return
	}
	go func() {
		err := self.worker.Restart()
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.done = true
		self.err = err
	}()
	// held until the run that took the command has ended: the drain has cancelled the command by then
	<-runCtx.Done()
}

func (self *dnsCertsBarrierRestart) Done() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.done
}

func (self *dnsCertsBarrierRestart) Err() error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.err
}

func (self *dnsCertsBarrierRestart) ChallengeId() server.Id {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.challengeId
}

// Waits until want holds for each domain's custom row, read every CertPollInterval.
func dnsCertsAwaitCustomRows(ctx context.Context, domains []*dnsCertsCustomDomain, settings *dnsCertsSettings, want func(custom *dnsCertsCustomDomain, row *sidecar.TlsCertRecord) bool) error {
	for {
		waiting := []string{}
		for _, custom := range domains {
			rows, err := dnsCertsCustomCerts(ctx, custom.Network.NetworkId)
			if err != nil {
				return err
			}
			if !want(custom, custom.row(rows)) {
				waiting = append(waiting, fmt.Sprintf("%s (%s)", custom.Host, dnsCertsDescribeRows(rows)))
			}
		}
		if len(waiting) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the custom rows of %s never were as wanted: %w", strings.Join(waiting, ", "), ctx.Err())
		case <-time.After(settings.CertPollInterval):
		}
	}
}
