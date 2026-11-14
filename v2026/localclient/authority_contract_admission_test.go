// The owner's contract creation bound refuses a pack that only creates
// contracts before any credential or database read, holds an admitted
// creation's slot until its control returns, reads as a rate limit, and never
// refuses control that closes or settles state. Every identity here is
// synthetic and no database is needed.
package localclient

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/server/v2026"
)

// An authority with no credential: any call that gets past admission fails
// its local preflight with a 401 and never reaches the database.
func contractAdmissionTestAuthority(admission *ContractAdmission) *Authority {
	return &Authority{
		networkId:         server.NewId(),
		userId:            server.NewId(),
		clientId:          server.NewId(),
		deviceId:          server.NewId(),
		apiUrl:            "https://control.example",
		contractAdmission: admission,
		cancel:            func() {},
	}
}

// One control pack of the messages, in order. The test owns the frames'
// pooled bytes until it has joined its calls.
func contractAdmissionTestArgs(t testing.TB, messages ...proto.Message) *connect.ConnectControlArgs {
	t.Helper()
	frames := make([]*protocol.Frame, 0, len(messages))
	for _, message := range messages {
		frame, err := connect.ToFrame(message, connect.DefaultProtocolVersion)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { connect.MessagePoolReturn(frame.MessageBytes) })
		frames = append(frames, frame)
	}
	pack, err := proto.Marshal(&protocol.Pack{Frames: frames})
	if err != nil {
		t.Fatal(err)
	}
	return &connect.ConnectControlArgs{Pack: base64.StdEncoding.EncodeToString(pack)}
}

func contractAdmissionTestCreate() *protocol.CreateContract {
	return &protocol.CreateContract{DestinationId: server.NewId().Bytes(), TransferByteCount: 1024}
}

func contractAdmissionExpectStatus(t testing.TB, err error, want int, what string) {
	t.Helper()
	var status *connect.HttpStatusError
	if !errors.As(err, &status) {
		t.Fatalf("%s answered %v, which carries no http status; want %d", what, err, want)
	}
	if status.StatusCode != want {
		t.Fatalf("%s answered %d %q, want %d", what, status.StatusCode, status.Body, want)
	}
}

func contractAdmissionReceive[T any](t testing.TB, values <-chan T, what string) T {
	t.Helper()
	select {
	case value := <-values:
		return value
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		panic("unreachable")
	}
}

// A full bound refuses a creation at once as a 429 and counts it; a free slot
// admits the call to its preflight, and the slot is returned when the call
// ends.
func TestAuthorityContractAdmissionRefusesPastBound(t *testing.T) {
	admission := NewContractAdmission(1)
	authority := contractAdmissionTestAuthority(admission)
	args := contractAdmissionTestArgs(t, contractAdmissionTestCreate())
	if !admission.tryAcquire() {
		t.Fatal("a fresh bound refused its first slot")
	}
	refusals := testutil.ToFloat64(contractAdmissionRefusals)
	_, err := authority.ConnectControl(t.Context(), "", args)
	contractAdmissionExpectStatus(t, err, http.StatusTooManyRequests, "a creation with the bound full")
	if counted := testutil.ToFloat64(contractAdmissionRefusals) - refusals; counted != 1 {
		t.Fatalf("one refusal was counted %v times", counted)
	}
	admission.release()

	// admitted: the empty credential fails preflight, not admission
	_, err = authority.ConnectControl(t.Context(), "", args)
	contractAdmissionExpectStatus(t, err, http.StatusUnauthorized, "an admitted creation")
	if !admission.tryAcquire() {
		t.Fatal("an ended call kept its slot")
	}
	admission.release()
}

