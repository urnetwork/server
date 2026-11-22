//go:build sidecar_e2e && sidecar_pending && sidecar_localstack

// The certificate side of the dns and certificates case (dns_certs_concurrency_test.go), on the world's blocks
// (world_test.go), which stands in §13.8's certs worker and its CA (World.IssueTlsCertificateRow): here the real chain
// runs instead.
//
//	dns       the world's dns API and edge, with internal bearers for certs and the site monitor beside the edge's in
//	          the API's verify file (internal-verify.yml), as production's API holds each caller class's digest; the
//	          world's own verify file names the edge alone
//	canary    a stand-in for §12.6's site monitor (dnsCertsCanary): each round writes canary.<root> through POST
//	          /dns/internal/canary on the monitor's bearer, validates the value the edge serves against the world's
//	          KSK, as the monitor's Unbound does through each NS, and reports the verdict, so GET /dns/internal/ready
//	          answers ready once its other checks pass; the real monitor's validator is unbound-host, which no test host
//	          runs and nothing may be fetched to run
//	resolver  a stand-in for the internet's recursive resolvers (dnsCertsResolver): every name under the world's root
//	          is asked of the dns edge as it runs, the edge's restarts included, and a custom domain's one-time
//	          _acme-challenge CNAME, which the customer's own DNS holds in production, is answered with the target's
//	          records beside it; Pebble's validation authority asks it, as a CA's resolvers ask the internet
//	ca        sidecartest's Pebble in process, refusing §22.9's 5% of nonces as the local stack's Pebble does
//	api       api's certs install (controller.NewTlsCertsInstall) over a gate cache of the world's, as api's process
//	          builds it when sidecar.yml's certs switch is on, on certs.yml's root, so §13.15's routes serve devices'
//	          orders and an ordering desktop's custom routes
//	worker    the certs worker as a block of its own (dnsCertsWorkerBlock): taskworker's certs install
//	          (work.NewTlsCertChainsInstall) on the harness's certs.yml, its LE account file and its internal-certs.yml,
//	          and a task worker of taskworker's queue claiming only certs' targets, each run built afresh on the same
//	          stores, so chaos's restart is a deploy's: the drain cuts what a pass has under way, and the next pass
//	          takes it up
//
// The chains run on taskworker's own cadences, which no install sets: an order pass every 30 s and a renewal scan
// every hour. The case kicks a chain's next run (task.KickTasks) where it waits on one, as an operator's `bringyourctl
// task kick` does, rather than wait the cadence out.

package sidecare2e

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/certsservice"
	"github.com/urnetwork/sidecar/dnsservice"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/sidecarhost"
	"github.com/urnetwork/server/task"
	"github.com/urnetwork/server/taskworker/work"
)

// The world and everything the certificate side adds to it.
type dnsCertsStack struct {
	World    *World
	settings *dnsCertsSettings
	// the internal bearers the dns API's verify file holds beside the edge's
	certsBearer   string
	monitorBearer string
	// what the edge's signatures verify under, once the dns blocks stand
	Keys     *dnsCertsZoneKeys
	Resolver *dnsCertsResolver
	Canary   *dnsCertsCanary
	Pebble   *sidecartest.Pebble
	Worker   *dnsCertsWorkerBlock
	// certs.yml as the api and the worker read it
	certsYaml []byte
}

