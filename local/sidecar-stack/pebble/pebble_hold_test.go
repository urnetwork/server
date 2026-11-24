// The local Pebble's hold on a request to an authorization whose challenge Pebble is validating, which keeps Pebble's
// own lock leak from wedging it until restart (pebble_hold.go), and the bounds on the hold's and the stop's waits.
// Each Pebble runs in the test's process on loopback. A test holds the validation authority at a line of its own log,
// which reaches the operator's log the test passes, so a challenge stays under validation for as long as the test
// needs with no lookup to time out (Pebble's resolver client reads for 2 s at most). The order a race needs is forced
// that way and with the hold's own signal, never a sleep, and every wait has its test's deadline.

package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/letsencrypt/pebble/v2/wfe"
	"github.com/mholt/acmez/v3/acme"
	"github.com/miekg/dns"
)

// A local Pebble served in the test's process on loopback.
type testPebble struct {
	server        *pebbleServer
	directoryUrl  string
	managementUrl string
	// trusts the ACME listener's certificate
	httpClient *http.Client
	// stops Pebble and returns what its serve returned; the test's end calls it too
	stop func() error
}

// Starts a local Pebble on loopback with settings, logging to logWriter with no prefix or flags, and stops it at the
// test's end, which fails the test if the stop does. Pebble reads its knobs from the environment, which
// newPebbleServer sets, so the test's end puts them back.
func testStartPebble(t *testing.T, settings *PebbleSettings, logWriter io.Writer) *testPebble {
	t.Helper()
	for _, name := range []string{"PEBBLE_VA_NOSLEEP", "PEBBLE_WFE_NONCEREJECT", "PEBBLE_AUTHZREUSE", "PEBBLE_VA_ALWAYS_VALID"} {
		t.Setenv(name, os.Getenv(name))
	}
	server, err := newPebbleServer(settings, log.New(logWriter, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	acmeListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	managementListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		acmeListener.Close()
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() {
		served <- server.serve(ctx, acmeListener, managementListener, settings.ShutdownTimeout)
	}()
	roots := x509.NewCertPool()
	roots.AddCert(server.certificate.Leaf)
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}}
	pebble := &testPebble{
		server:        server,
		directoryUrl:  "https://" + acmeListener.Addr().String() + wfe.DirectoryPath,
		managementUrl: "http://" + managementListener.Addr().String(),
		httpClient:    &http.Client{Transport: transport},
		stop: sync.OnceValue(func() error {
			cancel()
			defer transport.CloseIdleConnections()
			select {
			case err := <-served:
				return err
			case <-time.After(settings.ShutdownTimeout + 20*time.Second):
				return errors.New("Pebble's serve had not returned long after its stop")
			}
		}),
	}
	t.Cleanup(func() {
		if err := pebble.stop(); err != nil {
			t.Error(err)
		}
	})
	return pebble
}

