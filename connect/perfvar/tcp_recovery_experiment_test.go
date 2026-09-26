//go:build acklineagetrace

package perfvar

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/netip"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect"
	"golang.org/x/net/dns/dnsmessage"
	"golang.org/x/net/http2"
	"gvisor.dev/gvisor/pkg/tcpip"
)

// This is a screening experiment, not a canonical PERFVAR baseline. It changes
// only the client application TUN. The peer's surrogate stack stays at 8s, and
// no OS socket, provider selection, carrier, or established stream is migrated.
type tcpRecoveryArm struct {
	Name                  string        `json:"name"`
	ClientMaxRTO          time.Duration `json:"client_max_rto_nanoseconds"`
	NoFirstByteRetryAfter time.Duration `json:"no_first_byte_retry_after_nanoseconds"`
}

func tcpRecoveryArmNamed(name string) (tcpRecoveryArm, error) {
	arm := tcpRecoveryArm{Name: name, ClientMaxRTO: 8 * time.Second}
	switch name {
	case "control":
	case "rto4":
		arm.ClientMaxRTO = 4 * time.Second
	case "rto2":
		arm.ClientMaxRTO = 2 * time.Second
	case "early":
		arm.NoFirstByteRetryAfter = 8 * time.Second
	default:
		return tcpRecoveryArm{}, fmt.Errorf("unsupported TCP recovery arm %q", name)
	}
	return arm, nil
}

type tcpRecoveryScenario struct {
	Version            int            `json:"version"`
	Consumer           string         `json:"consumer"`
	Profile            networkProfile `json:"profile"`
	ForcedPayloadDrops int            `json:"forced_payload_drops"`
	ResponseBytes      int            `json:"response_bytes"`
	RequestBudget      time.Duration  `json:"request_budget_nanoseconds"`
	PeerMaxRTO         time.Duration  `json:"peer_max_rto_nanoseconds"`
	FaultPolicy        string         `json:"fault_policy"`
	RetryPolicy        string         `json:"retry_policy"`
	SettingsScope      string         `json:"settings_scope"`
}

func tcpRecoveryScenarioNamed(consumer, name string) (tcpRecoveryScenario, error) {
	if consumer != "doh" && consumer != "proxy" {
		return tcpRecoveryScenario{}, fmt.Errorf("unsupported TCP consumer %q", consumer)
	}
	profiles := initialNetworkProfiles(20260810)
	profile := profiles["clean-lan"]
	losses := 0
	switch {
	case name == "clean":
	case name == "clean-rtt3s" || name == "clean-rtt3s-burst-1":
		profile.Name = "diagnostic-" + name
		profile.Forward.BaseDelay, profile.Reverse.BaseDelay = 1500*time.Millisecond, 1500*time.Millisecond
		profile.Forward.Jitter, profile.Reverse.Jitter = 0, 0
		if name == "clean-rtt3s-burst-1" {
			// The clean high-RTT control can finish before RACK's first PTO.
			// One identical first-payload loss also exercises the capped RTO
			// after that probe, when the peer's ACK itself needs 3 seconds.
			losses = 1
		}
	case name == "low-edge":
		// Cell-edge lives in its own catalog and is already uplink-forward.
		// A missing map entry must never become a zero-capacity experiment.
		profile = cellEdgeNetworkProfiles(20260810)[cellEdge256kDown64kUpName]
		profile.Name = "diagnostic-application-" + profile.Name
	case strings.HasPrefix(name, "burst-"):
		var err error
		losses, err = strconv.Atoi(strings.TrimPrefix(name, "burst-"))
		if err != nil || losses != 1 && losses != 3 && losses != 6 && losses != 10 {
			return tcpRecoveryScenario{}, fmt.Errorf("unsupported bounded burst %q", name)
		}
		profile.Name = "diagnostic-" + name
	default:
		return tcpRecoveryScenario{}, fmt.Errorf("unsupported TCP profile %q", name)
	}
	if err := validateTCPRecoveryProfile(profile); err != nil {
		return tcpRecoveryScenario{}, err
	}
	responseBytes := 16 * 1024
	if consumer == "doh" {
		responseBytes = 4 // useful A-record address bytes, not DNS/TLS overhead
	}
	return tcpRecoveryScenario{
		Version: 1, Consumer: consumer, Profile: profile, ForcedPayloadDrops: losses,
		ResponseBytes: responseBytes, RequestBudget: 60 * time.Second, PeerMaxRTO: 8 * time.Second,
		FaultPolicy:   "first-N-client-TCP-payload-transmissions-after-warmup; ACK/SYN-only excluded",
		RetryPolicy:   "at-most-one-explicit-new-connection; DoH-idempotent; proxy-original-stream-failure-retained",
		SettingsScope: "client-application-TUN-only; peer-surrogate-unchanged; no-platform-OS-socket-setting",
	}, nil
}