// A fresh internal bearer: 256 random bits, base64url, as each caller's vault file holds one.
func dnsCertsBearer() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// The world with the dns blocks the certificate side needs: every block but the dns API and the edge is the world's
// own start; then the API's verify file names the certs and monitor bearers beside the edge's, and both dns blocks
// start through the world's own composition (World.newBlock), the edge after the API it feeds from, so chaos restarts
// them as any block. Then the resolver, the canary's first round, Pebble, api's certs install and the certs worker.
func newDnsCertsStack(t testing.TB, settings *dnsCertsSettings) *dnsCertsStack {
	worldSettings := DefaultWorldSettings()
	hostsSettings := defaultE2eHostsSettings()
	hostsSettings.CaseTimeout = settings.CaseTimeout
	worldSettings.Hosts = hostsSettings
	worldSettings.NetworkCount = settings.NetworkCount
	worldSettings.Blocks = slices.DeleteFunc(WorldBlockNames(), func(name string) bool {
		return name == WorldBlockDns || name == WorldBlockDnsedge
	})
	world := NewWorld(t, worldSettings)
	self := &dnsCertsStack{World: world, settings: settings}
	var err error
	if self.certsBearer, err = dnsCertsBearer(); err != nil {
		t.Fatal(err)
	}
	if self.monitorBearer, err = dnsCertsBearer(); err != nil {
		t.Fatal(err)
	}
	verifyLines := []string{}
	for _, caller := range []struct {
		name   string
		class  dnsservice.DnsCallerClass
		bearer string
	}{
		{name: "dnsedge-1", class: dnsservice.DnsCallerEdge, bearer: world.credentials.dnsBearer},
		{name: "certs", class: dnsservice.DnsCallerCerts, bearer: self.certsBearer},
		{name: "monitor", class: dnsservice.DnsCallerMonitor, bearer: self.monitorBearer},
	} {
		digest := sha256.Sum256([]byte(caller.bearer))
		verifyLines = append(verifyLines, fmt.Sprintf("%s: {class: %s, sha256: [%s]}", caller.name, caller.class, hex.EncodeToString(digest[:])))
	}
	if world.credentials.dnsAuth, err = dnsservice.NewDnsInternalVerifier([]byte(strings.Join(verifyLines, "\n") + "\n")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{WorldBlockDns, WorldBlockDnsedge} {
		block, err := world.newBlock(name)
		if err != nil {
			t.Fatalf("the %s block: %v", name, err)
		}
		world.blocks = append(world.blocks, block)
		if err := block.start(); err != nil {
			t.Fatalf("the %s block: %v", name, err)
		}
		if err := world.awaitStartStatus(block); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := world.StepContext(t.Context(), settings.PhaseTimeout)
	defer cancel()
	if self.Keys, err = newDnsCertsZoneKeys(ctx, world, settings.Dns); err != nil {
		t.Fatal(err)
	}
	if self.Resolver, err = startDnsCertsResolver(world, self.Keys.root, settings.Dns); err != nil {
		t.Fatal(err)
	}
	world.T3().OnClose(self.Resolver.Close)
	self.Canary = &dnsCertsCanary{world: world, root: self.Keys.root, bearer: self.monitorBearer, keys: self.Keys, settings: settings}
	if err := self.Canary.Round(ctx); err != nil {
		t.Fatalf("the canary's first round: %v", err)
	}
	canaryCtx, cancelCanary := context.WithCancel(world.T3().Context())
	var canaryLoop sync.WaitGroup
	canaryLoop.Go(func() {
		self.Canary.Run(canaryCtx)
	})
	world.T3().OnClose(func() {
		cancelCanary()
		canaryLoop.Wait()
	})

	pebbleSettings := sidecartest.DefaultPebbleSettings()
	pebbleSettings.NonceRejectPercent = settings.PebbleNonceRejectPercent
	pebbleSettings.DnsAddress = self.Resolver.TcpAddress()
	self.Pebble = sidecartest.StartPebble(t, pebbleSettings)
	self.certsYaml = []byte(strings.Join([]string{
		"root: " + strings.TrimSuffix(self.Keys.root, "."),
		"dns_url: " + world.T3().Api().ServiceOrigin("dns"),
		"ca_order: [" + string(certsservice.CertsCaLetsEncrypt) + "]",
		"ephemeral_ca_order: [" + string(certsservice.CertsCaLetsEncrypt) + "]",
		fmt.Sprintf("order_retry_after_s: %d", settings.OrderRetryAfterSeconds),
		"cas:",
		"  " + string(certsservice.CertsCaLetsEncrypt) + ":",
		"    directory_url: " + self.Pebble.DirectoryUrl,
		"    profile: " + sidecartest.PebbleProfileTlsServer,
		"    account_file: " + settings.AccountFileName,
		"",
	}, "\n"))
	if err := self.installApiCerts(); err != nil {
		t.Fatalf("api's certs install: %v", err)
	}
	if self.Worker, err = newDnsCertsWorkerBlock(self); err != nil {
		t.Fatal(err)
	}
	if err := self.Worker.Start(); err != nil {
		t.Fatalf("the certs worker: %v", err)
	}
	world.T3().OnClose(func() {
		if err := self.Worker.Stop(); err != nil {
			t.Errorf("%v", err)
		}
	})
	return self
}

// api's certs install as its process builds it with sidecar.yml's certs switch on, on certs.yml: the engine with no
// DNS or ACME client, the Postgres store, the certs authenticator under the world's svc- verifier, and the subject
// over a gate cache of the world's, which the world's syncs wait on. Removed when the world closes.
func (self *dnsCertsStack) installApiCerts() error {
	world := self.World
	cache, closeCache, err := world.T3().AddGate("api-certs")
	if err != nil {
		return err
	}
	ctx := world.T3().Context()
	abuse := model.NewAbuseGate(ctx, model.NewAbuseHardDenySnapshot(ctx, nil), nil)
	host := sidecarhost.NewServiceHost(ctx, &sidecarhost.ServiceHostOptions{
		Blobs:         world.T3().Host().Blobs,
		Abuse:         abuse,
		ServiceTokens: world.T3().ServiceTokens(),
	})
	certsYaml := self.certsYaml
	install := controller.NewTlsCertsInstall(&controller.TlsCertsInstallDeps{
		SwitchOn: func() bool { return true },
		ReadConfigFile: func(name string) ([]byte, error) {
			if name != controller.TlsCertsConfigFileName {
				return nil, fmt.Errorf("the case's config holds no %s", name)
			}
			return certsYaml, nil
		},
		Gate: func(ctx context.Context) (*controller.TlsCertsGate, error) {
			return &controller.TlsCertsGate{
				Host:   host,
				States: cache,
				Abuse:  abuse,
				Close: func() {
					closeCache()
					abuse.Close()
				},
			}, nil
		},
	}, nil)
	logged := []string{}
	install.Testing_SetLog(func(format string, arguments ...any) {
		logged = append(logged, fmt.Sprintf(format, arguments...))
	})
	if !install.Sync(ctx) {
		install.Close()
		return fmt.Errorf("not installed: %s", strings.Join(logged, "; "))
	}
	world.T3().OnClose(install.Close)
	return nil
}

// One POST of body to the dns API's internal path on bearer, through the world's lb, its answer decoded into answer
// when one is given: an error naming the status for anything but a 2xx.
func dnsCertsInternalPost(ctx context.Context, world *World, bearer string, path string, body any, answer any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, world.T3().Api().ServiceOrigin("dns")+path, bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+bearer)
	request.Header.Set("Content-Type", "application/json")
	response, err := world.T3().Api().Transport().RoundTrip(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		return err
	}
	if response.StatusCode/100 != 2 {
		return fmt.Errorf("POST %s answered %d: %s", path, response.StatusCode, strings.TrimSpace(string(data)))
	}
	if answer != nil {
		return json.Unmarshal(data, answer)
	}
	return nil
}

// A stand-in for §12.6's site monitor (see the file header). Safe for concurrent use.
type dnsCertsCanary struct {
	world    *World
	root     string
	bearer   string
	keys     *dnsCertsZoneKeys
	settings *dnsCertsSettings

	stateLock sync.Mutex
	rounds    int
	okRounds  int
	lastErr   error
}

// One round: the canary written, read back from the edge until it serves the value signed, and the verdict reported
// at the serial the edge then served. The verdict is ok only when the value validated; a round that cannot report
// is the error, and the API's verdict stays the last one reported.
func (self *dnsCertsCanary) Round(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, self.settings.CanaryTimeout)
	defer cancel()
	value := fmt.Sprintf("%d e2e-canary", time.Now().UnixMilli())
	written := &dnsservice.DnsCanaryAnswer{}
	if err := dnsCertsRetry(ctx, self.settings.Dns.RetryInterval, func() error {
		return dnsCertsInternalPost(ctx, self.world, self.bearer, dnsservice.DnsCanaryPath, &dnsservice.DnsCanaryRequest{Root: self.root, Value: value}, written)
	}); err != nil {
		return self.end(fmt.Errorf("the canary write: %w", err))
	}
	owner := dnsservice.DnsCanaryOwner(self.root)
	ok := true
	answer, err := self.world.AwaitDnsAnswer(ctx, owner, dns.TypeTXT, func(answer *dns.Msg) bool {
		return dnsAnswerHoldsSignedTxt(answer, owner, value)
	}, self.settings.Dns)
	if err == nil {
		err = self.keys.Verify(answer, owner, dns.TypeTXT, time.Now())
	}
	if err != nil {
		ok = false
	}
	serial := written.Serial
	if soa, soaErr := dnsCertsSoaSerial(ctx, self.world, self.keys, self.settings.Dns); soaErr == nil {
		serial = soa
	}
	report := &dnsservice.DnsCanaryReport{Root: self.root, Source: dnsservice.DnsCanarySourceSite, Serial: serial, Ok: ok}
	if reportErr := dnsCertsRetry(ctx, self.settings.Dns.RetryInterval, func() error {
		return dnsCertsInternalPost(ctx, self.world, self.bearer, dnsservice.DnsCanaryReportPath, report, nil)
	}); reportErr != nil {
		return self.end(fmt.Errorf("the canary report: %w", reportErr))
	}
	if !ok {
		return self.end(fmt.Errorf("the canary did not validate at the edge, reported failing: %w", err))
	}
	return self.end(nil)
}

