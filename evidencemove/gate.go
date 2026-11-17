package evidencemove

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// One condition the mover runs under, read again every Interval.
type Gate interface {
	Name() string
	Interval() time.Duration
	Check(ctx context.Context) GateState
}

type GateState struct {
	Open bool
	// why the gate is closed, or what it read when open
	Reason string
	// when an open gate closes without another check (zero: never)
	Until time.Time
}

// Keeps every gate's latest state, refreshed on each gate's interval, and a
// backoff after failed storage requests. Workers wait on it before every
// object.
type GateKeeper struct {
	gates []Gate
	now   func() time.Time
	logf  func(format string, args ...any)

	lock   sync.Mutex
	states []GateState
	// signaled when a gate is to be checked at once
	kicks []chan struct{}
	// closed and replaced on every change
	changed chan struct{}

	// after failed storage requests: closed until backoffUntil
	backoff      time.Duration
	backoffUntil time.Time
	lastFailure  time.Time
	failureCause string
	// the first backoff, the largest, and the quiet time that resets it
	minBackoff   time.Duration
	maxBackoff   time.Duration
	backoffReset time.Duration

	open        bool
	closedSince time.Time
	paused      time.Duration
}

// The bound of one gate check: the validator's is an RPC read and an ssh
// read of up to 40 s.
const gateCheckTimeout = time.Minute

// Storage failure backoff bounds: the first wait, the largest, and how long
// without a failure after a backoff ends resets it.
const (
	minFailureBackoff   = 5 * time.Second
	maxFailureBackoff   = 5 * time.Minute
	failureBackoffReset = 2 * time.Minute
)

// Every gate starts closed until its first check.
func NewGateKeeper(logf func(format string, args ...any), gates ...Gate) *GateKeeper {
	keeper := &GateKeeper{
		gates:   gates,
		now:     time.Now,
		logf:    logf,
		states:  make([]GateState, len(gates)),
		kicks:   make([]chan struct{}, len(gates)),
		changed: make(chan struct{}),

		minBackoff:   minFailureBackoff,
		maxBackoff:   maxFailureBackoff,
		backoffReset: failureBackoffReset,
	}
	for index, gate := range gates {
		keeper.states[index] = GateState{Reason: gate.Name() + " not checked yet"}
		keeper.kicks[index] = make(chan struct{}, 1)
	}
	keeper.closedSince = keeper.now()
	return keeper
}

// Checks each gate on its interval until ctx ends. Returns at once; the gates
// open as their first checks pass.
func (self *GateKeeper) Start(ctx context.Context) {
	for index, gate := range self.gates {
		go func() {
			for {
				checkCtx, cancel := context.WithTimeout(ctx, gateCheckTimeout)
				state := gate.Check(checkCtx)
				cancel()
				if ctx.Err() != nil {
					return
				}
				self.set(index, state)
				select {
				case <-ctx.Done():
					return
				case <-self.kicks[index]:
				case <-time.After(gate.Interval()):
				}
			}
		}()
	}
}

// Checks every gate once, in order, without starting the refresh: for tests
// and for a single read.
func (self *GateKeeper) CheckNow(ctx context.Context) {
	for index, gate := range self.gates {
		self.set(index, gate.Check(ctx))
	}
}

func (self *GateKeeper) set(index int, state GateState) {
	self.lock.Lock()
	defer self.lock.Unlock()
	self.states[index] = state
	self.update()
}

// Must hold lock. Logs every open/closed transition with its reason.
func (self *GateKeeper) update() {
	open, reason := self.openLocked(self.now())
	if open != self.open {
		now := self.now()
		if open {
			self.paused += now.Sub(self.closedSince)
			if self.logf != nil {
				self.logf("gates open: %s", reason)
			}
		} else {
			self.closedSince = now
			if self.logf != nil {
				self.logf("gates closed: %s", reason)
			}
		}
		self.open = open
	}
	close(self.changed)
	self.changed = make(chan struct{})
}