// An account on pebble, and a pending order for name with its one authorization and that authorization's dns-01
// challenge.
func testPebbleChallenge(t *testing.T, ctx context.Context, pebble *testPebble, name string) (*acme.Client, acme.Account, acme.Order, acme.Authorization, acme.Challenge) {
	t.Helper()
	client := &acme.Client{Directory: pebble.directoryUrl, HTTPClient: pebble.httpClient}
	accountKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	account, err := client.NewAccount(ctx, acme.Account{PrivateKey: accountKey, TermsOfServiceAgreed: true})
	if err != nil {
		t.Fatal(err)
	}
	order, err := client.NewOrder(ctx, account, acme.Order{Identifiers: []acme.Identifier{{Type: "dns", Value: name}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(order.Authorizations) != 1 {
		t.Fatalf("authorizations %v", order.Authorizations)
	}
	authz, err := client.GetAuthorization(ctx, account, order.Authorizations[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, challenge := range authz.Challenges {
		if challenge.Type == acme.ChallengeTypeDNS01 {
			return client, account, order, authz, challenge
		}
	}
	t.Fatalf("Pebble offered no dns-01 challenge: %+v", authz.Challenges)
	return nil, acme.Account{}, acme.Order{}, acme.Authorization{}, acme.Challenge{}
}

// The operator's log of a Pebble under test. It keeps every line, and a line with the text a test chose waits in
// Write until the test lets it go. The validation authority's lines reach it through a logger of their own
// (newPebbleServer), so a held line of the authority's holds the authority alone, and the front end, which logs each
// request, goes on. Safe for concurrent use.
type testPebbleLog struct {
	stateLock sync.Mutex
	lines     []string
	// a line containing it is held; empty holds none
	holdText string
	// closed at the first line held
	holdArrived chan struct{}
	// closed to let held lines go
	holdRelease chan struct{}
}

// Holds each line containing text from now on until release, which the test's end calls too; arrived closes at the
// first line held. release may be called more than once.
func (self *testPebbleLog) hold(t *testing.T, text string) (arrived <-chan struct{}, release func()) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	holdArrived := make(chan struct{})
	holdRelease := make(chan struct{})
	self.holdText = text
	self.holdArrived = holdArrived
	self.holdRelease = holdRelease
	release = sync.OnceFunc(func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		close(holdRelease)
		if self.holdRelease == holdRelease {
			self.holdText = ""
			self.holdArrived = nil
			self.holdRelease = nil
		}
	})
	t.Cleanup(release)
	return holdArrived, release
}

func (self *testPebbleLog) Write(line []byte) (int, error) {
	release := func() chan struct{} {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.lines = append(self.lines, string(line))
		if self.holdText == "" || !strings.Contains(string(line), self.holdText) {
			return nil
		}
		select {
		case <-self.holdArrived:
		default:
			close(self.holdArrived)
		}
		return self.holdRelease
	}()
	if release != nil {
		<-release
	}
	return len(line), nil
}

func (self *testPebbleLog) text() string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return strings.Join(self.lines, "")
}

// What the validation authority logs in each of its checks under -always-valid, before it hands in its result: held,
// it keeps the challenge under validation past its first lock, as a lookup under way does.
func testPebbleAlwaysValidCheck(challenge acme.Challenge) string {
	return "Skipping real validation of challenge " + path.Base(challenge.URL)
}

// A deactivation that arrives while the authorization's challenge is under validation, as acmez's cleanup sends one
// when a later authorization's solver fails, waits until the validation authority has finished; Pebble then
// deactivates the authorization and still takes orders. Without the hold, Pebble deactivates it while the authority's
// check is held, the authority then waits forever with the authorization's lock held, and the next newOrder never
// answers: this test's deadline fails it in seconds, and the stop at its end fails naming Pebble's wedged goroutines.
func TestPebbleHoldsADeactivationUntilItsChallengeIsValidated(t *testing.T) {
	settings := DefaultPebbleSettings()
	settings.NonceRejectPercent = 0
	settings.AlwaysValid = true
	pebbleLog := &testPebbleLog{}
	pebble := testStartPebble(t, settings, pebbleLog)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	client, account, _, authz, challenge := testPebbleChallenge(t, ctx, pebble, "held.pebble.example")

	arrived, release := pebbleLog.hold(t, testPebbleAlwaysValidCheck(challenge))
	if _, err := client.InitiateChallenge(ctx, account, challenge); err != nil {
		t.Fatal(err)
	}
	select {
	case <-arrived:
	case <-ctx.Done():
		t.Fatal("Pebble's validation authority never checked the challenge")
	}

	deactivated := make(chan error, 1)
	go func() {
		_, err := client.DeactivateAuthorization(ctx, account, authz.Location)
		deactivated <- err
	}()
	held := false
	answered := false
	var deactivateErr error
	select {
	case <-pebble.server.hold.authorizationHeld:
		held = true
	case deactivateErr = <-deactivated:
		answered = true
	case <-ctx.Done():
		t.Fatal("the deactivation was neither held nor answered")
	}
	release()
	if !held {
		t.Errorf("Pebble deactivated the authorization while its challenge was under validation: %v", deactivateErr)
	}
	if !answered {
		select {
		case deactivateErr = <-deactivated:
		case <-ctx.Done():
			t.Fatal("the held deactivation never reached Pebble once the validation ended")
		}
	}
	if deactivateErr != nil {
		t.Fatal(deactivateErr)
	}

	// a newOrder reads every authorization's lock, the deactivated one's among them
	if _, err := client.NewOrder(ctx, account, acme.Order{Identifiers: []acme.Identifier{{Type: "dns", Value: "next.pebble.example"}}}); err != nil {
		t.Fatalf("Pebble took no order after the deactivation: %v", err)
	}
	ended, err := client.GetAuthorization(ctx, account, authz.Location)
	if err != nil {
		t.Fatal(err)
	}
	if ended.Status != acme.StatusDeactivated {
		t.Fatalf("the authorization ended %s", ended.Status)
	}
}

// The resolver a test's Pebble asks over TCP, as Pebble's validation authority asks one: TXT records by name, and
// NOERROR with no answer for anything else. Stopped at the test's end. Safe for concurrent use.
type testPebbleDns struct {
	address string

	stateLock        sync.Mutex
	nameTxts         map[string][]string
	nameLookupCounts map[string]int
}

func testStartPebbleDns(t *testing.T) *testPebbleDns {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	self := &testPebbleDns{
		address:          listener.Addr().String(),
		nameTxts:         map[string][]string{},
		nameLookupCounts: map[string]int{},
	}
	started := make(chan struct{})
	server := &dns.Server{
		Listener: listener,
		Net:      "tcp",
		Handler:  dns.HandlerFunc(self.serve),
		NotifyStartedFunc: func() {
			close(started)
		},
	}
	served := make(chan error, 1)
	go func() {
		served <- server.ActivateAndServe()
	}()
	// a server that has not started takes no shutdown, and would serve on past the test
	select {
	case <-started:
	case err := <-served:
		t.Fatalf("the test's resolver: %v", err)
	case <-time.After(20 * time.Second):
		t.Fatal("the test's resolver had not started")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.ShutdownContext(ctx); err != nil {
			t.Errorf("the test's resolver had not stopped: %v", err)
		}
	})
	return self
}

func (self *testPebbleDns) serve(writer dns.ResponseWriter, request *dns.Msg) {
	reply := new(dns.Msg)
	reply.SetReply(request)
	reply.Authoritative = true
	for _, question := range request.Question {
		name := strings.ToLower(strings.TrimSuffix(question.Name, "."))
		txts := func() []string {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.nameLookupCounts[name] += 1
			return append([]string{}, self.nameTxts[name]...)
		}()
		if question.Qtype != dns.TypeTXT {
			continue
		}
		for _, txt := range txts {
			reply.Answer = append(reply.Answer, &dns.TXT{
				Hdr: dns.RR_Header{Name: question.Name, Rrtype: dns.TypeTXT, Class: dns.ClassINET, Ttl: 60},
				Txt: []string{txt},
			})
		}
	}
	writer.WriteMsg(reply)
}

func (self *testPebbleDns) set(name string, txts ...string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.nameTxts[strings.ToLower(name)] = txts
}

func (self *testPebbleDns) lookupCount(name string) int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.nameLookupCounts[strings.ToLower(name)]
}

