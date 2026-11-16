// T2 for server/cli/smtpedge (SMTP-M9-20): the block scrubs its logs first; its gate loads a network once on a
// miss and answers from memory, its unwired rows answering not loaded; the X-UR-Ref key file lays its keys
// over the settings and names no key when refused; and the block is not ready while a seam is missing, a
// sending address with no FCrDNS included. Time is a fake clock and nothing waits on a sleep.

package main

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/smtpedgeservice"

	"github.com/urnetwork/server/sidecarhost"
)

func TestMainScrubsLogsFirst(t *testing.T) {
	if err := sidecarhost.CheckMainScrubsLogsFirst("main.go"); err != nil {
		t.Fatal(err)
	}
}

// A gate source counting its loads.
type testGateSource struct {
	stateLock sync.Mutex
	loads     map[sidecar.Id]int
	facts     *smtpedgeservice.SmtpGateFacts
}

func (self *testGateSource) Facts(ctx context.Context, networkId sidecar.Id) (*smtpedgeservice.SmtpGateFacts, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.loads[networkId] += 1
	if self.facts == nil {
		return nil, errors.New("not there")
	}
	kept := *self.facts
	return &kept, nil
}

func (self *testGateSource) CommunityLimits(ctx context.Context) (*sidecar.QuotaLimits, error) {
	return &sidecar.QuotaLimits{Tier: "free", Limits: map[string]int64{"smtp_connections_per_day": 5}}, nil
}

func (self *testGateSource) loadCount(networkId sidecar.Id) int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.loads[networkId]
}

func TestHostGateLoadsOnAMissAndAnswersFromMemory(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	source := &testGateSource{loads: map[sidecar.Id]int{}, facts: &smtpedgeservice.SmtpGateFacts{Label: "n1", MailEnableTime: clock.Now().Add(-48 * time.Hour)}}
	gate := newHostGate(ctx, source, clock, defaultHostGateSettings())
	networkId := sidecar.NewId()
	// a miss answers not loaded and asks for one load, however many preambles miss before it lands
	for range 3 {
		if facts, ok := gate.Facts(networkId); ok || facts != nil {
			t.Fatal("a cold gate answered")
		}
	}
	cancel()
	gate.wait()
	if count := source.loadCount(networkId); count != 1 {
		t.Fatalf("%d loads for one network", count)
	}
	facts, ok := gate.Facts(networkId)
	if !ok || facts.Label != "n1" {
		t.Fatalf("facts %+v, %v", facts, ok)
	}
	// a copy, so a caller's change never reaches the memory
	facts.Label = "changed"
	if again, _ := gate.Facts(networkId); again.Label != "n1" {
		t.Fatal("the gate handed out its own facts")
	}
	if limits, ok := gate.CommunityLimits(); !ok || limits.Tier != "free" {
		t.Fatalf("community %+v, %v", limits, ok)
	}
}

// mail_send and §21.5's breaker have no source in this block yet, so both answer not loaded: TRY_LATER.
func TestHostGateUnwiredRowsAnswerNotLoaded(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	gate := newHostGate(ctx, &testGateSource{loads: map[sidecar.Id]int{}}, sidecartest.NewFakeClock(sidecartest.DefaultClockStart()), defaultHostGateSettings())
	defer gate.wait()
	defer cancel()
	if decision := gate.MaySendMail(sidecar.NewId(), sidecar.NewId(), time.Now()); decision.Allowed || decision.Reason != sidecar.AclReasonNoState {
		t.Fatalf("mail_send %+v", decision)
	}
	if state, ok := gate.Breaker(); ok || state != nil {
		t.Fatal("a breaker with no source answered")
	}
}