func (self *dnsCertsCanary) end(err error) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.rounds += 1
	if err == nil {
		self.okRounds += 1
	}
	self.lastErr = err
	return err
}

// The rounds so far, those that reported ok, and the last round's error.
func (self *dnsCertsCanary) Rounds() (int, int, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.rounds, self.okRounds, self.lastErr
}

// A round every CanaryInterval until ctx ends, as the monitor's minute does.
func (self *dnsCertsCanary) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(self.settings.CanaryInterval):
		}
		self.Round(ctx)
	}
}

// Calls call until it answers nil or ctx ends, every interval: its last error then.
func dnsCertsRetry(ctx context.Context, interval time.Duration, call func() error) error {
	for {
		err := call()
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w (%w)", err, ctx.Err())
		case <-time.After(interval):
		}
	}
}

// A stand-in for the internet's recursive resolvers (see the file header), on loopback TCP and UDP. Safe for
// concurrent use.
type dnsCertsResolver struct {
	world    *World
	root     string
	settings *WorldDnsSettings
	tcp      *dns.Server
	udp      *dns.Server

	stateLock sync.Mutex
	// a custom host's _acme-challenge owner, canonical, to the target its CNAME names
	delegations map[string]string
	// the questions asked, by name, canonical
	asked map[string]int
}