func validateTCPRecoveryProfile(profile networkProfile) error {
	if profile.Name == "" || profile.Seed == 0 || profile.InnerMtu < 576 ||
		profile.Forward.RateBitsPerSecond <= 0 || profile.Reverse.RateBitsPerSecond <= 0 ||
		profile.Forward.OuterMtu < profile.InnerMtu || profile.Reverse.OuterMtu < profile.InnerMtu ||
		profile.Forward.QueueByteCount <= 0 || profile.Reverse.QueueByteCount <= 0 {
		return fmt.Errorf("invalid or missing application TUN recovery profile %q", profile.Name)
	}
	return nil
}

func tcpRecoveryHash(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func tcpRecoveryTrace(scenario tcpRecoveryScenario, run int) (perfvarTrace, error) {
	return perfvarTraceForRun(perfvarScenario{
		Route: "diagnostic-direct-application-TUN", Workload: perfvarWorkload("diagnostic-" + scenario.Consumer),
		Direction: perfvarDirectionDownload, Profile: scenario.Profile,
		ProviderAccessProfile: scenario.Profile, Resource: perfvarResourceMobile,
		PayloadByteCount: int64(scenario.ResponseBytes), RunCount: 3, FlowCount: 1,
	}, run)
}

// This callback is owned by one scheduler and uses only IPv4/TCP headers.
// It retains no packet bytes and cannot overshoot N during a batched write.
type tcpRecoveryBurst struct {
	limit int64
	drops atomic.Int64
}

func tcpRecoveryHasPayload(packet []byte) bool {
	if len(packet) < 40 || packet[0]>>4 != 4 || packet[9] != 6 {
		return false
	}
	ipHeader := int(packet[0]&15) * 4
	if ipHeader < 20 || len(packet) < ipHeader+20 {
		return false
	}
	tcpHeader := int(packet[ipHeader+12]>>4) * 4
	return tcpHeader >= 20 && len(packet) > ipHeader+tcpHeader
}

func (self *tcpRecoveryBurst) drop(packet []byte) bool {
	if !tcpRecoveryHasPayload(packet) {
		return false
	}
	for {
		count := self.drops.Load()
		if count >= self.limit {
			return false
		}
		if self.drops.CompareAndSwap(count, count+1) {
			return true
		}
	}
}

type tcpRecoveryMemory struct {
	HeapBeforeBytes    uint64 `json:"heap_before_bytes"`
	HeapAfterBytes     uint64 `json:"heap_after_bytes"`
	HeapPeakBytes      uint64 `json:"heap_peak_bytes"`
	AllocatedBytes     uint64 `json:"allocated_bytes"`
	Allocations        uint64 `json:"allocations"`
	GarbageCollections uint32 `json:"garbage_collections"`
	ProcessMaxRSSBytes int64  `json:"process_max_rss_bytes"`
	InitialMaxRSSBytes int64  `json:"initial_max_rss_bytes"`
	GoroutinesBefore   int    `json:"goroutines_before"`
	GoroutinesAfter    int    `json:"goroutines_after"`
}

type tcpRecoveryRecord struct {
	RecordType             string                    `json:"record_type"`
	ScenarioHash           string                    `json:"scenario_hash"`
	ProfileHash            string                    `json:"profile_hash"`
	Scenario               tcpRecoveryScenario       `json:"scenario"`
	Trace                  perfvarTrace              `json:"trace"`
	Host                   perfvarHostMetadata       `json:"host"`
	Arm                    tcpRecoveryArm            `json:"arm"`
	RunIndex               int                       `json:"run_index"`
	Correct                bool                      `json:"correct"`
	FailureReason          string                    `json:"failure_reason,omitempty"`
	InvalidReason          string                    `json:"invalid_reason,omitempty"`
	OriginalStreamCorrect  bool                      `json:"original_stream_correct"`
	NewConnectionCorrect   bool                      `json:"new_connection_correct"`
	TransparentStreamRetry bool                      `json:"transparent_stream_retry"`
	ProviderOrEgressMove   bool                      `json:"provider_or_egress_move"`
	Attempts               int                       `json:"attempts"`
	FirstAttemptFailure    string                    `json:"first_attempt_failure,omitempty"`
	FirstAttemptDuration   time.Duration             `json:"first_attempt_duration_nanoseconds"`
	SetupDuration          time.Duration             `json:"setup_duration_nanoseconds"`
	Workload               workloadResult            `json:"workload"`
	ClientTCP              h1FailureTCPStackCounters `json:"client_tcp"`
	PeerTCP                h1FailureTCPStackCounters `json:"peer_tcp"`
	TCPInfoBefore          tcpip.TCPInfoOption       `json:"tcp_info_before"`
	TCPInfoAfter           tcpip.TCPInfoOption       `json:"tcp_info_after"`
	ForcedPayloadDropCount int64                     `json:"forced_payload_drop_count"`
	ServerAccepted         int64                     `json:"server_accepted"`
	ServerHTTP2Requests    int64                     `json:"server_http2_requests"`
	Memory                 tcpRecoveryMemory         `json:"memory"`
	BaselineEligible       bool                      `json:"baseline_eligible"`
}

func tcpRecoveryStatsDelta(after, before h1FailureTCPStackCounters) h1FailureTCPStackCounters {
	return h1FailureTCPStackCounters{
		SegmentsSent:     after.SegmentsSent - before.SegmentsSent,
		SegmentsReceived: after.SegmentsReceived - before.SegmentsReceived,
		Retransmits:      after.Retransmits - before.Retransmits, Timeouts: after.Timeouts - before.Timeouts,
		FastRetransmits:      after.FastRetransmits - before.FastRetransmits,
		SlowStartRetransmits: after.SlowStartRetransmits - before.SlowStartRetransmits,
		ResetsSent:           after.ResetsSent - before.ResetsSent, ResetsReceived: after.ResetsReceived - before.ResetsReceived,
		SegmentSendErrors: after.SegmentSendErrors - before.SegmentSendErrors,
	}
}

func tcpRecoveryMaxRSS() int64 {
	var usage syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &usage) != nil {
		return 0
	}
	if runtime.GOOS == "darwin" {
		return usage.Maxrss
	}
	return usage.Maxrss * 1024
}

