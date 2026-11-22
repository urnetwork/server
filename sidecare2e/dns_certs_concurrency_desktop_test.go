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
	"slices"
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

// One custom domain of the delegated phase: its network and the publishers whose devices serve its host, each with a
// custom row of its own for it, the host, the owner it maps to and its acme name, and the desktops that order it,
// each with its orderer and its passes' reports.
type dnsCertsCustomDomain struct {
	Network    *dnsCertsNetwork
	Publishers []*dnsCertsPublisher
	Domain     string
	Host       string
	// <TlsHost>.<label>.<root>, and <acme_token>.acme.<root>, canonical
	Owner    string
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

// The custom rows of the domain's host, its publishers' devices' and any other's, oldest first.
func (self *dnsCertsCustomDomain) rows(rows []*sidecar.TlsCertRecord) []*sidecar.TlsCertRecord {
	hostRows := []*sidecar.TlsCertRecord{}
	for _, row := range rows {
		if row.CustomHost == self.Host {
			hostRows = append(hostRows, row)
		}
	}
	return hostRows
}

// The publisher whose device a row is, nil for none.
func (self *dnsCertsCustomDomain) publisher(row *sidecar.TlsCertRecord) *dnsCertsPublisher {
	for _, publisher := range self.Publishers {
		if publisher.SidecarId == row.SidecarId {
			return publisher
		}
	}
	return nil
}

// Whether every publisher's row of the host issued, and no other row stands for it.
func (self *dnsCertsCustomDomain) issuedAll(rows []*sidecar.TlsCertRecord) bool {
	hostRows := self.rows(rows)
	if len(hostRows) != len(self.Publishers) {
		return false
	}
	for _, row := range hostRows {
		if row.State != sidecar.TlsCertStateCustomIssued {
			return false
		}
	}
	return true
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
	acme := newDnsCertsAcmeClient(stack.Pebble.DirectoryUrl, stack.desktopAcmeHttp, customSettings)
	files := newDnsCertsKeyFiles()
	domains := []*dnsCertsCustomDomain{}
	for index, network := range networks[:min(settings.CustomDomainCount, len(networks))] {
		networkPublishers := []*dnsCertsPublisher{}
		for _, candidate := range publishers {
			if candidate.Network == network {
				networkPublishers = append(networkPublishers, candidate)
			}
		}
		if len(networkPublishers) == 0 {
			t.Fatalf("%s has no publisher to serve its custom host", network.Name)
		}
		domain := fmt.Sprintf(settings.CustomDomainFormat, network.Name)
		custom := &dnsCertsCustomDomain{
			Network:    network,
			Publishers: networkPublishers,
			Domain:     domain,
			Host:       settings.CustomHostLabel + "." + domain,
			Owner:      network.OwnerName(settings.TlsHost),
			issued:     map[int][]sidecar.Id{},
			newOrders:  map[int]int{},
		}
		acmeToken, err := dnsCertsAddCustomDomain(ctx, network.NetworkId, domain, custom.Host, network.Root, custom.Owner)
		if err != nil {
			t.Fatal(err)
		}
		custom.AcmeName = dns.CanonicalName(acmeToken + ".acme." + network.Root)
		// the customer's CNAMEs: the host's to its owner, a weak-mode host (§12.8 step 2), and its _acme-challenge's,
		// one time, to the domain's acme name (step 3)
		stack.Resolver.MapHost(custom.Host, custom.Owner)
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

	// each publisher's pass queues one CSR of its own for its custom host, which its zone now maps (17.2.5): every
	// device listening on the host's owner, so each domain orders as many certificates of its one host's set
	for _, custom := range domains {
		for _, publisher := range custom.Publishers {
			if err := publisher.Component.CertManager().Sync(ctx); err != nil {
				t.Errorf("%s's certificate pass: %v", publisher.Device.Name, err)
			}
		}
	}
	if err := dnsCertsAwaitCustomRows(ctx, domains, settings, func(custom *dnsCertsCustomDomain, rows []*sidecar.TlsCertRecord) bool {
		return len(rows) == len(custom.Publishers)
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
	// and the dns edge restarts under the first command to read ready: that read is held until the edge is back, so
	// the desktop's GET waits on an edge that restarted under it
	edgeHold := stack.Dns.HoldNextReady("the dns edge's restart under a delegated challenge's ready read", func() error {
		return world.Restart(WorldBlockDnsedge)
	})
	defer func() {
		stack.Dns.Disarm(edgeHold)
		edgeHold.Wait(settings.EdgeRestartTimeout)
	}()

	// every desktop's passes at once, until every row of its domain's host is issued
	bursts := []*WorldBurst{}
	for _, custom := range domains {
		for desktopIndex, orderer := range custom.Orderers {
			burst := &WorldBurst{Name: "ordering desktop " + custom.Desktops[desktopIndex].Name, StepCount: settings.CustomPassMaxCount}
			burst.Step = func(ctx context.Context, index int, random *rand.Rand) error {
				rows, err := dnsCertsCustomCerts(ctx, custom.Network.NetworkId)
				if err != nil {
					return err
				}
				if custom.issuedAll(rows) {
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
		world.FailProgress(t, "the delegated orders", fmt.Errorf("%w\n%s", err, stack.DescribeCerts()), bursts)
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
	stack.Dns.Disarm(edgeHold)
	edgeStarted, edgeSerial := edgeHold.Started()
	if err := edgeHold.Wait(settings.EdgeRestartTimeout); err != nil {
		t.Errorf("%v", err)
	}
	if !edgeStarted {
		t.Errorf("no challenge command read ready, so the dns edge never restarted under one")
	}
	t.Logf("the delegated orders: the certs worker restarted at run %d while receipt %s held its write; the dns edge restarted under a ready read of serial %q", stack.Worker.RunCount(), restart.ChallengeId(), edgeSerial)

	rows, err := dnsCertsJournal(ctx, networks[0].Root, 0, settings.JournalPageCount)
	if err != nil {
		t.Fatal(err)
	}
	for _, custom := range domains {
		dnsCertsCheckCustomDomain(t, ctx, stack, custom, rows)
		dnsCertsCheckCustomHostRecords(t, ctx, stack, custom, rows)
	}
}

// What one domain's delegated orders must leave (see the case's header).
func dnsCertsCheckCustomDomain(t testing.TB, ctx context.Context, stack *dnsCertsStack, custom *dnsCertsCustomDomain, rows []dnsservice.DnsJournalRow) {
	t.Helper()
	certs, err := dnsCertsCustomCerts(ctx, custom.Network.NetworkId)
	if err != nil {
		t.Fatal(err)
	}
	hostRows := custom.rows(certs)
	if !custom.issuedAll(certs) {
		t.Errorf("%s's custom rows did not all issue, one each for %d publishers: %s", custom.Host, len(custom.Publishers), dnsCertsDescribeRows(hostRows))
	}
	for _, row := range hostRows {
		publisher := custom.publisher(row)
		if publisher == nil {
			t.Errorf("%s's custom row %s is of a device that publishes no name of the case's", custom.Host, row.CertId)
			continue
		}
		if row.State != sidecar.TlsCertStateCustomIssued || len(row.Chain) == 0 {
			continue
		}
		leaf, err := dnsCertsVerifyChain(row.Chain, stack.Pebble.Roots, []string{custom.Host}, time.Now())
		if err != nil {
			t.Errorf("%s's custom certificate for %s: %v", custom.Host, publisher.Device.Name, err)
			continue
		}
		spkiSha256, err := worldTlsKeySpkiSha256(publisher.Device)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(dnsCertsSpkiSha256(leaf), spkiSha256) {
			t.Errorf("%s's custom certificate %s is on a key not %s's", custom.Host, row.CertId, publisher.Device.Name)
		}
	}
	// one desktop registered each row's chain, once, whatever the others did meanwhile
	custom.stateLock.Lock()
	registeredBy := map[sidecar.Id][]string{}
	newOrders := 0
	for desktopIndex, certIds := range custom.issued {
		for _, certId := range certIds {
			registeredBy[certId] = append(registeredBy[certId], custom.Desktops[desktopIndex].Name)
		}
	}
	for _, count := range custom.newOrders {
		newOrders += count
	}
	custom.stateLock.Unlock()
	for _, row := range hostRows {
		if desktops := registeredBy[row.CertId]; len(desktops) != 1 {
			t.Errorf("%s's row %s was registered by %v, where one claim wins it once", custom.Host, row.CertId, desktops)
		}
		delete(registeredBy, row.CertId)
	}
	for certId, desktops := range registeredBy {
		t.Errorf("%v registered %s, which is no row of %s's", desktops, certId, custom.Host)
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

// The custom host answers through its customer's CNAME what its owner answers at the edge (§12.8 step 2's weak mode):
// the resolver's answer to the host's TXT holds the CNAME to the owner and the owner's newest TXT commit in rows, and
// its answers to the host's A and AAAA the CNAME and the world's lb interfaces of the family, though the owner holds a
// TXT set of its own (§12.7's * edge records through the owner's copy, QD-1190), each RRset signed under the root's
// zone keys, each asked until it answers so within the dns settings' Timeout, since the edge restarted in the phase.
func dnsCertsCheckCustomHostRecords(t testing.TB, ctx context.Context, stack *dnsCertsStack, custom *dnsCertsCustomDomain, rows []dnsservice.DnsJournalRow) {
	t.Helper()
	settings := stack.settings.Dns
	want := []string{}
	for _, row := range rows {
		for _, upsert := range row.Upserts {
			if dns.CanonicalName(upsert.Owner) == custom.Owner && upsert.Type == dns.TypeTXT {
				values, err := dnsCertsRdataValues(upsert)
				if err != nil {
					t.Errorf("serial %d: %v", row.Serial, err)
					continue
				}
				want = values
			}
		}
		for _, deleted := range row.Deletes {
			if dns.CanonicalName(deleted.Owner) == custom.Owner && deleted.Type == dns.TypeTXT {
				want = []string{}
			}
		}
	}
	if len(want) == 0 {
		t.Errorf("%s's owner %s holds no TXT, which the records phase wrote", custom.Host, custom.Owner)
		return
	}
	// the host's rrType through the resolver until its answer holds the CNAME to the owner and holds is true of it
	awaitCnamed := func(rrType uint16, holds func(answer *dns.Msg) bool, wanted string) bool {
		waitCtx, cancel := context.WithTimeout(ctx, settings.Timeout)
		defer cancel()
		for {
			answer, err := stack.Resolver.exchange(waitCtx, custom.Host, rrType)
			cnamed := false
			if err == nil {
				for _, record := range answer.Answer {
					if cname, ok := record.(*dns.CNAME); ok && dns.CanonicalName(cname.Hdr.Name) == dns.CanonicalName(custom.Host) && dns.CanonicalName(cname.Target) == custom.Owner {
						cnamed = true
					}
				}
			}
			if err == nil && cnamed && holds(answer) {
				if err := stack.Keys.Verify(answer, custom.Owner, rrType, time.Now()); err != nil {
					t.Errorf("%s's %s through its customer's CNAME: %v", custom.Host, dns.TypeToString[rrType], err)
					return false
				}
				return true
			}
			select {
			case <-waitCtx.Done():
				t.Errorf("%s's %s through its customer's CNAME never answered %s's %s: %s, %v", custom.Host, dns.TypeToString[rrType], custom.Owner, wanted, dnsCertsDescribeAnswer(answer), err)
				return false
			case <-time.After(settings.RetryInterval):
			}
		}
	}
	set := dnsCertsSetKey{Owner: custom.Owner, Type: dns.TypeTXT}
	held := []string{}
	if awaitCnamed(dns.TypeTXT, func(answer *dns.Msg) bool {
		return slices.Equal(dnsCertsAnswerValues(answer, set), want)
	}, fmt.Sprintf("newest %v", want)) {
		held = append(held, fmt.Sprintf("TXT %v", want))
	}
	for _, rrType := range []uint16{dns.TypeA, dns.TypeAAAA} {
		addresses := dnsCertsLbAddresses(stack.World, rrType)
		if awaitCnamed(rrType, func(answer *dns.Msg) bool {
			return slices.Equal(dnsCertsAnswerAddresses(answer, custom.Owner, rrType), addresses)
		}, fmt.Sprintf("lb addresses %v", addresses)) {
			held = append(held, fmt.Sprintf("%s %v", dns.TypeToString[rrType], addresses))
		}
	}
	t.Logf("%s through its customer's CNAME to %s answers, signed: %s", custom.Host, custom.Owner, strings.Join(held, "; "))
}

// answer's rcode and records, one line, for a log.
func dnsCertsDescribeAnswer(answer *dns.Msg) string {
	if answer == nil {
		return "no answer"
	}
	records := []string{}
	for _, record := range answer.Answer {
		records = append(records, strings.Join(strings.Fields(record.String()), " "))
	}
	return fmt.Sprintf("%s %v", dns.RcodeToString[answer.Rcode], records)
}

// Ends a desktop's burst once every row of its domain's host issued: a step's error, which the phase reads as done.
var dnsCertsStop = errors.New("the domain's rows issued")

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

// Waits until want holds for each domain's custom rows of its host, read every CertPollInterval.
func dnsCertsAwaitCustomRows(ctx context.Context, domains []*dnsCertsCustomDomain, settings *dnsCertsSettings, want func(custom *dnsCertsCustomDomain, rows []*sidecar.TlsCertRecord) bool) error {
	for {
		waiting := []string{}
		for _, custom := range domains {
			rows, err := dnsCertsCustomCerts(ctx, custom.Network.NetworkId)
			if err != nil {
				return err
			}
			if !want(custom, custom.rows(rows)) {
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