// A resolver for root, answered from world's dns edge.
func startDnsCertsResolver(world *World, root string, settings *WorldDnsSettings) (*dnsCertsResolver, error) {
	self := &dnsCertsResolver{world: world, root: root, settings: settings, delegations: map[string]string{}, asked: map[string]int{}}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	packetConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		listener.Close()
		return nil, err
	}
	handler := dns.HandlerFunc(self.serve)
	self.tcp = &dns.Server{Listener: listener, Net: "tcp", Handler: handler}
	self.udp = &dns.Server{PacketConn: packetConn, Net: "udp", Handler: handler}
	go self.tcp.ActivateAndServe()
	go self.udp.ActivateAndServe()
	return self, nil
}

// The TCP address, which Pebble's validation authority asks.
func (self *dnsCertsResolver) TcpAddress() string {
	return self.tcp.Listener.Addr().String()
}

// Stops both servers.
func (self *dnsCertsResolver) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), self.settings.Timeout)
	defer cancel()
	self.tcp.ShutdownContext(ctx)
	self.udp.ShutdownContext(ctx)
}

// Answers _acme-challenge.<host> with a CNAME to target, as a customer's zone does once it delegated the host's
// DNS-01 to its domain's acme name (§12.8 step 3).
func (self *dnsCertsResolver) Delegate(host string, target string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.delegations[dns.CanonicalName(model.DnsChallengeLabel+"."+host)] = dns.CanonicalName(target)
}

