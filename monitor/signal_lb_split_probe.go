package monitor

// Signal lb-split-probe implements SIGNALS.md §31.1 (OPS-M5-05; §13.4's probes, W1, D-OPS-9, R-OPS-11,
// FS-50, owner decision 143): each lb interface running §13.4's SNI split is probed each minute, on each
// address family, as a visitor would reach it on 443, in two halves:
//
//   - a hello for probe.edge.<root>, for each root of services.yml's lb.tls_split, which passes only on
//     tlsedge's answer, one plaintext fatal unrecognized_name alert before any ServerHello (D-TLS-3): a
//     ServerHello is the http branch answering a user name, which is what a broken split looks like;
//   - the monitor's verified handshake for a platform name the http servers serve, which passes only on a
//     certificate that verifies for that name: tlsedge's alert, or any other answer, is a platform name
//     reaching the wrong branch.
//
// An answer that fails either half pages §19.8's rota at any hour from M5, under lb-split-probe-failing:
// that interface's prior lb version. A hello that could not run, for a timeout or a connection that never
// opened, is lb-split-probe-unobservable, which holds the interface's page open and never clears it,
// since the monitor's own path fails the same way.
//
// The probe.edge.<root> hello cannot test the custom-host map, since the *.<root> wildcard routes it
// whatever the map holds (§13.4): the dns edges' route probes do that, and a map-stale interface pages
// from M6 (OPS-M6-01).
//
// Interfaces come from services.yml's lb inventory (server.ParseServicesLbInventory), narrowed to those
// monitor.yml's lb_split_probe block says run the split, since during W1's rollout an interface flips one
// lb deploy at a time. An interface is named by its block, <host>-<interface> as sidecar.yml's drained and
// the lb's Route 53 sets name it, and never by an address: no target, finding or page holds one (I-13).

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// SIGNALS.md §31.1's signal.
func NewLbSplitProbeSignal() Signal {
	return &signalAdapter{
		number: "31.1", key: "lb-split-probe", name: "lb interfaces' SNI split",
		probe: lbSplitProbe{cadenceValue: DefaultLbSplitProbeSettings().Cadence},
	}
}

const lbSplitProbeId = "lb/split-probe"

// SIGNALS.md §31.1's classes.
const (
	lbSplitProbeClassFailing      = "lb-split-probe-failing"
	lbSplitProbeClassUnobservable = "lb-split-probe-unobservable"
)

// The signal's own target, for findings about its settings rather than an interface.
const lbSplitProbeTarget = "lb-split-probe"

// Wire and naming facts. RFC 8446 §5.1 and §6: a record's header, the alert and handshake record types,
// the record version's major byte, an alert's two bytes, a fatal alert's level and unrecognized_name; a
// whole alert record is the header and its two bytes. §12.5 reserves the probe label under every root,
// and the split owns tcp 443.
const (
	lbSplitTlsRecordHeaderByteCount = 5
	lbSplitTlsRecordTypeAlert       = 21
	lbSplitTlsRecordTypeHandshake   = 22
	lbSplitTlsRecordVersionMajor    = 3
	lbSplitTlsAlertByteCount        = 2
	lbSplitTlsAlertLevelFatal       = 2
	lbSplitTlsAlertUnrecognizedName = 112
	lbSplitTlsAlertRecordByteCount  = lbSplitTlsRecordHeaderByteCount + lbSplitTlsAlertByteCount
	lbSplitProbeEdgeLabel           = "probe.edge"
	lbSplitProbePort                = 443
)

// The address families, as monitor.yml's block names them.
const (
	lbSplitFamilyIpv4 = "ipv4"
	lbSplitFamilyIpv6 = "ipv6"
)