type tcpRecoveryOperation struct {
	warm     func(context.Context) error
	request  func(context.Context, *atomic.Int64, int) (int64, string, error)
	retire   func()
	close    func()
	accepted atomic.Int64
	http2    atomic.Int64
	raw      atomic.Pointer[clientconnect.TunTcpConn]
}

func runTCPRecoveryRequests(ctx context.Context, arm tcpRecoveryArm, op *tcpRecoveryOperation, record *tcpRecoveryRecord) (useful int64, hash string, firstByte time.Duration, duration time.Duration, err error) {
	started := time.Now()
	for attempt := 1; attempt <= 2; attempt++ {
		record.Attempts = attempt
		attemptCtx, cancel := context.WithCancel(ctx)
		var first atomic.Int64
		var earlyCanceled atomic.Bool
		var timer *time.Timer
		if attempt == 1 && arm.NoFirstByteRetryAfter > 0 {
			timer = time.AfterFunc(arm.NoFirstByteRetryAfter, func() {
				if first.Load() == 0 {
					earlyCanceled.Store(true)
					cancel()
				}
			})
		}
		useful, hash, err = op.request(attemptCtx, &first, attempt)
		if timer != nil {
			timer.Stop()
		}
		cancel()
		if attempt == 1 {
			record.FirstAttemptDuration = time.Since(started)
			record.OriginalStreamCorrect = err == nil
		}
		if err == nil {
			if ns := first.Load(); ns != 0 {
				firstByte = time.Unix(0, ns).Sub(started)
			}
			record.NewConnectionCorrect = attempt > 1
			break
		}
		if attempt == 1 {
			record.FirstAttemptFailure = err.Error()
		}
		if !earlyCanceled.Load() || ctx.Err() != nil || attempt == 2 {
			break
		}
		op.retire()
	}
	duration = time.Since(started)
	return
}

func newTCPRecoveryDoh(path *tunPath) (*tcpRecoveryOperation, error) {
	listener, err := path.right.ListenTCP(&net.TCPAddr{IP: path.endpointAddress(false)})
	if err != nil {
		return nil, err
	}
	return newTCPRecoveryDohWithListener(path.left, listener)
}