// How many times name was asked.
func (self *dnsCertsResolver) Asked(name string) int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.asked[dns.CanonicalName(name)]
}

// name's question of qtype as the edge answers it now: nil with an error while the edge does not answer.
func (self *dnsCertsResolver) forward(ctx context.Context, name string, qtype uint16) (*dns.Msg, error) {
	return self.world.DnsQuery(ctx, name, qtype, self.settings)
}

func (self *dnsCertsResolver) serve(writer dns.ResponseWriter, request *dns.Msg) {
	reply := new(dns.Msg)
	reply.SetReply(request)
	reply.RecursionAvailable = true
	ctx, cancel := context.WithTimeout(context.Background(), self.settings.ExchangeTimeout)
	defer cancel()
	for _, question := range request.Question {
		name := dns.CanonicalName(question.Name)
		target, delegated := func() (string, bool) {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.asked[name] += 1
			target, ok := self.delegations[name]
			return target, ok
		}()
		asked := name
		if delegated {
			reply.Answer = append(reply.Answer, &dns.CNAME{
				Hdr:    dns.RR_Header{Name: question.Name, Rrtype: dns.TypeCNAME, Class: dns.ClassINET, Ttl: 60},
				Target: target,
			})
			if question.Qtype == dns.TypeCNAME {
				continue
			}
			asked = target
		}
		if asked != self.root && !strings.HasSuffix(asked, "."+self.root) {
			reply.Rcode = dns.RcodeNameError
			continue
		}
		answer, err := self.forward(ctx, asked, question.Qtype)
		if err != nil {
			reply.Rcode = dns.RcodeServerFailure
			continue
		}
		reply.Rcode = answer.Rcode
		reply.Answer = append(reply.Answer, answer.Answer...)
		reply.Ns = append(reply.Ns, answer.Ns...)
	}
	writer.WriteMsg(reply)
}

// sidecar.TlsCustomResolver's CNAME lookup, as the desktop's system resolver answers it: the target, canonical, or
// an error when name holds none.
func (self *dnsCertsResolver) LookupCname(ctx context.Context, name string) (string, error) {
	answer, err := self.exchange(ctx, name, dns.TypeCNAME)
	if err != nil {
		return "", err
	}
	for _, record := range answer.Answer {
		if cname, ok := record.(*dns.CNAME); ok && dns.CanonicalName(cname.Hdr.Name) == dns.CanonicalName(name) {
			return dns.CanonicalName(cname.Target), nil
		}
	}
	return "", fmt.Errorf("%s holds no CNAME", name)
}

// sidecar.TlsCustomResolver's TXT lookup: every TXT value the answer holds, each record's strings joined.
func (self *dnsCertsResolver) LookupTxt(ctx context.Context, name string) ([]string, error) {
	answer, err := self.exchange(ctx, name, dns.TypeTXT)
	if err != nil {
		return nil, err
	}
	values := []string{}
	for _, record := range answer.Answer {
		if txt, ok := record.(*dns.TXT); ok {
			values = append(values, strings.Join(txt.Txt, ""))
		}
	}
	return values, nil
}