func (self *GateKeeper) openLocked(now time.Time) (bool, string) {
	if now.Before(self.backoffUntil) {
		return false, fmt.Sprintf("backing off %s after a failure: %s", self.backoffUntil.Sub(now).Round(time.Second), self.failureCause)
	}
	reasons := []string{}
	for index, state := range self.states {
		if !state.Open {
			return false, self.gates[index].Name() + ": " + state.Reason
		}
		if !state.Until.IsZero() && !now.Before(state.Until) {
			return false, self.gates[index].Name() + ": its window ended at " + state.Until.UTC().Format(time.RFC3339)
		}
		reasons = append(reasons, self.gates[index].Name()+": "+state.Reason)
	}
	return true, strings.Join(reasons, "; ")
}

// Whether every gate is open now, and why or why not.
func (self *GateKeeper) Open() (bool, string) {
	self.lock.Lock()
	defer self.lock.Unlock()
	return self.openLocked(self.now())
}

// Blocks until every gate is open, or ctx ends.
func (self *GateKeeper) Wait(ctx context.Context) error {
	for {
		self.lock.Lock()
		now := self.now()
		open, _ := self.openLocked(now)
		if open != self.open {
			// a window or a backoff ended with no check: record the transition
			self.update()
		}
		changed := self.changed
		wake := time.Second
		if now.Before(self.backoffUntil) {
			wake = min(wake, self.backoffUntil.Sub(now))
		}
		self.lock.Unlock()
		if open {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		case <-time.After(wake):
		}
	}
}

// A failed storage request: every worker waits out a backoff, and the health
// gates are checked again at once. The backoff doubles with each failure
// episode inside the reset window: requests in flight when MinIO stalls fail
// together, and a failure while a backoff runs belongs to its episode and
// does not lengthen it. Throttling starts at a longer wait.
func (self *GateKeeper) Failed(err error, throttled bool) {
	self.lock.Lock()
	defer self.lock.Unlock()
	now := self.now()
	self.failureCause = err.Error()
	if 300 < len(self.failureCause) {
		self.failureCause = self.failureCause[:300] + "..."
	}
	defer self.update()
	defer self.kickHealth()
	if now.Before(self.backoffUntil) {
		self.lastFailure = now
		return
	}
	// quiet since the last backoff ended
	if now.Sub(self.backoffUntil) > self.backoffReset {
		self.backoff = 0
	}
	switch {
	case self.backoff == 0 && throttled:
		self.backoff = min(4*self.minBackoff, self.maxBackoff)
	case self.backoff == 0:
		self.backoff = self.minBackoff
	default:
		self.backoff = min(2*self.backoff, self.maxBackoff)
	}
	self.lastFailure = now
	self.backoffUntil = now.Add(self.backoff)
}

// Must hold lock. Checks the health gates again at once.
func (self *GateKeeper) kickHealth() {
	for index, gate := range self.gates {
		if _, health := gate.(*MinioHealthGate); health {
			select {
			case self.kicks[index] <- struct{}{}:
			default:
			}
		}
	}
}

// A successful object: a quiet period resets the backoff.
func (self *GateKeeper) Succeeded() {
	self.lock.Lock()
	defer self.lock.Unlock()
	if self.backoff != 0 && self.now().Sub(self.backoffUntil) > self.backoffReset {
		self.backoff = 0
	}
}

// Total time the gates have been closed.
func (self *GateKeeper) Paused() time.Duration {
	self.lock.Lock()
	defer self.lock.Unlock()
	paused := self.paused
	if !self.open {
		paused += self.now().Sub(self.closedSince)
	}
	return paused
}

// The SN25 validator's steering: the first native-epoch boundary the gate
// counts from, the epoch that boundary ends, and the epoch's length.
// Matches the release gates (factury validator-deploy-gate, temp/snow-gate.sh).
const (
	nativeEpochOriginBlock = 9423802
	nativeEpochOrigin      = 26111
	nativeEpochBlocks      = 360
	blockTime              = 12 * time.Second
)