// A poll that arrives while the authorization's DNS-01 challenge is under validation through Pebble's resolver waits
// for the validation and then reads the authorization valid, and the order finalizes to a chain under the root the
// management port hands out: the hold stands in every order's way, not only a deactivation's, and lets it through.
// The authority is held before its lookups, so the resolver answers each as it comes.
func TestPebbleHoldsAPollUntilItsDns01ChallengeIsValidated(t *testing.T) {
	resolver := testStartPebbleDns(t)
	settings := DefaultPebbleSettings()
	settings.NonceRejectPercent = 0
	settings.DnsServer = resolver.address
	pebbleLog := &testPebbleLog{}
	pebble := testStartPebble(t, settings, pebbleLog)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	name := "dns01.pebble.example"
	client, account, order, authz, challenge := testPebbleChallenge(t, ctx, pebble, name)
	resolver.set("_acme-challenge."+name, challenge.DNS01KeyAuthorization())

	// what the authority logs as it takes the challenge up, before its lookups
	arrived, release := pebbleLog.hold(t, "Starting 3 validations.")
	if _, err := client.InitiateChallenge(ctx, account, challenge); err != nil {
		t.Fatal(err)
	}
	select {
	case <-arrived:
	case <-ctx.Done():
		t.Fatal("Pebble's validation authority never took the challenge up")
	}
	type pollResult struct {
		authz acme.Authorization
		err   error
	}
	polled := make(chan pollResult, 1)
	go func() {
		authz, err := client.GetAuthorization(ctx, account, authz.Location)
		polled <- pollResult{authz: authz, err: err}
	}()
	select {
	case <-pebble.server.hold.authorizationHeld:
	case result := <-polled:
		t.Fatalf("the poll was answered while its challenge was under validation: %s %v", result.authz.Status, result.err)
	case <-ctx.Done():
		t.Fatal("the poll was neither held nor answered")
	}
	release()
	var result pollResult
	select {
	case result = <-polled:
	case <-ctx.Done():
		t.Fatal("the held poll never reached Pebble once the validation ended")
	}
	if result.err != nil {
		t.Fatal(result.err)
	}
	if result.authz.Status != acme.StatusValid {
		t.Fatalf("the authorization read %s once its challenge was validated", result.authz.Status)
	}
	if resolver.lookupCount("_acme-challenge."+name) == 0 {
		t.Fatal("Pebble validated the challenge without asking the resolver")
	}

	certificateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{DNSNames: []string{name}}, certificateKey)
	if err != nil {
		t.Fatal(err)
	}
	order, err = client.FinalizeOrder(ctx, account, order, csr)
	if err != nil {
		t.Fatal(err)
	}
	chains, err := client.GetCertificateChain(ctx, account, order.Certificate)
	if err != nil {
		t.Fatal(err)
	}
	if len(chains) == 0 {
		t.Fatal("Pebble issued no chain")
	}
	chainCertificates := []*x509.Certificate{}
	for rest := chains[0].ChainPEM; ; {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		certificate, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		chainCertificates = append(chainCertificates, certificate)
	}
	if len(chainCertificates) == 0 {
		t.Fatal("the chain holds no certificate")
	}
	rootRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, pebble.managementUrl+"/roots/0", nil)
	if err != nil {
		t.Fatal(err)
	}
	rootResponse, err := http.DefaultClient.Do(rootRequest)
	if err != nil {
		t.Fatal(err)
	}
	rootPem, err := io.ReadAll(rootResponse.Body)
	rootResponse.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(rootPem) {
		t.Fatalf("the management port's root is no PEM: %q", rootPem)
	}
	intermediates := x509.NewCertPool()
	for _, certificate := range chainCertificates[1:] {
		intermediates.AddCert(certificate)
	}
	if _, err := chainCertificates[0].Verify(x509.VerifyOptions{DNSName: name, Roots: roots, Intermediates: intermediates}); err != nil {
		t.Fatalf("the issued chain: %v", err)
	}
}