// One question to the resolver over UDP, as a client's stub asks.
func (self *dnsCertsResolver) exchange(ctx context.Context, name string, qtype uint16) (*dns.Msg, error) {
	question := new(dns.Msg)
	question.SetQuestion(dns.Fqdn(name), qtype)
	question.SetEdns0(self.settings.UdpBufferByteCount, false)
	client := &dns.Client{Net: "udp", Timeout: self.settings.ExchangeTimeout}
	answer, _, err := client.ExchangeContext(ctx, question, self.udp.PacketConn.LocalAddr().String())
	if err != nil {
		return nil, err
	}
	if answer.Rcode != dns.RcodeSuccess {
		return nil, fmt.Errorf("%s %s answered %s", name, dns.TypeToString[qtype], dns.RcodeToString[answer.Rcode])
	}
	return answer, nil
}

// The certs worker as a block of its own (see the file header): one run at a time, each built afresh on the same
// stores. Safe for concurrent use.
type dnsCertsWorkerBlock struct {
	stack *dnsCertsStack
	// what taskworker reads from vault, the harness's: the LE account file and internal-certs.yml
	accountFile    []byte
	credentialFile []byte

	stateLock sync.Mutex
	current   *dnsCertsWorkerRun
	runCount  int
}

// One run of the certs worker.
type dnsCertsWorkerRun struct {
	cancel  context.CancelFunc
	install *server.SidecarSwitchInstall
	worker  *task.TaskWorker
	loops   sync.WaitGroup
}

// The block with a fresh P-256 LE account key in its account file and the certs bearer in its credential file.
func newDnsCertsWorkerBlock(stack *dnsCertsStack) (*dnsCertsWorkerBlock, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	keyPem := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	accountFile := "ca: " + string(certsservice.CertsCaLetsEncrypt) + "\n" +
		"key_create_time: " + time.Now().UTC().Format(time.RFC3339) + "\n" +
		"private_key: |\n"
	for _, line := range strings.Split(strings.TrimSpace(string(keyPem)), "\n") {
		accountFile += "  " + line + "\n"
	}
	return &dnsCertsWorkerBlock{
		stack:          stack,
		accountFile:    []byte(accountFile),
		credentialFile: []byte("current: " + stack.certsBearer + "\n"),
	}, nil
}

// How many runs have started.
func (self *dnsCertsWorkerBlock) RunCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.runCount
}