// The probe's settings: monitor.yml's lb_split_probe block beside services.yml (config.go).
type LbSplitProbeSettings struct {
	// The split's roots, from services.yml's lb.tls_split, lowercase with no final dot: every interface is
	// sent a hello for probe.edge.<root> of each.
	Roots []string
	// The interfaces running the split, from services.yml's inventory as the block names them, by block.
	Interfaces []LbSplitProbeInterface
	// A platform name the http servers serve, whose verified handshake checks the platform branch: the
	// block's platform_name, else services.yml's manager hostname. Empty leaves that half blind.
	PlatformName string
	// The families probed, lbSplitFamilyIpv4 and lbSplitFamilyIpv6; the block's families, else both.
	Families []string
	// How often every interface is probed. The registry reads the default, since a signal's cadence is
	// fixed when it registers.
	Cadence time.Duration
	// Consecutive runs failing an interface before it pages: 2 rides out one odd minute, and a timeout
	// never counts toward it.
	PageSustain int
	// Consecutive runs blind on an interface before its visibility alert reaches Slack. A blind run holds
	// the interface's page open from the first, whatever this says.
	BlindSustain int
	// Interfaces probed at once. Non-positive probes one at a time.
	Concurrency int
	// Why the block or services.yml did not read. Set, the probe reports itself blind rather than off,
	// since a split probe asked for and not running must hold incidents open. Never holds an address.
	ConfigError string
}

// One lb interface running the split.
type LbSplitProbeInterface struct {
	// <host>-<interface>, the host without the domain: how sidecar.yml's drained and the lb's Route 53
	// sets name it, and the only name a finding carries.
	Block string
	// The zero value where the interface has no address of the family.
	Ipv4 netip.Addr
	Ipv6 netip.Addr
}

// The production defaults. Roots, Interfaces and PlatformName are the deployment's.
func DefaultLbSplitProbeSettings() *LbSplitProbeSettings {
	return &LbSplitProbeSettings{
		Families:     []string{lbSplitFamilyIpv4, lbSplitFamilyIpv6},
		Cadence:      time.Minute,
		PageSustain:  2,
		BlindSustain: 2,
		Concurrency:  8,
	}
}

// A deep copy.
func cloneLbSplitProbeSettings(settings *LbSplitProbeSettings) *LbSplitProbeSettings {
	if settings == nil {
		return nil
	}
	clone := *settings
	clone.Roots = slices.Clone(settings.Roots)
	clone.Interfaces = slices.Clone(settings.Interfaces)
	clone.Families = slices.Clone(settings.Families)
	return &clone
}

// The interface's block, as drained names it: the inventory host, which has no domain, and the
// interface.
func lbSplitProbeBlock(inventoryHost string, interfaceName string) string {
	return inventoryHost + "-" + interfaceName
}

// The name a hello for root asks for: probe.edge.<root>, without the final dot SNI leaves off.
func lbSplitProbeServerName(root string) string {
	return lbSplitProbeEdgeLabel + "." + strings.TrimSuffix(root, ".")
}

// The first record crypto/tls writes as a client asking for serverName, offering no ALPN: its ClientHello,
// whole, the hello a visitor's client sends and the one the dns edges' probe sends. It is made over an
// in-memory pipe, and a probe never finishes a handshake, so its random and key share cost nothing.
func lbSplitProbeClientHello(serverName string) ([]byte, error) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()
	handshakeDone := make(chan struct{})
	go func() {
		defer close(handshakeDone)
		// the far side closes after one record, so this always fails, and only the hello matters
		tls.Client(clientConn, &tls.Config{ServerName: serverName, MinVersion: tls.VersionTLS12}).Handshake()
	}()
	header := make([]byte, lbSplitTlsRecordHeaderByteCount)
	if _, err := io.ReadFull(serverConn, header); err != nil {
		return nil, err
	}
	if header[0] != lbSplitTlsRecordTypeHandshake {
		return nil, errors.New("crypto/tls opened with no handshake record")
	}
	body := make([]byte, binary.BigEndian.Uint16(header[3:5]))
	if _, err := io.ReadFull(serverConn, body); err != nil {
		return nil, err
	}
	serverConn.Close()
	<-handshakeDone
	return append(header, body...), nil
}

// What one half of a probe made of an interface.
type lbSplitVerdict int

const (
	lbSplitPassed lbSplitVerdict = iota
	// the interface answered, and wrongly
	lbSplitFailed
	// the probe could not run: no evidence either way
	lbSplitBlind
)