// The native epoch the chain is in at head, and the time to its boundary.
func NativeEpoch(head uint64) (epoch uint64, toBoundary time.Duration) {
	if head < nativeEpochOriginBlock {
		return 0, 0
	}
	since := head - nativeEpochOriginBlock
	epoch = nativeEpochOrigin + (since+nativeEpochBlocks-1)/nativeEpochBlocks
	blocksLeft := (nativeEpochBlocks - since%nativeEpochBlocks) % nativeEpochBlocks
	return epoch, time.Duration(blocksLeft) * blockTime
}

// The validator's published state: progress.json's intent.
type ValidatorProgress struct {
	NativeEpoch uint64
	Status      string
}

type ProgressReader interface {
	ReadProgress(ctx context.Context) (ValidatorProgress, error)
}

type HeadReader interface {
	BlockNumber(ctx context.Context) (uint64, error)
}

// Open only while the validator is idle: its intent for the chain's current
// native epoch is finalized or applied, and at least MinRemaining is left to
// the next boundary. Open state carries the time it closes by itself, so the
// mover stops at the margin even between checks. Any unreadable input keeps
// it closed.
type ValidatorGate struct {
	Progress     ProgressReader
	Head         HeadReader
	MinRemaining time.Duration
	Every        time.Duration
	Now          func() time.Time
}

func (self *ValidatorGate) Name() string { return "validator" }

func (self *ValidatorGate) Interval() time.Duration {
	if self.Every <= 0 {
		return 30 * time.Second
	}
	return self.Every
}

func (self *ValidatorGate) Check(ctx context.Context) GateState {
	now := time.Now
	if self.Now != nil {
		now = self.Now
	}
	head, err := self.Head.BlockNumber(ctx)
	if err != nil {
		return GateState{Reason: fmt.Sprintf("chain head unreadable: %v", err)}
	}
	read := now()
	epoch, left := NativeEpoch(head)
	progress, err := self.Progress.ReadProgress(ctx)
	if err != nil {
		return GateState{Reason: fmt.Sprintf("progress unreadable: %v", err)}
	}
	if progress.NativeEpoch != epoch || (progress.Status != "finalized" && progress.Status != "applied") {
		return GateState{Reason: fmt.Sprintf("intent %d %s is not finalized for epoch %d (boundary in %s)", progress.NativeEpoch, progress.Status, epoch, left)}
	}
	if left < self.MinRemaining {
		return GateState{Reason: fmt.Sprintf("only %s to the epoch %d boundary (need %s)", left, epoch, self.MinRemaining)}
	}
	return GateState{
		Open:   true,
		Reason: fmt.Sprintf("intent %d %s, boundary in %s", progress.NativeEpoch, progress.Status, left),
		Until:  read.Add(left - self.MinRemaining),
	}
}

// Reads progress.json on the validator host over ssh, as the release gates
// do: a read of one file, with no change on the host.
type SSHProgressReader struct {
	// user@host
	Target string
	Path   string
}

func (self *SSHProgressReader) ReadProgress(ctx context.Context) (ValidatorProgress, error) {
	readCtx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	command := exec.CommandContext(readCtx, "ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=15", self.Target, "cat", self.Path)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return ValidatorProgress{}, fmt.Errorf("ssh %s cat %s: %w: %s", self.Target, self.Path, err, strings.TrimSpace(stderr.String()))
	}
	return ParseValidatorProgress(stdout.Bytes())
}

// progress.json's intent.value.native_epoch and intent.value.status.
func ParseValidatorProgress(data []byte) (ValidatorProgress, error) {
	var progress struct {
		Intent struct {
			Value struct {
				NativeEpoch *uint64 `json:"native_epoch"`
				Status      string  `json:"status"`
			} `json:"value"`
		} `json:"intent"`
	}
	if err := json.Unmarshal(data, &progress); err != nil {
		return ValidatorProgress{}, fmt.Errorf("progress is not JSON: %w", err)
	}
	if progress.Intent.Value.NativeEpoch == nil || progress.Intent.Value.Status == "" {
		return ValidatorProgress{}, errors.New("progress has no intent epoch and status")
	}
	return ValidatorProgress{NativeEpoch: *progress.Intent.Value.NativeEpoch, Status: progress.Intent.Value.Status}, nil
}