// A request on an authorization still held at ValidationWaitTimeout is answered 503 naming the challenge, and logged,
// rather than reach a Pebble that would wedge on it. The authority's check is held past the bound, so no hold can end
// before it; acmez sends a POST answered 5xx three times, and each is held and answered alike. The deactivation never
// reached Pebble: once let go, the validation sets the authorization valid, and Pebble still takes orders.
func TestPebbleAnswersAHeldRequest503AtItsTimeout(t *testing.T) {
	settings := DefaultPebbleSettings()
	settings.NonceRejectPercent = 0
	settings.AlwaysValid = true
	settings.ValidationWaitTimeout = 50 * time.Millisecond
	pebbleLog := &testPebbleLog{}
	pebble := testStartPebble(t, settings, pebbleLog)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	client, account, _, authz, challenge := testPebbleChallenge(t, ctx, pebble, "timeout.pebble.example")
	challengeId := path.Base(challenge.URL)

	arrived, release := pebbleLog.hold(t, testPebbleAlwaysValidCheck(challenge))
	if _, err := client.InitiateChallenge(ctx, account, challenge); err != nil {
		t.Fatal(err)
	}
	select {
	case <-arrived:
	case <-ctx.Done():
		t.Fatal("Pebble's validation authority never checked the challenge")
	}
	_, err := client.DeactivateAuthorization(ctx, account, authz.Location)
	var problem acme.Problem
	if !errors.As(err, &problem) || problem.Status != http.StatusServiceUnavailable || !strings.Contains(problem.Detail, challengeId) {
		t.Fatalf("the held deactivation answered %v", err)
	}
	select {
	case <-pebble.server.hold.authorizationHeld:
	default:
		t.Fatal("the deactivation was answered with no hold")
	}
	if !strings.Contains(pebbleLog.text(), "had not finished challenge "+challengeId) {
		t.Fatalf("the operator's log has no word of the answer:\n%s", pebbleLog.text())
	}

	// watched before the authority goes on, so its finish is seen
	done, unwatch := pebble.server.hold.validations.watch(challengeId)
	defer unwatch()
	release()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("the validation never finished once let go")
	}
	ended, err := client.GetAuthorization(ctx, account, authz.Location)
	if err != nil {
		t.Fatal(err)
	}
	if ended.Status != acme.StatusValid {
		t.Fatalf("the authorization ended %s", ended.Status)
	}
	if _, err := client.NewOrder(ctx, account, acme.Order{Identifiers: []acme.Identifier{{Type: "dns", Value: "next.pebble.example"}}}); err != nil {
		t.Fatalf("Pebble took no order after the answer: %v", err)
	}
}