func TestXUrRefKeyFileLaysItsKeysOverTheSettings(t *testing.T) {
	settings := smtpedgeservice.DefaultSmtpEdgeSettings()
	settings.Root = "root.example"
	settings.ReportHostName = "rpt.report.example"
	file := "issue_key_id: 2\nkeys:\n  - {id: 1, key: " + strings.Repeat("QUFB", 11) + "}\n  - {id: 2, key: " + strings.Repeat("QkJC", 11) + "}\n"
	if err := applyXUrRefKeys(settings, []byte(file)); err != nil {
		t.Fatal(err)
	}
	if len(settings.XUrRefKeys) != 2 || settings.XUrRefIssueKeyId != 2 || settings.XUrRefKeys[1].KeyId != 2 || len(settings.XUrRefKeys[1].Key) != 33 {
		t.Fatalf("keys %d, issue %d", len(settings.XUrRefKeys), settings.XUrRefIssueKeyId)
	}
	secret := "c2VjcmV0LXZhbHVlLW5vdC1iYXNlNjQ!!"
	for _, bad := range []string{
		"issue_key_id: 3\nkeys:\n  - {id: 1, key: " + strings.Repeat("QUFB", 11) + "}\n",
		"issue_key_id: 1\nkeys:\n  - {id: 1, key: " + secret + "}\n",
		"issue_key_id: 1\nkeys:\n  - {id: 300, key: " + strings.Repeat("QUFB", 11) + "}\n",
		"issue_key_id: 1\nkeys:\n  - {id: 1, key: QUFB}\n",
	} {
		err := applyXUrRefKeys(settings, []byte(bad))
		if err == nil {
			t.Fatalf("%q was taken", bad)
		}
		if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "QUFB") {
			t.Fatalf("a refusal named a key: %v", err)
		}
	}
}

// The block is not ready while a seam is missing, and says which; a sending address with no FCrDNS leaves the
// engine not ready either.
func TestBlockNotReadyWithoutItsSeams(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	host := sidecartest.NewFakeHost(ctx, clock, nil, nil, nil)
	config, err := smtpedgeservice.LoadSmtpEdgeConfig([]byte(`
root: root.example
report_host: rpt.report.example
helo_domain: out.report.example
sending_addresses:
  - {address: 192.0.2.10, helo: o1.out.report.example}
`), 0)
	if err != nil {
		t.Fatal(err)
	}
	log := &testLog{}
	block, err := newSmtpEdgeHost(ctx, defaultHostSettings(config), &hostDeps{
		Quota: host.Quota,
		Redis: host.Redis,
		Gates: host.Gates,
		Clock: clock,
		Log:   log,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer block.Close()
	if block.Ready() {
		t.Fatal("a block with no store, gate, resolver, verifier or client is ready")
	}
	missing := block.Missing()
	for _, want := range []string{"the store", "the send gate", "MX resolver", "PTR resolver", "the proof verifier", "the platform client", "sidecar"} {
		if !slices.ContainsFunc(missing, func(text string) bool { return strings.Contains(text, want) }) {
			t.Fatalf("missing %q does not name %q", missing, want)
		}
	}
	if lines := log.Lines(); len(lines) != 1 || !strings.Contains(lines[0], "not ready without") {
		t.Fatalf("the block logged %q", lines)
	}
}

// Every line logged.
type testLog struct {
	stateLock sync.Mutex
	lines     []string
}

func (self *testLog) add(line string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.lines = append(self.lines, line)
}

func (self *testLog) Lines() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.lines)
}

func (self *testLog) Info(args ...any)                    { self.add("info") }
func (self *testLog) Infof(format string, args ...any)    { self.add(format) }
func (self *testLog) Warningf(format string, args ...any) { self.add(format) }
func (self *testLog) Errorf(format string, args ...any)   { self.add(format) }
func (self *testLog) V(level int32) sidecar.Verbose       { return testVerbose{} }

type testVerbose struct{}

func (testVerbose) Enabled() bool                    { return false }
func (testVerbose) Info(args ...any)                 {}
func (testVerbose) Infof(format string, args ...any) {}

// config/local's smtpedge.yml loads as the block reads it, its names documentation names and its addresses
// RFC 5737's, and its root and report host are mxedge.yml's platform ones, which the platform block's intake
// requires of the same file.
func TestLocalSmtpEdgeConfigLoads(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "config", "local", "smtpedge.yml"))
	if err != nil {
		t.Fatal(err)
	}
	config, err := smtpedgeservice.LoadSmtpEdgeConfig(data, 0)
	if err != nil {
		t.Fatal(err)
	}
	documentation := netip.MustParsePrefix("192.0.2.0/24")
	for _, address := range config.Settings.SendingAddresses {
		if !documentation.Contains(address.Address) || !strings.HasSuffix(address.HeloName, ".example") {
			t.Fatalf("a sending address that is not documentation's: %+v", address)
		}
	}
	mxedge, err := os.ReadFile(filepath.Join("..", "..", "..", "config", "local", "mxedge.yml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{"platform_root: " + config.Settings.Root, "platform_report_host: " + config.Settings.ReportHostName} {
		if !strings.Contains(string(mxedge), line) {
			t.Fatalf("mxedge.yml has no %q", line)
		}
	}
}