// The same production Tun.DohCache request can use either a direct peer TUN
// or a provider-side origin listener. The client never substitutes a host
// dialer: buildDohCache binds its remote transport to this exact TUN.
func newTCPRecoveryDohWithListener(clientTun *clientconnect.Tun, listener net.Listener) (*tcpRecoveryOperation, error) {
	serverTLS, clientTLS, err := newWorkloadTlsConfigs()
	if err != nil {
		listener.Close()
		return nil, err
	}
	serverTLS.NextProtos, clientTLS.NextProtos = []string{"h2"}, []string{"h2"}
	op := &tcpRecoveryOperation{}
	answer := netip.MustParseAddr("203.0.113.66")
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.ProtoMajor != 2 {
			http.Error(w, "HTTP/2 required", http.StatusBadRequest)
			return
		}
		op.http2.Add(1)
		var wire []byte
		var err error
		if request.Method == http.MethodGet {
			wire, err = base64.RawURLEncoding.DecodeString(request.URL.Query().Get("dns"))
		} else if request.Method == http.MethodPost {
			wire, err = io.ReadAll(io.LimitReader(request.Body, 4096))
		} else {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		var query dnsmessage.Message
		if err != nil || query.Unpack(wire) != nil || len(query.Questions) != 1 || query.Questions[0].Type != dnsmessage.TypeA {
			http.Error(w, "DNS question", http.StatusBadRequest)
			return
		}
		question := query.Questions[0]
		response := dnsmessage.Message{
			Header: dnsmessage.Header{ID: query.ID, Response: true, RecursionAvailable: true}, Questions: query.Questions,
			Answers: []dnsmessage.Resource{{Header: dnsmessage.ResourceHeader{Name: question.Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 60}, Body: &dnsmessage.AResource{A: answer.As4()}}},
		}
		body, err := response.Pack()
		if err != nil {
			http.Error(w, "encode", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write(body)
	}), ConnState: func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			op.accepted.Add(1)
		}
	}}
	if err := http2.ConfigureServer(server, &http2.Server{}); err != nil {
		listener.Close()
		return nil, err
	}
	serverDone := make(chan struct{})
	go func() { defer close(serverDone); _ = server.Serve(tls.NewListener(listener, serverTLS)) }()
	resolver := &clientconnect.DnsResolverSettings{EnableRemoteDoh: true, RemoteDohUrlsIpv4: []string{"https://" + listener.Addr().String() + "/dns-query"}, TlsConfig: clientTLS}
	reset := func() { clientTun.SetDnsResolverSettings(resolver, 60*time.Second) }
	reset()
	query := func(ctx context.Context, name string, first *atomic.Int64) (int64, string, error) {
		ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
			GotFirstResponseByte: func() { first.CompareAndSwap(0, time.Now().UnixNano()) },
			GotConn: func(info httptrace.GotConnInfo) {
				connection := info.Conn
				if secure, ok := connection.(*tls.Conn); ok {
					connection = secure.NetConn()
				}
				if raw, ok := connection.(*clientconnect.TunTcpConn); ok {
					op.raw.Store(raw)
				}
			},
		})
		addresses, authoritative := clientTun.DohCache().QueryResult(ctx, "A", name)
		if !authoritative || len(addresses) != 1 || addresses[0] != answer {
			return 0, "", fmt.Errorf("DoH answer missing/incorrect (ctx=%v authoritative=%t count=%d)", ctx.Err(), authoritative, len(addresses))
		}
		payload := answer.As4()
		sum := sha256.Sum256(payload[:])
		return 4, hex.EncodeToString(sum[:]), nil
	}
	op.warm = func(ctx context.Context) error {
		var first atomic.Int64
		_, _, err := query(ctx, "warm.recovery.invalid", &first)
		return err
	}
	op.request = func(ctx context.Context, first *atomic.Int64, attempt int) (int64, string, error) {
		if attempt > 1 {
			// This fixture owns one query/cache. Retire the canceled request and
			// its connection before replaying the same idempotent DNS question.
			reset()
		}
		return query(ctx, "measured.recovery.invalid", first)
	}
	op.retire = func() { clientTun.DohCache().Close() }
	op.close = func() { op.retire(); _ = server.Close(); _ = listener.Close(); <-serverDone }
	return op, nil
}