// The chain head from an EVM JSON-RPC endpoint (eth_blockNumber), as `cast
// block-number` reads it.
type RpcHeadReader struct {
	Url    string
	Client *http.Client
}

func (self *RpcHeadReader) BlockNumber(ctx context.Context) (uint64, error) {
	readCtx, cancel := context.WithTimeout(ctx, gateRequestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(readCtx, http.MethodPost, self.Url, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`))
	if err != nil {
		return 0, err
	}
	request.Header.Set("Content-Type", "application/json")
	client := self.Client
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 64*1024))
	if err != nil {
		return 0, err
	}
	if response.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("eth_blockNumber answered %d", response.StatusCode)
	}
	var answer struct {
		Result string `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		return 0, err
	}
	if answer.Error != nil {
		return 0, fmt.Errorf("eth_blockNumber: %s", answer.Error.Message)
	}
	return strconv.ParseUint(strings.TrimPrefix(answer.Result, "0x"), 16, 64)
}

// Open while MinIO's cluster health route answers 200 within MaxLatency. Each
// answer's latency is recorded.
type MinioHealthGate struct {
	Url        string
	MaxLatency time.Duration
	Every      time.Duration
	Client     *http.Client
	Latency    *Latencies
}

func (self *MinioHealthGate) Name() string { return "minio" }

func (self *MinioHealthGate) Interval() time.Duration {
	if self.Every <= 0 {
		return 5 * time.Second
	}
	return self.Every
}

func (self *MinioHealthGate) Check(ctx context.Context) GateState {
	readCtx, cancel := context.WithTimeout(ctx, gateRequestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(readCtx, http.MethodGet, self.Url, nil)
	if err != nil {
		return GateState{Reason: err.Error()}
	}
	client := self.Client
	if client == nil {
		client = http.DefaultClient
	}
	start := time.Now()
	response, err := client.Do(request)
	if err != nil {
		return GateState{Reason: fmt.Sprintf("cluster health unreadable: %v", err)}
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64*1024))
	response.Body.Close()
	elapsed := time.Since(start)
	if self.Latency != nil {
		self.Latency.Observe("health", elapsed)
	}
	if response.StatusCode != http.StatusOK {
		return GateState{Reason: fmt.Sprintf("cluster health answered %d in %s", response.StatusCode, elapsed.Round(time.Millisecond))}
	}
	if self.MaxLatency < elapsed {
		return GateState{Reason: fmt.Sprintf("cluster health took %s (more than %s)", elapsed.Round(time.Millisecond), self.MaxLatency)}
	}
	return GateState{Open: true, Reason: fmt.Sprintf("cluster health %s", elapsed.Round(time.Millisecond))}
}

// The validator host, its progress file and the chain endpoint the release
// gates read (temp/snow-gate.sh). Each is read only.
const (
	DefaultValidatorProgressTarget = "by@172.28.208.185"
	DefaultValidatorProgressPath   = "/var/lib/sn-validator-progress/progress.json"
	DefaultChainRpcUrl             = "https://lite.chain.opentensor.ai"
	// the margin before a native-epoch boundary the validator's next pass
	// begins at
	DefaultValidatorMinRemaining = 15 * time.Minute
	// the slowest cluster health answer the mover runs under
	DefaultMinioHealthMaxLatency = time.Second
)

// The production gates: the validator idle, and MinIO healthy within a
// second at the store's own server.
func DefaultGates(healthUrl string, latency *Latencies) []Gate {
	return []Gate{
		&ValidatorGate{
			Progress:     &SSHProgressReader{Target: DefaultValidatorProgressTarget, Path: DefaultValidatorProgressPath},
			Head:         &RpcHeadReader{Url: DefaultChainRpcUrl},
			MinRemaining: DefaultValidatorMinRemaining,
		},
		&MinioHealthGate{Url: healthUrl, MaxLatency: DefaultMinioHealthMaxLatency, Latency: latency},
	}
}