// An admitted creation holds its slot for its whole control call. With every
// slot held by a call in progress the next creation is refused, and once they
// return a creation is admitted again. A pack of several creations takes one
// slot.
func TestAuthorityContractAdmissionHoldsSlotsUntilControlReturns(t *testing.T) {
	const bound = 3
	admission := NewContractAdmission(bound)
	authority := contractAdmissionTestAuthority(admission)
	args := contractAdmissionTestArgs(t, contractAdmissionTestCreate(), contractAdmissionTestCreate())
	entered := make(chan struct{})
	release := make(chan struct{})
	// a failed assertion still releases the held calls
	releaseHeld := sync.OnceFunc(func() { close(release) })
	defer releaseHeld()
	var admitted atomic.Int32
	authority.afterContractAdmissionForTest = func() {
		if admitted.Add(1) <= bound {
			entered <- struct{}{}
			<-release
		}
	}
	results := make(chan error, bound)
	for range bound {
		go func() {
			_, err := authority.ConnectControl(context.Background(), "", args)
			results <- err
		}()
		contractAdmissionReceive(t, entered, "an admitted creation")
	}

	_, err := authority.ConnectControl(t.Context(), "", args)
	contractAdmissionExpectStatus(t, err, http.StatusTooManyRequests, "a creation with every slot held")

	releaseHeld()
	for range bound {
		err := contractAdmissionReceive(t, results, "a held creation to return")
		contractAdmissionExpectStatus(t, err, http.StatusUnauthorized, "a held creation")
	}
	_, err = authority.ConnectControl(t.Context(), "", args)
	contractAdmissionExpectStatus(t, err, http.StatusUnauthorized, "a creation after the held ones returned")
	for range bound {
		if !admission.tryAcquire() {
			t.Fatal("a returned creation kept its slot")
		}
	}
	if admission.tryAcquire() {
		t.Fatal("the bound admitted past its capacity")
	}
}

// Control that is not purely contract creation is never refused, even with the
// bound full: a close, a ping, a provide, a creation packed with a close, an
// empty pack and a pack that does not decode all reach their preflight.
func TestAuthorityContractAdmissionNeverRefusesOtherControl(t *testing.T) {
	admission := NewContractAdmission(1)
	authority := contractAdmissionTestAuthority(admission)
	if !admission.tryAcquire() {
		t.Fatal("a fresh bound refused its first slot")
	}
	defer admission.release()
	closeContract := &protocol.CloseContract{ContractId: server.NewId().Bytes(), AckedByteCount: 1024}
	for _, c := range []struct {
		name string
		args *connect.ConnectControlArgs
	}{
		{name: "close", args: contractAdmissionTestArgs(t, closeContract)},
		{name: "ping", args: contractAdmissionTestArgs(t, &protocol.ControlPing{})},
		{name: "provide", args: contractAdmissionTestArgs(t, &protocol.Provide{})},
		{name: "create then close", args: contractAdmissionTestArgs(t, contractAdmissionTestCreate(), closeContract)},
		{name: "close then create", args: contractAdmissionTestArgs(t, closeContract, contractAdmissionTestCreate())},
		{name: "empty", args: contractAdmissionTestArgs(t)},
		{name: "undecodable", args: &connect.ConnectControlArgs{Pack: "not a pack"}},
	} {
		_, err := authority.ConnectControl(t.Context(), "", c.args)
		contractAdmissionExpectStatus(t, err, http.StatusUnauthorized, c.name+" with the bound full")
	}
}

// Concurrent creations never hold more slots than the bound, and every
// admitted call returns its slot.
func TestAuthorityContractAdmissionConcurrentCallsReturnSlots(t *testing.T) {
	const bound = 3
	admission := NewContractAdmission(bound)
	authority := contractAdmissionTestAuthority(admission)
	args := contractAdmissionTestArgs(t, contractAdmissionTestCreate())
	var wait sync.WaitGroup
	for range 64 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, err := authority.ConnectControl(context.Background(), "", args)
			var status *connect.HttpStatusError
			if !errors.As(err, &status) || status.StatusCode != http.StatusUnauthorized && status.StatusCode != http.StatusTooManyRequests {
				t.Errorf("unexpected result %v", err)
			}
		}()
	}
	wait.Wait()
	for range bound {
		if !admission.tryAcquire() {
			t.Fatal("a slot leaked after concurrent calls")
		}
	}
	if admission.tryAcquire() {
		t.Fatal("the bound admitted past its capacity")
	}
}

// No admission admits everything, and nonpositive capacities build none.
func TestAuthorityContractAdmissionNilAdmitsEverything(t *testing.T) {
	if NewContractAdmission(0) != nil || NewContractAdmission(-1) != nil {
		t.Fatal("a nonpositive bound built an admission")
	}
	authority := contractAdmissionTestAuthority(nil)
	args := contractAdmissionTestArgs(t, contractAdmissionTestCreate())
	for range 4 {
		_, err := authority.ConnectControl(t.Context(), "", args)
		contractAdmissionExpectStatus(t, err, http.StatusUnauthorized, "a creation of an unbounded owner")
	}
}