// Starts a run: certs' install, which must install, and the worker's loops claiming certs' targets alone. The first
// run schedules certs' chains, as taskworker's start does (taskworker.initTaskSchedule).
func (self *dnsCertsWorkerBlock) Start() error {
	settings := self.stack.settings
	world := self.stack.World
	ctx, cancel := context.WithCancel(context.Background())
	certsYaml := self.stack.certsYaml
	accountFile := self.accountFile
	credentialFile := self.credentialFile
	install := work.NewTlsCertChainsInstall(&work.TlsCertChainsInstallDeps{
		SwitchOn: func() bool { return true },
		ReadConfigFile: func(name string) ([]byte, error) {
			if name != "certs.yml" {
				return nil, fmt.Errorf("the case's config holds no %s", name)
			}
			return certsYaml, nil
		},
		ReadSecretFile: func(name string) ([]byte, error) {
			switch name {
			case settings.AccountFileName:
				return accountFile, nil
			case server.CertsDnsCredentialFileName:
				return credentialFile, nil
			}
			return nil, fmt.Errorf("the case's vault holds no %s", name)
		},
		DnsTransport:   world.T3().Api().Transport(),
		AcmeHttpClient: self.stack.Pebble.HttpClient,
		CaaResolver:    dnsCertsNoCaaResolver{},
	}, nil)
	logged := []string{}
	var loggedLock sync.Mutex
	install.Testing_SetLog(func(format string, arguments ...any) {
		loggedLock.Lock()
		defer loggedLock.Unlock()
		logged = append(logged, fmt.Sprintf(format, arguments...))
	})
	if !install.Sync(ctx) {
		cancel()
		install.Close()
		loggedLock.Lock()
		defer loggedLock.Unlock()
		return fmt.Errorf("certs' install did not install: %s", strings.Join(logged, "; "))
	}
	workerSettings := task.DefaultTaskWorkerSettings()
	workerSettings.ClaimRegisteredTargetsOnly = true
	workerSettings.PollTimeout = settings.WorkerPollTimeout
	workerSettings.DrainFinishTimeout = settings.WorkerDrainTimeout
	worker := task.NewTaskWorker(ctx, workerSettings)
	worker.AddTargets(
		task.NewTaskTargetWithPost(work.TlsCertOrders, work.TlsCertOrdersPost),
		task.NewTaskTargetWithPost(work.TlsCertRenewals, work.TlsCertRenewalsPost),
		task.NewTaskTargetWithPost(work.TlsCertRevocations, work.TlsCertRevocationsPost),
		task.NewTaskTargetWithPost(work.TlsCustomSweep, work.TlsCustomSweepPost),
		task.NewTaskTargetWithPost(controller.TlsCustomChallengeWork, controller.TlsCustomChallengeWorkPost),
	)
	run := &dnsCertsWorkerRun{cancel: cancel, install: install, worker: worker}
	first := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.current = run
		self.runCount += 1
		return self.runCount == 1
	}()
	if first {
		var scheduleErr error
		server.HandleError(func() {
			server.Tx(ctx, func(tx server.PgTx) {
				clientSession := session.NewLocalClientSession(ctx, "", nil)
				work.ScheduleTlsCertOrders(clientSession, tx)
				work.ScheduleTlsCertRenewals(clientSession, tx)
				work.ScheduleTlsCertRevocations(clientSession, tx)
				work.ScheduleTlsCustomSweep(clientSession, tx)
			})
		}, func(err error) {
			scheduleErr = err
		})
		if scheduleErr != nil {
			self.Stop()
			return fmt.Errorf("certs' chains' first runs: %w", scheduleErr)
		}
	}
	for range settings.WorkerLoopCount {
		run.loops.Go(worker.Run)
	}
	return nil
}

// Ends the current run as a deploy's SIGTERM does: the worker drains, cutting a pass still running past
// WorkerDrainTimeout, which leaves its orders under way for the next run, then certs' install is removed. Nothing
// while stopped.
func (self *dnsCertsWorkerBlock) Stop() error {
	run := func() *dnsCertsWorkerRun {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		run := self.current
		self.current = nil
		return run
	}()
	if run == nil {
		return nil
	}
	run.worker.Drain()
	run.cancel()
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		run.loops.Wait()
	}()
	select {
	case <-stopped:
	case <-time.After(self.stack.World.settings.StopTimeout):
		return fmt.Errorf("the certs worker's loops did not end within %s of its stop", self.stack.World.settings.StopTimeout)
	}
	run.install.Close()
	run.worker.Close()
	return nil
}

// Stops the current run and starts another on the same stores: chaos's restart.
func (self *dnsCertsWorkerBlock) Restart() error {
	if err := self.Stop(); err != nil {
		return err
	}
	return self.Start()
}

// Moves the next run of certs' chain named runOnceKey to now (task.KickTasks), as `bringyourctl task kick` does.
func dnsCertsKick(ctx context.Context, runOnceKey string) (kicked int64, returnErr error) {
	server.HandleError(func() {
		kicked = task.KickTasks(ctx, runOnceKey)
	}, func(err error) {
		returnErr = fmt.Errorf("the kick of %s: %w", runOnceKey, err)
	})
	return
}

// The CAA resolver certs' install takes, which nothing in the case asks: the CAA checks of custom hosts run on a chain
// the worker does not claim, and a resolver that answers nothing keeps any lookup off the test host's own.
type dnsCertsNoCaaResolver struct{}

var errDnsCertsNoCaa = errors.New("the case resolves no CAA")