// A stop answers a held request 503 rather than wait on it, so it ends well inside ShutdownTimeout and fails nothing,
// with the validation authority still held. Were the request left held, the stop would wait out ShutdownTimeout and
// fail naming it.
func TestPebbleStopAnswersAHeldRequest503(t *testing.T) {
	settings := DefaultPebbleSettings()
	settings.NonceRejectPercent = 0
	settings.AlwaysValid = true
	pebbleLog := &testPebbleLog{}
	pebble := testStartPebble(t, settings, pebbleLog)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	client, account, _, authz, challenge := testPebbleChallenge(t, ctx, pebble, "stop.pebble.example")

	arrived, release := pebbleLog.hold(t, testPebbleAlwaysValidCheck(challenge))
	defer release()
	if _, err := client.InitiateChallenge(ctx, account, challenge); err != nil {
		t.Fatal(err)
	}
	select {
	case <-arrived:
	case <-ctx.Done():
		t.Fatal("Pebble's validation authority never checked the challenge")
	}
	deactivated := make(chan error, 1)
	go func() {
		_, err := client.DeactivateAuthorization(ctx, account, authz.Location)
		deactivated <- err
	}()
	select {
	case <-pebble.server.hold.authorizationHeld:
	case err := <-deactivated:
		t.Fatalf("the deactivation was answered with no hold: %v", err)
	case <-ctx.Done():
		t.Fatal("the deactivation was neither held nor answered")
	}

	if err := pebble.stop(); err != nil {
		t.Fatalf("the stop: %v", err)
	}
	select {
	case err := <-deactivated:
		// answered 503, and its retries find the listener closed
		if err == nil {
			t.Fatal("the held deactivation reached Pebble across the stop")
		}
	case <-ctx.Done():
		t.Fatal("the held deactivation never ended after the stop")
	}
}