// A closed authority and an ended request report their lifecycle, not a rate
// limit, and neither takes a slot or counts a refusal.
func TestAuthorityContractAdmissionLifecycleBeforeRefusal(t *testing.T) {
	admission := NewContractAdmission(1)
	authority := contractAdmissionTestAuthority(admission)
	args := contractAdmissionTestArgs(t, contractAdmissionTestCreate())
	if !admission.tryAcquire() {
		t.Fatal("a fresh bound refused its first slot")
	}
	refusals := testutil.ToFloat64(contractAdmissionRefusals)
	inUse := contractAdmissionGauge(t, contractAdmissionInUseName)
	ended, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := authority.ConnectControl(ended, "", args); !errors.Is(err, context.Canceled) {
		t.Fatalf("an ended request answered %v, want canceled", err)
	}
	authority.closed.Store(true)
	if _, err := authority.ConnectControl(t.Context(), "", args); !errors.Is(err, context.Canceled) {
		t.Fatalf("a closed authority answered %v, want canceled", err)
	}
	if counted := testutil.ToFloat64(contractAdmissionRefusals) - refusals; counted != 0 {
		t.Fatalf("lifecycle answers counted %v refusals", counted)
	}
	if held := contractAdmissionGauge(t, contractAdmissionInUseName) - inUse; held != 0 {
		t.Fatalf("lifecycle answers moved the slots in use by %v", held)
	}
	admission.release()
	if !admission.tryAcquire() {
		t.Fatal("a lifecycle answer took a slot")
	}
	admission.release()
}

// Counts the in-process control calls a device's contract manager makes and
// publishes each result.
type contractAdmissionCountingControl struct {
	control connect.NetworkClientControl
	calls   atomic.Int64
	results chan error
}

func (self *contractAdmissionCountingControl) ConnectControl(ctx context.Context, token string, args *connect.ConnectControlArgs) (*connect.ConnectControlResult, error) {
	result, err := self.control.ConnectControl(ctx, token, args)
	self.calls.Add(1)
	self.results <- err
	return result, err
}

// Answers every control request with an empty reply, a successful round trip.
type contractAdmissionEmptyReplyControl struct{}

func (contractAdmissionEmptyReplyControl) ConnectControl(context.Context, string, *connect.ConnectControlArgs) (*connect.ConnectControlResult, error) {
	return &connect.ConnectControlResult{}, nil
}

// A hosted device's contract manager, sending its control through `control`
// the way a hosted window does.
func contractAdmissionTestClient(t testing.TB, control connect.NetworkClientControl) *connect.Client {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	settings := connect.DefaultClientStrategySettings()
	settings.EnableResilient = false
	strategy := connect.NewClientStrategy(ctx, settings)
	oob := connect.NewApiOutOfBandControlWithLocalControl(ctx, strategy, "", "https://control.example", control)
	client := connect.NewClient(ctx, connect.NewId(), oob, connect.DefaultClientSettings())
	t.Cleanup(func() {
		client.Cancel()
		_ = oob.CloseAndWait(context.Background())
		strategy.Close()
		cancel()
	})
	return client
}