func newTCPRecoveryProxy(path *tunPath, responseBytes int) (*tcpRecoveryOperation, error) {
	listener, err := path.right.ListenTCP(&net.TCPAddr{IP: path.endpointAddress(false)})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(path.ctx)
	op := &tcpRecoveryOperation{}
	body := bytes.Repeat([]byte{0x6d}, responseBytes)
	var workers sync.WaitGroup
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			op.accepted.Add(1)
			workers.Go(func() {
				defer connection.Close()
				stop := context.AfterFunc(ctx, func() { _ = connection.Close() })
				defer stop()
				var request [128]byte
				for {
					if _, err := io.ReadFull(connection, request[:]); err != nil {
						return
					}
					response := body
					if request[0] == 0 {
						response = []byte{0x6d}
					}
					if _, err := connection.Write(response); err != nil {
						return
					}
				}
			})
		}
	}()
	var current net.Conn
	dial := func(ctx context.Context) error {
		connection, err := path.left.DialContext(ctx, "tcp", listener.Addr().String())
		if err != nil {
			return err
		}
		raw, ok := connection.(*clientconnect.TunTcpConn)
		if !ok {
			connection.Close()
			return fmt.Errorf("proxy surrogate did not use TUN: %T", connection)
		}
		current = connection
		op.raw.Store(raw)
		return nil
	}
	exchange := func(ctx context.Context, warm bool, first *atomic.Int64) (int64, string, error) {
		deadline, _ := ctx.Deadline()
		_ = current.SetDeadline(deadline)
		stop := interruptDeadlineOnContext(ctx, current)
		defer stop()
		var request [128]byte
		want := body
		if warm {
			want = body[:1]
		} else {
			request[0] = 1
		}
		if _, err := current.Write(request[:]); err != nil {
			return 0, "", err
		}
		response := make([]byte, len(want))
		if _, err := io.ReadFull(current, response[:1]); err != nil {
			return 0, "", err
		}
		first.CompareAndSwap(0, time.Now().UnixNano())
		if _, err := io.ReadFull(current, response[1:]); err != nil {
			return 0, "", err
		}
		if !bytes.Equal(response, want) {
			return 0, "", fmt.Errorf("proxy stream content mismatch")
		}
		sum := sha256.Sum256(response)
		return int64(len(response)), hex.EncodeToString(sum[:]), nil
	}
	op.warm = func(ctx context.Context) error {
		if err := dial(ctx); err != nil {
			return err
		}
		var first atomic.Int64
		_, _, err := exchange(ctx, true, &first)
		return err
	}
	op.request = func(ctx context.Context, first *atomic.Int64, attempt int) (int64, string, error) {
		if attempt > 1 {
			if current != nil {
				_ = current.Close()
			}
			if err := dial(ctx); err != nil {
				return 0, "", err
			}
		}
		return exchange(ctx, false, first)
	}
	op.retire = func() {
		if current != nil {
			_ = current.Close()
		}
	}
	op.close = func() { op.retire(); cancel(); _ = listener.Close(); <-serverDone; workers.Wait() }
	return op, nil
}