// One half's verdict and a fixed phrase saying why. The phrase never holds an address or a peer's text.
type lbSplitOutcome struct {
	verdict lbSplitVerdict
	reason  string
}

// Whether err is a probe running out of time, the monitor's path as much as the interface's.
func lbSplitTimedOut(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// The operation of the network error err ends in: "dial", "read", "write", or crypto/tls's "remote error"
// for an alert the peer sent; empty when err is none of them.
func lbSplitNetOp(err error) string {
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return opErr.Op
	}
	return ""
}

// What the start of the answer to a probe.edge.<root> hello says: response holds the bytes read toward a
// whole alert record, zero past what arrived, and err why fewer arrived. Bytes that arrived outrank the
// error, since they are the interface's answer.
func lbSplitUserAnswer(response []byte, err error) lbSplitOutcome {
	// the buffer is zeroed past what arrived, and no record type is zero, so a set first byte arrived
	if 0 < len(response) && response[0] != 0 {
		switch response[0] {
		case lbSplitTlsRecordTypeHandshake:
			return lbSplitOutcome{verdict: lbSplitFailed, reason: "the http branch answered (a ServerHello)"}
		case lbSplitTlsRecordTypeAlert:
			if err != nil || len(response) < lbSplitTlsAlertRecordByteCount {
				return lbSplitOutcome{verdict: lbSplitFailed, reason: "a short alert record, then the connection ended"}
			}
			if response[1] != lbSplitTlsRecordVersionMajor ||
				binary.BigEndian.Uint16(response[3:5]) != lbSplitTlsAlertByteCount ||
				response[5] != lbSplitTlsAlertLevelFatal {
				return lbSplitOutcome{verdict: lbSplitFailed, reason: "a malformed alert record"}
			}
			if response[6] != lbSplitTlsAlertUnrecognizedName {
				return lbSplitOutcome{verdict: lbSplitFailed, reason: fmt.Sprintf("alert %d, not unrecognized_name", response[6])}
			}
			return lbSplitOutcome{verdict: lbSplitPassed}
		}
		return lbSplitOutcome{verdict: lbSplitFailed, reason: "an answer that is no TLS record"}
	}
	switch op := lbSplitNetOp(err); {
	case err == nil:
		return lbSplitOutcome{verdict: lbSplitFailed, reason: "an empty answer"}
	case lbSplitTimedOut(err):
		return lbSplitOutcome{verdict: lbSplitBlind, reason: "no answer in time"}
	case op == "dial":
		return lbSplitOutcome{verdict: lbSplitBlind, reason: "the connection did not open"}
	case op == "write":
		return lbSplitOutcome{verdict: lbSplitBlind, reason: "the hello could not be sent"}
	case errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF):
		return lbSplitOutcome{verdict: lbSplitFailed, reason: "closed after the hello with no answer"}
	case op == "read":
		return lbSplitOutcome{verdict: lbSplitFailed, reason: "reset after the hello with no answer"}
	}
	return lbSplitOutcome{verdict: lbSplitBlind, reason: "the probe failed (" + classifyObservationError(err) + ")"}
}

// What the verified handshake for the platform name made of its answer.
func lbSplitPlatformAnswer(observation TLSCertificateObservation, err error) lbSplitOutcome {
	if err == nil {
		if observation.VerifyError != nil {
			return lbSplitOutcome{verdict: lbSplitFailed, reason: "a certificate that does not verify for the platform name"}
		}
		return lbSplitOutcome{verdict: lbSplitPassed}
	}
	if lbSplitTimedOut(err) {
		return lbSplitOutcome{verdict: lbSplitBlind, reason: "no answer in time"}
	}
	var recordErr tls.RecordHeaderError
	switch op := lbSplitNetOp(err); {
	case op == "dial":
		return lbSplitOutcome{verdict: lbSplitBlind, reason: "the connection did not open"}
	case op == "write":
		return lbSplitOutcome{verdict: lbSplitBlind, reason: "the hello could not be sent"}
	case op == "remote error":
		return lbSplitOutcome{verdict: lbSplitFailed, reason: "an alert answered the platform name (tlsedge's, if the split sent it there)"}
	case errors.As(err, &recordErr):
		return lbSplitOutcome{verdict: lbSplitFailed, reason: "an answer to the platform name that is no TLS record"}
	case errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF):
		return lbSplitOutcome{verdict: lbSplitFailed, reason: "closed during the platform name's handshake"}
	case op == "read":
		return lbSplitOutcome{verdict: lbSplitFailed, reason: "reset during the platform name's handshake"}
	}
	return lbSplitOutcome{verdict: lbSplitBlind, reason: "the handshake failed (" + classifyObservationError(err) + ")"}
}