// The device side of a refusal, through the real in-process control path and
// contract manager: one requested contract is one refused round trip, the
// manager does not ask again by itself, and a sequence waiting for the contract
// is not woken by the refusal, so its next request waits out its own retry
// interval (connect's TestCreateContractRetryBackoff: 1 s doubling to 5 s).
func TestAuthorityContractAdmissionRefusalIsOneFailedRoundTripForTheDevice(t *testing.T) {
	admission := NewContractAdmission(1)
	if !admission.tryAcquire() {
		t.Fatal("a fresh bound refused its first slot")
	}
	defer admission.release()
	control := &contractAdmissionCountingControl{
		control: contractAdmissionTestAuthority(admission),
		results: make(chan error, 16),
	}
	contractManager := contractAdmissionTestClient(t, control).ContractManager()
	// A refusal counts toward the process-wide degraded-backend streak, which
	// other tests in this binary read. One successful round trip clears it.
	t.Cleanup(func() {
		recovery := contractAdmissionTestClient(t, contractAdmissionEmptyReplyControl{}).ContractManager()
		recovery.CreateContract(connect.ContractKey{Destination: connect.DestinationId(connect.NewId())}, 0, 1024)
	})

	key := connect.ContractKey{Destination: connect.DestinationId(connect.NewId())}
	const wait = time.Second
	taken := make(chan *protocol.Contract, 1)
	takeStart := time.Now()
	go func() {
		taken <- contractManager.TakeContract(t.Context(), key, wait)
	}()
	// the take is waiting when the refusal lands
	time.Sleep(100 * time.Millisecond)
	contractManager.CreateContract(key, 0, 1024)
	err := contractAdmissionReceive(t, control.results, "the contract request")
	contractAdmissionExpectStatus(t, err, http.StatusTooManyRequests, "the device's contract request with the bound full")
	if contract := contractAdmissionReceive(t, taken, "the waiting take"); contract != nil {
		t.Fatal("a refused request delivered a contract")
	}
	if elapsed := time.Since(takeStart); elapsed < wait-50*time.Millisecond {
		t.Fatalf("the refusal woke the waiting take after %v, before its %v wait", elapsed, wait)
	}
	select {
	case err := <-control.results:
		t.Fatalf("the manager asked again by itself (%v)", err)
	case <-time.After(250 * time.Millisecond):
	}
	if calls := control.calls.Load(); calls != 1 {
		t.Fatalf("one requested contract made %d control calls", calls)
	}
}

const contractAdmissionInUseName = "urnetwork_hosted_contract_admission_in_use"
const contractAdmissionCapacityName = "urnetwork_hosted_contract_admission_capacity"

// A process-wide hosted admission gauge, read from the default registry by the
// name the proxy dashboard queries.
func contractAdmissionGauge(t testing.TB, name string) float64 {
	t.Helper()
	// a collector that fails to gather still leaves every other family
	families, err := prometheus.DefaultGatherer.Gather()
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		metrics := family.GetMetric()
		if len(metrics) != 1 || metrics[0].GetGauge() == nil {
			t.Fatalf("%s is not a single gauge", name)
		}
		return metrics[0].GetGauge().GetValue()
	}
	t.Fatalf("%s is not exported (gather: %v)", name, err)
	return 0
}

// A slot counts in use from its acquire to its release and a refused acquire
// counts none. The bound's slots count toward the process capacity until it is
// closed, once.
func TestContractAdmissionGaugesCountSlotsAndCapacity(t *testing.T) {
	inUse := contractAdmissionGauge(t, contractAdmissionInUseName)
	capacity := contractAdmissionGauge(t, contractAdmissionCapacityName)
	expect := func(what string, wantInUse float64, wantCapacity float64) {
		t.Helper()
		if got := contractAdmissionGauge(t, contractAdmissionInUseName) - inUse; got != wantInUse {
			t.Fatalf("%s: %v slots in use, want %v", what, got, wantInUse)
		}
		if got := contractAdmissionGauge(t, contractAdmissionCapacityName) - capacity; got != wantCapacity {
			t.Fatalf("%s: capacity %v, want %v", what, got, wantCapacity)
		}
	}
	admission := NewContractAdmission(2)
	defer admission.Close()
	expect("a new bound", 0, 2)
	if !admission.tryAcquire() || !admission.tryAcquire() {
		t.Fatal("a fresh bound of two refused a slot")
	}
	expect("both slots taken", 2, 2)
	refusals := testutil.ToFloat64(contractAdmissionRefusals)
	if admission.tryAcquire() {
		t.Fatal("a full bound admitted a third slot")
	}
	if counted := testutil.ToFloat64(contractAdmissionRefusals) - refusals; counted != 1 {
		t.Fatalf("one refusal was counted %v times", counted)
	}
	expect("a refused acquire", 2, 2)
	admission.release()
	expect("one slot released", 1, 2)
	admission.release()
	expect("both slots released", 0, 2)
	admission.Close()
	expect("a closed bound", 0, 0)
	admission.Close()
	expect("a bound closed twice", 0, 0)
}