func measureTCPRecoveryExperiment(ctx context.Context, scenario tcpRecoveryScenario, arm tcpRecoveryArm, run int) (record tcpRecoveryRecord) {
	record = tcpRecoveryRecord{RecordType: "diagnostic-tcp-recovery", Scenario: scenario, ScenarioHash: tcpRecoveryHash(scenario), ProfileHash: tcpRecoveryHash(scenario.Profile), Arm: arm, RunIndex: run}
	trace, err := tcpRecoveryTrace(scenario, run)
	if err != nil {
		record.InvalidReason = err.Error()
		return
	}
	record.Trace = trace
	profile := scenario.Profile
	profile.Seed = trace.ApplicationOrDirectSeed
	resources := defaultTunResourceProfile()
	resources.ChannelSize = 32 // server ProxyDevice's sequence/TUN default
	if scenario.Consumer == "doh" {
		resources.ChannelSize, resources.TcpBufferDefault, resources.TcpBufferMax = 128, 64*1024, 64*1024
		resources.UdpBuffer = 32 * 1024
	}
	var memoryBefore runtime.MemStats
	runtime.ReadMemStats(&memoryBefore)
	record.Memory.HeapBeforeBytes, record.Memory.GoroutinesBefore = memoryBefore.HeapAlloc, runtime.NumGoroutine()
	record.Memory.InitialMaxRSSBytes = tcpRecoveryMaxRSS()
	var peak atomic.Uint64
	peak.Store(memoryBefore.HeapAlloc)
	stopMemory, memoryDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(memoryDone)
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stopMemory:
				return
			case <-ticker.C:
				var sample runtime.MemStats
				runtime.ReadMemStats(&sample)
				peak.Store(max(peak.Load(), sample.HeapAlloc))
			}
		}
	}()
	defer func() {
		close(stopMemory)
		<-memoryDone
		var after runtime.MemStats
		runtime.ReadMemStats(&after)
		record.Memory.HeapAfterBytes, record.Memory.HeapPeakBytes = after.HeapAlloc, max(after.HeapAlloc, peak.Load())
		record.Memory.AllocatedBytes, record.Memory.Allocations = after.TotalAlloc-memoryBefore.TotalAlloc, after.Mallocs-memoryBefore.Mallocs
		record.Memory.GarbageCollections, record.Memory.GoroutinesAfter = after.NumGC-memoryBefore.NumGC, runtime.NumGoroutine()
		record.Memory.ProcessMaxRSSBytes = tcpRecoveryMaxRSS()
	}()
	setup := time.Now()
	path, err := newTunPathWithSettings(ctx, profile, resources, func(left bool, settings *clientconnect.TunSettings) {
		if left {
			settings.TcpMaxRto = arm.ClientMaxRTO
		} else {
			settings.TcpMaxRto = scenario.PeerMaxRTO
		}
		settings.DialRace = 1
	})
	if err != nil {
		record.FailureReason = err.Error()
		return
	}
	defer path.close()
	var op *tcpRecoveryOperation
	if scenario.Consumer == "doh" {
		op, err = newTCPRecoveryDoh(path)
	} else {
		op, err = newTCPRecoveryProxy(path, scenario.ResponseBytes)
	}
	if err != nil {
		record.FailureReason = err.Error()
		return
	}
	defer op.close()
	if err := op.warm(ctx); err != nil {
		record.FailureReason = "warmup: " + err.Error()
		return
	}
	boundary, err := path.beginMeasurement(ctx)
	if err != nil {
		record.InvalidReason = err.Error()
		return
	}
	record.SetupDuration = time.Since(setup)
	if raw := op.raw.Load(); raw != nil {
		record.TCPInfoBefore, _ = raw.TcpInfo()
	} else {
		record.InvalidReason = "warmed request has no native TUN TCP ownership"
		return
	}
	burst := &tcpRecoveryBurst{limit: int64(scenario.ForcedPayloadDrops)}
	if burst.limit > 0 {
		path.forwardLink.forcedLossForTest.Store(&linkLossTestHook{drop: burst.drop})
		defer path.forwardLink.forcedLossForTest.Store(nil)
	}
	clientBefore, peerBefore := h1FailureTCPStats(path.left), h1FailureTCPStats(path.right)
	requestCtx, requestCancel := context.WithTimeout(ctx, scenario.RequestBudget)
	defer requestCancel()
	useful, hash, firstByte, duration, err := runTCPRecoveryRequests(requestCtx, arm, op, &record)
	if err != nil {
		record.FailureReason = err.Error()
	} else {
		record.Correct = true
	}
	if scenario.Consumer == "proxy" && !record.OriginalStreamCorrect {
		// A later connection is not correctness for the failed TCP stream.
		record.Correct = false
		record.FailureReason = "original proxy stream failed; explicit application new-connection outcome reported separately"
	}
	if raw := op.raw.Load(); raw != nil {
		record.TCPInfoAfter, _ = raw.TcpInfo()
	}
	op.retire()
	forward, reverse, finishErr := path.finishMeasurement(ctx, boundary)
	if finishErr != nil {
		record.InvalidReason = finishErr.Error()
	}
	record.ClientTCP = tcpRecoveryStatsDelta(h1FailureTCPStats(path.left), clientBefore)
	record.PeerTCP = tcpRecoveryStatsDelta(h1FailureTCPStats(path.right), peerBefore)
	record.ForcedPayloadDropCount = burst.drops.Load()
	if record.Correct && record.ForcedPayloadDropCount != int64(scenario.ForcedPayloadDrops) {
		record.InvalidReason = "successful request did not consume its exact bounded loss trace"
	}
	record.ServerAccepted, record.ServerHTTP2Requests = op.accepted.Load(), op.http2.Load()
	if scenario.Consumer == "doh" && record.Correct && record.ServerHTTP2Requests < 2 {
		record.InvalidReason = "DoH request bypassed production HTTP/2 path"
	}
	record.Workload = finishWorkloadResult(workloadResult{
		UsefulByteCount: useful, Duration: duration, TimeToFirstByte: firstByte, ContentHash: hash,
		Latency: summarizeLatencies([]time.Duration{duration}), ForwardLink: forward, ReverseLink: reverse,
	})
	return
}

func TestTCPRecoveryExperimentReplay(t *testing.T) {
	name := os.Getenv("CONNECT_PERFVAR_TCP_RECOVERY_ARM")
	if name == "" {
		t.Skip("explicit diagnostic TCP recovery selector required")
	}
	if os.Getenv("URNETWORK_ACK_LINEAGE_REPLAY_SLOT") != "granted" || runtime.GOMAXPROCS(0) != 8 || !ackLineageReplayRuntimeSupported(runtime.Version(), runtime.GOOS, runtime.GOARCH, 8) || perfvarRaceEnabled {
		t.Fatal("TCP experiment requires the owned canonical CPU8 non-race host")
	}
	arm, err := tcpRecoveryArmNamed(name)
	if err != nil {
		t.Fatal(err)
	}
	scenario, err := tcpRecoveryScenarioNamed(os.Getenv("CONNECT_PERFVAR_TCP_RECOVERY_CONSUMER"), os.Getenv("CONNECT_PERFVAR_TCP_RECOVERY_PROFILE"))
	if err != nil {
		t.Fatal(err)
	}
	run, err := strconv.Atoi(os.Getenv("CONNECT_PERFVAR_TCP_RECOVERY_RUN"))
	if err != nil || run < 1 || run > 30 {
		t.Fatal("TCP experiment run index must be 1..30")
	}
	host := currentPerfvarHostMetadata()
	if err := validatePerfvarHostMetadata(host); err != nil {
		t.Fatal(err)
	}
	host.MeasurementKind = "diagnostic-application-tun-tcp-recovery"
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	record := measureTCPRecoveryExperiment(ctx, scenario, arm, run)
	record.Host = host
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("[perfvar-tcp-recovery] %s", encoded)
	if record.InvalidReason != "" {
		t.Errorf("invalid experiment: %s", record.InvalidReason)
	}
	if !record.Correct {
		t.Errorf("incorrect workload: %s", record.FailureReason)
	}
}