// One family of one interface, and what its probe found.
type lbSplitTask struct {
	block  string
	family string
	// the interface's address and port: dialed, and kept out of every finding
	address  string
	network  string
	outcomes []string
	failed   bool
	blind    bool
}

type lbSplitProbe struct {
	cadenceValue time.Duration
}

func (lbSplitProbe) id() string                  { return lbSplitProbeId }
func (lbSplitProbe) tier() string                { return tierPage }
func (self lbSplitProbe) cadence() time.Duration { return self.cadenceValue }

func (lbSplitProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	settings := env.cfg.lbSplitProbe
	if settings == nil {
		return []finding{{
			probeId: lbSplitProbeId, tier: tierWarn, class: lbSplitProbeClassUnobservable, target: lbSplitProbeTarget,
			sustain: 1, configuredOff: true,
			symptom: "monitor.yml has no lb_split_probe block, so no interface's split is probed",
		}}, nil
	}
	if settings.ConfigError != "" {
		return []finding{{
			probeId: lbSplitProbeId, tier: tierWarn, class: lbSplitProbeClassUnobservable, target: lbSplitProbeTarget,
			sustain: 1, unobservable: true,
			symptom:  "the split probe is asked for and its settings do not read",
			observed: settings.ConfigError,
			action:   "Fix monitor.yml's lb_split_probe block or services.yml's lb inventory and tls_split, as the observed value says.",
		}}, nil
	}
	hellos := [][]byte{}
	for _, root := range settings.Roots {
		hello, err := lbSplitProbeClientHello(lbSplitProbeServerName(root))
		if err != nil {
			return nil, fmt.Errorf("the probe hello did not build: %w", err)
		}
		hellos = append(hellos, hello)
	}

	tasks := []*lbSplitTask{}
	blindBlocks := []string{}
	for _, lbInterface := range settings.Interfaces {
		added := 0
		for _, family := range settings.Families {
			address, network := lbInterface.Ipv4, "tcp4"
			if family == lbSplitFamilyIpv6 {
				address, network = lbInterface.Ipv6, "tcp6"
			}
			if !address.IsValid() {
				continue
			}
			tasks = append(tasks, &lbSplitTask{
				block:   lbInterface.Block,
				family:  family,
				address: netip.AddrPortFrom(address, lbSplitProbePort).String(),
				network: network,
			})
			added++
		}
		if added == 0 {
			blindBlocks = append(blindBlocks, lbInterface.Block)
		}
	}

	probeOne := func(task *lbSplitTask) {
		note := func(half string, outcome lbSplitOutcome) {
			switch outcome.verdict {
			case lbSplitFailed:
				task.failed = true
			case lbSplitBlind:
				task.blind = true
			default:
				return
			}
			task.outcomes = append(task.outcomes, task.family+" "+half+": "+outcome.reason)
		}
		for index, hello := range hellos {
			if ctx.Err() != nil {
				return
			}
			response, err := env.runner.tcpExchange(ctx, task.network, task.address, hello, lbSplitTlsAlertRecordByteCount)
			note(lbSplitProbeServerName(settings.Roots[index]), lbSplitUserAnswer(response, err))
		}
		if ctx.Err() != nil {
			return
		}
		if settings.PlatformName == "" {
			note("platform", lbSplitOutcome{verdict: lbSplitBlind, reason: "no platform name is configured"})
			return
		}
		observation, err := env.runner.tlsCertificates(ctx, task.network, task.address, settings.PlatformName)
		note("platform", lbSplitPlatformAnswer(observation, err))
	}
	concurrency := max(1, settings.Concurrency)
	semaphore := make(chan struct{}, concurrency)
	var wait sync.WaitGroup
	for _, task := range tasks {
		wait.Add(1)
		go func() {
			defer wait.Done()
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-ctx.Done():
				return
			}
			probeOne(task)
		}()
	}
	wait.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// one finding per interface, so each interface pages once, whatever failed on it
	blockTasks := map[string][]*lbSplitTask{}
	blocks := []string{}
	for _, task := range tasks {
		if _, ok := blockTasks[task.block]; !ok {
			blocks = append(blocks, task.block)
		}
		blockTasks[task.block] = append(blockTasks[task.block], task)
	}
	sort.Strings(blocks)
	findings := []finding{}
	for _, block := range blocks {
		outcomes := []string{}
		failed := false
		blind := false
		for _, task := range blockTasks[block] {
			outcomes = append(outcomes, task.outcomes...)
			failed = failed || task.failed
			blind = blind || task.blind
		}
		switch {
		case failed:
			findings = append(findings, lbSplitFailingFinding(settings, block, outcomes))
		case blind:
			findings = append(findings, lbSplitBlindFinding(settings, block, strings.Join(outcomes, "; ")))
		default:
			findings = append(findings, healthyFinding(lbSplitProbeId, tierPage, lbSplitProbeClassFailing, block))
		}
	}
	for _, block := range blindBlocks {
		findings = append(findings, lbSplitBlindFinding(settings, block, "the interface has no address of a probed family"))
	}
	return findings, nil
}