// The stop waits ShutdownTimeout for Pebble's requests and then fails, closing their connections, rather than wait on
// a request that never ends, as every request on a wedged Pebble does.
func TestPebbleStopFailsOnARequestThatNeverEnds(t *testing.T) {
	entered := make(chan struct{})
	enteredOnce := &sync.Once{}
	leave := make(chan struct{})
	defer close(leave)
	certificate, err := pebbleListenerCertificate([]string{"127.0.0.1"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	server := &pebbleServer{
		acme: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			enteredOnce.Do(func() {
				close(entered)
			})
			// a wedged Pebble's handler, which no closed connection or stop lets go
			<-leave
		}),
		management:  http.NewServeMux(),
		certificate: certificate,
		stopping:    make(chan struct{}),
	}
	acmeListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	managementListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		acmeListener.Close()
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan error, 1)
	go func() {
		served <- server.serve(ctx, acmeListener, managementListener, 50*time.Millisecond)
	}()

	roots := x509.NewCertPool()
	roots.AddCert(certificate.Leaf)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}}}
	requested := make(chan error, 1)
	go func() {
		response, err := client.Get("https://" + acmeListener.Addr().String() + wfe.DirectoryPath)
		if err == nil {
			response.Body.Close()
		}
		requested <- err
	}()
	select {
	case <-entered:
	case <-time.After(20 * time.Second):
		t.Fatal("the request never reached the server")
	}

	cancel()
	select {
	case err := <-served:
		if err == nil || !strings.Contains(err.Error(), "had not ended") {
			t.Fatalf("the stop returned %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the stop never returned")
	}
	select {
	case err := <-requested:
		if err == nil {
			t.Fatal("a request on a closed connection was answered")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the closed connection never failed its request")
	}
}

// The hold keeps a challenge only while a request watches it, however many the authority finishes, so a Pebble that
// runs for a whole campaign keeps no more than its requests in flight: a finish lets every watch of its challenge go
// and keeps nothing, a finish no one watches keeps nothing, a watch ended before the finish keeps nothing, and a line
// that is no finish lets no watch go.
func TestPebbleValidationsKeepAChallengeOnlyWhileARequestWatchesIt(t *testing.T) {
	validations := newPebbleValidations(log.New(io.Discard, "", 0))
	closed := func(done <-chan struct{}) bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	}

	fmt.Fprintf(validations, "authz authz-0 set VALID by completed challenge challenge-0\n")

	done1, unwatch1 := validations.watch("challenge-1")
	done2, unwatch2 := validations.watch("challenge-1")
	fmt.Fprintf(validations, "authz authz-1 set INVALID by completed challenge challenge-1\n")
	if !closed(done1) || !closed(done2) {
		t.Fatal("a finish left a watch of its challenge waiting")
	}
	unwatch1()
	unwatch2()

	done3, unwatch3 := validations.watch("challenge-3")
	unwatch3()
	unwatch3()
	fmt.Fprintf(validations, "authz authz-3 set VALID by completed challenge challenge-3\n")
	if closed(done3) {
		t.Fatal("a finish reached a watch that had ended")
	}

	done4, unwatch4 := validations.watch("challenge-4")
	fmt.Fprintf(validations, "PEBBLE_VA_ALWAYS_VALID is enabled. Skipping real validation of challenge challenge-4\n")
	fmt.Fprintf(validations, "order order-4 set INVALID by invalid authz authz-4\n")
	if closed(done4) {
		t.Fatal("a line that is no finish let a watch go")
	}
	fmt.Fprintf(validations, "authz authz-4 set VALID by completed challenge challenge-4\n")
	if !closed(done4) {
		t.Fatal("a finish left a watch of its challenge waiting")
	}
	unwatch4()

	if count := len(validations.challengeIdWatches); count != 0 {
		t.Fatalf("the hold kept %d challenges with no request watching", count)
	}
}