// No bound has no slots: a nil admission counts nothing in use or in capacity,
// and closing it does nothing.
func TestContractAdmissionNilCountsNoSlots(t *testing.T) {
	inUse := contractAdmissionGauge(t, contractAdmissionInUseName)
	capacity := contractAdmissionGauge(t, contractAdmissionCapacityName)
	admission := NewContractAdmission(0)
	if admission != nil {
		t.Fatal("a nonpositive bound built an admission")
	}
	if !admission.tryAcquire() {
		t.Fatal("no bound refused a creation")
	}
	admission.release()
	admission.Close()
	if admission.Testing_Capacity() != 0 {
		t.Fatalf("no bound reports %d slots", admission.Testing_Capacity())
	}
	if got := contractAdmissionGauge(t, contractAdmissionInUseName) - inUse; got != 0 {
		t.Fatalf("no bound moved the slots in use by %v", got)
	}
	if got := contractAdmissionGauge(t, contractAdmissionCapacityName) - capacity; got != 0 {
		t.Fatalf("no bound moved the capacity by %v", got)
	}
}

// An admitted creation counts in use for its whole control call, up to its
// return; a creation refused meanwhile counts nothing.
func TestAuthorityContractAdmissionGaugeHoldsSlotForTheCall(t *testing.T) {
	admission := NewContractAdmission(1)
	defer admission.Close()
	authority := contractAdmissionTestAuthority(admission)
	args := contractAdmissionTestArgs(t, contractAdmissionTestCreate())
	inUse := contractAdmissionGauge(t, contractAdmissionInUseName)
	entered := make(chan struct{})
	release := make(chan struct{})
	// a failed assertion still releases the held call
	releaseHeld := sync.OnceFunc(func() { close(release) })
	defer releaseHeld()
	var admitted atomic.Int32
	authority.afterContractAdmissionForTest = func() {
		if admitted.Add(1) == 1 {
			entered <- struct{}{}
			<-release
		}
	}
	results := make(chan error, 1)
	go func() {
		_, err := authority.ConnectControl(context.Background(), "", args)
		results <- err
	}()
	contractAdmissionReceive(t, entered, "the admitted creation")
	if held := contractAdmissionGauge(t, contractAdmissionInUseName) - inUse; held != 1 {
		t.Fatalf("a creation inside its control call holds %v slots in use, want 1", held)
	}

	_, err := authority.ConnectControl(t.Context(), "", args)
	contractAdmissionExpectStatus(t, err, http.StatusTooManyRequests, "a creation with the slot held")
	if held := contractAdmissionGauge(t, contractAdmissionInUseName) - inUse; held != 1 {
		t.Fatalf("a refused creation left %v slots in use, want 1", held)
	}

	releaseHeld()
	err = contractAdmissionReceive(t, results, "the held creation to return")
	contractAdmissionExpectStatus(t, err, http.StatusUnauthorized, "the held creation")
	if held := contractAdmissionGauge(t, contractAdmissionInUseName) - inUse; held != 0 {
		t.Fatalf("a returned creation left %v slots in use", held)
	}
}

// A control call that panics after admission releases its slot as the panic
// unwinds, so the slots in use return to where they were and repeated panics
// never fill the bound.
func TestAuthorityContractAdmissionGaugeReleasesSlotOnPanic(t *testing.T) {
	const bound = 2
	admission := NewContractAdmission(bound)
	defer admission.Close()
	authority := contractAdmissionTestAuthority(admission)
	args := contractAdmissionTestArgs(t, contractAdmissionTestCreate())
	inUse := contractAdmissionGauge(t, contractAdmissionInUseName)
	raised := errors.New("controller raised after admission")
	var held float64
	authority.afterContractAdmissionForTest = func() {
		held = contractAdmissionGauge(t, contractAdmissionInUseName) - inUse
		panic(raised)
	}
	for range bound + 1 {
		held = -1
		_, err := authority.ConnectControl(t.Context(), "", args)
		if !errors.Is(err, raised) {
			t.Fatalf("a call that panicked after admission answered %v, want its raise", err)
		}
		if held != 1 {
			t.Fatalf("the panicking call held %v slots in use, want 1", held)
		}
		if left := contractAdmissionGauge(t, contractAdmissionInUseName) - inUse; left != 0 {
			t.Fatalf("a call that panicked left %v slots in use", left)
		}
	}
	for range bound {
		if !admission.tryAcquire() {
			t.Fatal("a call that panicked kept its slot")
		}
	}
	for range bound {
		admission.release()
	}
}