// The page for a block whose split answered wrongly: outcomes name each half that did not pass, by family
// and probe name, never by address.
func lbSplitFailingFinding(settings *LbSplitProbeSettings, block string, outcomes []string) finding {
	return finding{
		probeId: lbSplitProbeId, tier: tierPage, class: lbSplitProbeClassFailing, target: block,
		sustain: settings.PageSustain,
		symptom: "an lb interface's SNI split answers a probe as only a broken split would",
		mechanism: "The interface's stream server reads each hello's SNI and sends names under the roots to tlsedge and " +
			"every other name to the http servers (§13.4, W1). probe.edge.<root> answered by anything but tlsedge's " +
			"unrecognized_name, or the platform name answered by anything but a certificate that verifies for it, means " +
			"user names or platform names reach the wrong branch here; the dns edges drop it from user-name answers and " +
			"Route 53's checks from platform answers, and visitors who reach it meanwhile fail.",
		baseline: "probe.edge.<root> draws tlsedge's plaintext unrecognized_name alert, and the platform name a " +
			"certificate that verifies for it, on every probed family.",
		observed: strings.Join(outcomes, "; "),
		action: "Roll the interface back to its prior lb version (warpctl deploy main lb <prior version> " + block +
			"), draining it first when there is time. If every interface fails only the platform half alike, the " +
			"certificate is the cause (SIGNALS.md §18.2), not the split.",
		verify:   "Both halves pass on every probed family for two runs.",
		playbook: "warp/lb/SPLIT-ROLLOUT.md's rollback; §13.4 and §19.8 (DESIGN.md)",
	}
}

// The visibility alert for a block the probe could not finish: it holds the block's page open.
func lbSplitBlindFinding(settings *LbSplitProbeSettings, block string, observed string) finding {
	return finding{
		probeId: lbSplitProbeId, tier: tierWarn, class: lbSplitProbeClassUnobservable, target: block,
		sustain: settings.BlindSustain, unobservable: true,
		symptom: "the split probe could not finish on an lb interface, so its split is unknown",
		mechanism: "A timeout or a connection that never opened fails the same way for the monitor's own path as for " +
			"the interface, so it is no evidence either way: the interface's page, if open, stays open.",
		baseline: "Every probed family of the interface answers both halves, rightly or wrongly, within the probe's timeout.",
		observed: observed,
		action:   "Check the monitor host's route to the family named, then the interface's host and its lb.",
		verify:   "The interface answers both halves, rightly or wrongly, on the next run.",
		playbook: "SIGNALS.md §31.1",
	}
}