func (dnsCertsNoCaaResolver) LookupCaa(ctx context.Context, name string) (*sidecar.TlsCaaAnswer, error) {
	return nil, errDnsCertsNoCaa
}

func (dnsCertsNoCaaResolver) AuthoritativeCaa(ctx context.Context, name string, nameservers []string) (map[string][]string, error) {
	return nil, errDnsCertsNoCaa
}

// The leaf of chain once it verifies against roots, its other certificates as intermediates, for every one of names
// at now: an error naming the name that does not.
func dnsCertsVerifyChain(chain [][]byte, roots *x509.CertPool, names []string, now time.Time) (*x509.Certificate, error) {
	if len(chain) == 0 {
		return nil, errors.New("an empty chain")
	}
	leaf, err := x509.ParseCertificate(chain[0])
	if err != nil {
		return nil, err
	}
	intermediates := x509.NewCertPool()
	for _, der := range chain[1:] {
		certificate, err := x509.ParseCertificate(der)
		if err != nil {
			return nil, err
		}
		intermediates.AddCert(certificate)
	}
	for _, name := range names {
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediates, DNSName: name, CurrentTime: now}); err != nil {
			return nil, fmt.Errorf("the chain does not verify for %s: %w", name, err)
		}
	}
	return leaf, nil
}

// The SHA-256 of leaf's SubjectPublicKeyInfo.
func dnsCertsSpkiSha256(leaf *x509.Certificate) []byte {
	digest := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
	return digest[:]
}

// One visit to serverName at the world's lb interface's 443, as World.VisitTls makes it (the lb's PROXY header naming
// the visitor), but holding the listener's chain to roots for serverName, as a browser does, rather than to a pinned
// key: the leaf it was shown and the answer line to request.
func dnsCertsVisitVerified(ctx context.Context, world *World, serverName string, roots *x509.CertPool, request []byte, settings *WorldTlsSettings) (*x509.Certificate, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, settings.VisitTimeout)
	defer cancel()
	conn, err := world.DialLb(ctx, settings.VisitorAddress, netip.AddrPortFrom(world.settings.LbAddress, worldTlsPort))
	if err != nil {
		return nil, nil, err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		conn.SetDeadline(deadline)
	}
	client := tls.Client(conn, &tls.Config{ServerName: serverName, RootCAs: roots, MinVersion: tls.VersionTLS12})
	if err := client.HandshakeContext(ctx); err != nil {
		return nil, nil, fmt.Errorf("the handshake: %w", err)
	}
	leaf := client.ConnectionState().PeerCertificates[0]
	if _, err := client.Write(append(bytes.Clone(request), '\n')); err != nil {
		return leaf, nil, err
	}
	answer, err := bufio.NewReaderSize(client, settings.RequestMaxByteCount).ReadSlice('\n')
	if err != nil {
		return leaf, nil, fmt.Errorf("the answer: %w", err)
	}
	return leaf, bytes.TrimSuffix(answer, []byte("\n")), nil
}

// dnsCertsVisitVerified until its answer is want, every RetryInterval within Timeout: a registration reaches sockets at
// its verdict and tlsedge at its Find, with no hook a test reads. The leaf of the visit answered.
func dnsCertsAwaitVerifiedVisit(ctx context.Context, world *World, serverName string, roots *x509.CertPool, request []byte, want []byte, settings *WorldTlsSettings) (*x509.Certificate, error) {
	ctx, cancel := context.WithTimeout(ctx, settings.Timeout)
	defer cancel()
	tries := 0
	for {
		tries += 1
		leaf, answer, err := dnsCertsVisitVerified(ctx, world, serverName, roots, request, settings)
		if err == nil && bytes.Equal(answer, want) {
			return leaf, nil
		}
		if err == nil {
			err = fmt.Errorf("answered %q, not %q", answer, want)
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("%s through tlsedge, %d visits: %w (%w)", serverName, tries, err, ctx.Err())
		case <-time.After(settings.RetryInterval):
		}
	}
}