func TestTCPRecoveryExperimentIdentityAndScope(t *testing.T) {
	for _, consumer := range []string{"doh", "proxy"} {
		for _, profile := range []string{"clean", "clean-rtt3s", "clean-rtt3s-burst-1", "burst-1", "burst-3", "burst-6", "burst-10", "low-edge"} {
			scenario, err := tcpRecoveryScenarioNamed(consumer, profile)
			if err != nil {
				t.Fatal(err)
			}
			trace, err := tcpRecoveryTrace(scenario, 2)
			if err != nil || trace.ApplicationOrDirectSeed == 0 || scenario.PeerMaxRTO != 8*time.Second {
				t.Fatal("unstable experiment identity/scope")
			}
			for _, name := range []string{"control", "rto4", "rto2", "early"} {
				arm, err := tcpRecoveryArmNamed(name)
				if err != nil || arm.ClientMaxRTO < 2*time.Second || arm.ClientMaxRTO > 8*time.Second {
					t.Fatal("invalid arm")
				}
				paired, _ := tcpRecoveryTrace(scenario, 2)
				if paired != trace {
					t.Fatal("arm changed impairment identity")
				}
			}
		}
	}
	if _, err := tcpRecoveryArmNamed("combined"); err == nil {
		t.Fatal("unmeasured combined arm enabled")
	}
	if _, err := tcpRecoveryScenarioNamed("proxy", "burst-11"); err == nil {
		t.Fatal("unbounded fault selector accepted")
	}
}

func TestTCPRecoveryExperimentHighRTTOneDropPreservesCleanPath(t *testing.T) {
	for _, consumer := range []string{"doh", "proxy"} {
		clean, err := tcpRecoveryScenarioNamed(consumer, "clean-rtt3s")
		if err != nil {
			t.Fatal(err)
		}
		oneDrop, err := tcpRecoveryScenarioNamed(consumer, "clean-rtt3s-burst-1")
		if err != nil {
			t.Fatal(err)
		}
		want := clean
		want.Profile.Name = "diagnostic-clean-rtt3s-burst-1"
		want.ForcedPayloadDrops = 1
		if tcpRecoveryHash(oneDrop) != tcpRecoveryHash(want) {
			t.Fatal("adversarial control changed more than its explicit first-payload loss")
		}
		for _, direction := range []linkProfile{oneDrop.Profile.Forward, oneDrop.Profile.Reverse} {
			if direction.BaseDelay != 1500*time.Millisecond || direction.Jitter != 0 ||
				direction.LossModel != lossModelNone || direction.LossProbability != 0 ||
				direction.RateBitsPerSecond != 1_000_000_000 {
				t.Fatalf("high-RTT control added an unbounded impairment: %+v", direction)
			}
		}
		for _, run := range []int{11, 20} {
			trace, err := tcpRecoveryTrace(oneDrop, run)
			if err != nil || trace.RunIndex != run || trace.ApplicationOrDirectSeed == 0 {
				t.Fatalf("independent confirmation trace not available: run=%d trace=%+v err=%v", run, trace, err)
			}
		}
	}
	for _, invalid := range []string{"clean-rtt3s-burst-0", "clean-rtt3s-burst-2", "clean-rtt3s-burst-10"} {
		if _, err := tcpRecoveryScenarioNamed("doh", invalid); err == nil {
			t.Fatalf("unplanned adversarial loss budget accepted: %q", invalid)
		}
	}
}

func TestTCPRecoveryExperimentLowEdgeProfileCannotSilentlyMiss(t *testing.T) {
	scenario, err := tcpRecoveryScenarioNamed("doh", "low-edge")
	if err != nil {
		t.Fatal(err)
	}
	profile := scenario.Profile
	if profile.Seed != 20260810 || profile.InnerMtu != 1200 || profile.Forward.OuterMtu != 1280 ||
		profile.Forward.RateBitsPerSecond != 64_000 || profile.Reverse.RateBitsPerSecond != 256_000 ||
		profile.Forward.BaseDelay != 400*time.Millisecond || profile.Reverse.BaseDelay != 400*time.Millisecond ||
		profile.Forward.LossModel != lossModelBurst || profile.Reverse.LossModel != lossModelBurst {
		t.Fatalf("low-edge lost its exact uplink/downlink impairment: %+v", profile)
	}
	if validateTCPRecoveryProfile(networkProfile{}) == nil {
		t.Fatal("missing profile accepted")
	}
	for _, mutate := range []func(*networkProfile){
		func(p *networkProfile) { p.Seed = 0 },
		func(p *networkProfile) { p.InnerMtu = 0 },
		func(p *networkProfile) { p.Forward.RateBitsPerSecond = 0 },
		func(p *networkProfile) { p.Reverse.RateBitsPerSecond = 0 },
		func(p *networkProfile) { p.Forward.OuterMtu = 0 },
	} {
		broken := profile
		mutate(&broken)
		if validateTCPRecoveryProfile(broken) == nil {
			t.Fatalf("malformed profile accepted: %+v", broken)
		}
	}
}

func TestTCPRecoveryExperimentBurstIsExactAndPayloadOnly(t *testing.T) {
	packet := make([]byte, 41)
	packet[0], packet[9], packet[32] = 0x45, 6, 0x50
	burst := &tcpRecoveryBurst{limit: 3}
	if burst.drop(packet[:40]) || burst.drop(nil) {
		t.Fatal("ACK-only or malformed packet consumed a loss")
	}
	var drops atomic.Int64
	var workers sync.WaitGroup
	for range 100 {
		workers.Go(func() {
			if burst.drop(packet) {
				drops.Add(1)
			}
		})
	}
	workers.Wait()
	if drops.Load() != 3 || burst.drops.Load() != 3 {
		t.Fatal("bounded fault overshot")
	}
}

func TestTCPRecoveryExperimentRetryOwnership(t *testing.T) {
	for _, mode := range []string{"no-first-byte", "progress", "ordinary-error", "parent-cancel", "retry-error"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			var retires atomic.Int64
			op := &tcpRecoveryOperation{retire: func() { retires.Add(1) }}
			op.request = func(ctx context.Context, first *atomic.Int64, attempt int) (int64, string, error) {
				if attempt == 2 {
					if retires.Load() != 1 {
						t.Fatal("new connection overlapped unretired owner")
					}
					if mode == "retry-error" {
						return 0, "", fmt.Errorf("second request failed")
					}
					first.Store(time.Now().UnixNano())
					return 4, "hash", nil
				}
				if mode == "ordinary-error" {
					return 0, "", fmt.Errorf("non-timeout request failure")
				}
				if mode == "progress" {
					first.Store(time.Now().UnixNano())
					time.Sleep(30 * time.Millisecond)
					return 4, "hash", ctx.Err()
				}
				if mode == "parent-cancel" {
					cancel()
				}
				<-ctx.Done()
				return 0, "", ctx.Err()
			}
			var record tcpRecoveryRecord
			_, _, _, _, err := runTCPRecoveryRequests(ctx, tcpRecoveryArm{NoFirstByteRetryAfter: 5 * time.Millisecond}, op, &record)
			wantRetry := mode == "no-first-byte" || mode == "retry-error"
			if (record.Attempts == 2) != wantRetry || (retires.Load() == 1) != wantRetry || record.Attempts > 2 {
				t.Fatalf("retry ownership mismatch: mode=%s record=%+v retires=%d", mode, record, retires.Load())
			}
			if (err == nil) != (mode == "progress" || mode == "no-first-byte") {
				t.Fatalf("unexpected outcome: mode=%s err=%v", mode, err)
			}
		})
	}
}

func TestTCPRecoveryExperimentCleanProductionPaths(t *testing.T) {
	for _, consumer := range []string{"doh", "proxy"} {
		t.Run(consumer, func(t *testing.T) {
			scenario, _ := tcpRecoveryScenarioNamed(consumer, "clean")
			arm, _ := tcpRecoveryArmNamed("control")
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			record := measureTCPRecoveryExperiment(ctx, scenario, arm, 1)
			if !record.Correct || record.InvalidReason != "" || !record.OriginalStreamCorrect || record.Attempts != 1 || record.TransparentStreamRetry || record.ProviderOrEgressMove || record.Workload.UsefulByteCount != int64(scenario.ResponseBytes) || record.Workload.TimeToFirstByte <= 0 || record.TCPInfoBefore.RTO == 0 {
				t.Fatalf("invalid clean control: %+v", record)
			}
		})
	}
}
