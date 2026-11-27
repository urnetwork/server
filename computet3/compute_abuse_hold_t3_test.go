//go:build sidecar_pending

package computet3

// §17.2's operator hold on the private stores (CARCH-M12-01; D-CABUSE-1, D-CABUSE-2): sidecar/controller's suspend and
// resume over compute_abuse_hold, CARCH's part of MG-C1, in PostgreSQL and its projection in Redis, through server's own
// adapters.

import (
	"context"
	"sync"
	"testing"

	operator "github.com/urnetwork/operator-core"
	"github.com/urnetwork/sidecar"
	sidecarcontroller "github.com/urnetwork/sidecar/controller"
	"github.com/urnetwork/sidecar/model"
)

// ENC's listener as the tests stand it in: each network's stops counted, and a network set failing fails.
type computeT3AbuseHoldEnclaves struct {
	stateLock sync.Mutex
	held      map[operator.Id]int
	failing   map[operator.Id]bool
}

func (self *computeT3AbuseHoldEnclaves) Held(env operator.Env, ctx context.Context, networkId operator.Id) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.held[networkId] += 1
	if self.failing[networkId] {
		return context.DeadlineExceeded
	}
	return nil
}

func (self *computeT3AbuseHoldEnclaves) count(networkId operator.Id) int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.held[networkId]
}

func (self *computeT3AbuseHoldEnclaves) fail(networkId operator.Id) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.failing[networkId] = true
}

var (
	computeT3AbuseHoldOnce     sync.Once
	computeT3AbuseHoldListener = &computeT3AbuseHoldEnclaves{held: map[operator.Id]int{}, failing: map[operator.Id]bool{}}
)

// The listener registered, once for the process.
func registerComputeT3AbuseHoldListener() {
	computeT3AbuseHoldOnce.Do(func() {
		sidecarcontroller.RegisterComputeAbuseHoldListener(computeT3AbuseHoldListener)
	})
}

// One act, failing the test on its error.
func computeT3AbuseHoldAct(t3 *computeT3, act func(env operator.Env, ctx context.Context, settings *model.ComputeAbuseHoldSettings, request *sidecarcontroller.ComputeAbuseHoldRequest) (*sidecarcontroller.ComputeAbuseHoldAnswer, error), request *sidecarcontroller.ComputeAbuseHoldRequest) *sidecarcontroller.ComputeAbuseHoldAnswer {
	t3.t.Helper()
	answer, err := act(t3.env, t3.ctx, model.DefaultComputeAbuseHoldSettings(), request)
	if err != nil {
		t3.t.Fatal(err)
	}
	return answer
}

// The network's projection's value, "" for none.
func computeT3AbuseHoldProjection(t3 *computeT3, networkId operator.Id) string {
	t3.t.Helper()
	value, found, err := t3.redis.Get(t3.ctx, model.ComputeAbuseHoldRedisKey(networkId))
	if err != nil {
		t3.t.Fatal(err)
	}
	if !found {
		return ""
	}
	return string(value)
}

// A suspension commits the hold and projects it; a retry for the same reason changes nothing; another reason changes
// the class and keeps when the hold began; a resume ends it and restarts nothing, a second one changing nothing; a
// suspension after the resume begins a new hold. Every act projects its version, the ledger is never touched, and
// what the broker's store reads is the row.
func TestComputeAbuseHoldSuspendAndResumeOnTheStores(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		registerComputeT3AbuseHoldListener()
		networkId := operator.Id(sidecar.NewId())
		staffId := operator.Id(sidecar.NewId())
		otherStaffId := operator.Id(sidecar.NewId())

		suspended := computeT3AbuseHoldAct(t3, sidecarcontroller.ComputeAbuseHoldSuspend, &sidecarcontroller.ComputeAbuseHoldRequest{NetworkId: networkId, Reason: model.ComputeAbuseHoldReasonMining, StaffId: staffId})
		hold := suspended.Hold
		if !suspended.Changed || !suspended.Projected || suspended.Enclaves != sidecarcontroller.ComputeAbuseHoldEnclavesStopped || !hold.Held() ||
			hold.Version != 1 || hold.Reason != model.ComputeAbuseHoldReasonMining || hold.HoldStaffId != staffId {
			t3.t.Fatalf("the suspension answered %+v %+v", suspended, hold)
		}
		if value := computeT3AbuseHoldProjection(t3, networkId); value != "1:1" {
			t3.t.Fatalf("the suspension projected %q", value)
		}
		if count := computeT3AbuseHoldListener.count(networkId); count != 1 {
			t3.t.Fatalf("the enclaves were stopped %d times", count)
		}
		holdTime := hold.HoldTime

		retried := computeT3AbuseHoldAct(t3, sidecarcontroller.ComputeAbuseHoldSuspend, &sidecarcontroller.ComputeAbuseHoldRequest{NetworkId: networkId, Reason: model.ComputeAbuseHoldReasonMining, StaffId: otherStaffId})
		if retried.Changed || retried.Hold.Version != 1 || retried.Hold.HoldStaffId != staffId || !retried.Projected {
			t3.t.Fatalf("a retry changed the hold: %+v %+v", retried, retried.Hold)
		}
		// a retry asks the listener again, so a stop that failed is made good
		if count := computeT3AbuseHoldListener.count(networkId); count != 2 {
			t3.t.Fatalf("a retry stopped the enclaves %d times in all", count)
		}

		changed := computeT3AbuseHoldAct(t3, sidecarcontroller.ComputeAbuseHoldSuspend, &sidecarcontroller.ComputeAbuseHoldRequest{NetworkId: networkId, Reason: model.ComputeAbuseHoldReasonSpam, StaffId: otherStaffId})
		if !changed.Changed || changed.Hold.Version != 2 || changed.Hold.Reason != model.ComputeAbuseHoldReasonSpam || changed.Hold.HoldStaffId != otherStaffId ||
			!changed.Hold.HoldTime.Equal(holdTime) {
			t3.t.Fatalf("another reason answered %+v", changed.Hold)
		}
		if value := computeT3AbuseHoldProjection(t3, networkId); value != "2:3" {
			t3.t.Fatalf("another reason projected %q", value)
		}

		resumed := computeT3AbuseHoldAct(t3, sidecarcontroller.ComputeAbuseHoldResume, &sidecarcontroller.ComputeAbuseHoldRequest{NetworkId: networkId, StaffId: staffId})
		if !resumed.Changed || resumed.Hold.Held() || resumed.Hold.Version != 3 || resumed.Hold.ResumeStaffId == nil || *resumed.Hold.ResumeStaffId != staffId ||
			resumed.Enclaves != sidecarcontroller.ComputeAbuseHoldEnclavesUntouched {
			t3.t.Fatalf("the resume answered %+v %+v", resumed, resumed.Hold)
		}
		if value := computeT3AbuseHoldProjection(t3, networkId); value != "3:0" {
			t3.t.Fatalf("the resume projected %q", value)
		}
		if count := computeT3AbuseHoldListener.count(networkId); count != 3 {
			t3.t.Fatalf("a resume asked the listener: %d stops in all", count)
		}
		again := computeT3AbuseHoldAct(t3, sidecarcontroller.ComputeAbuseHoldResume, &sidecarcontroller.ComputeAbuseHoldRequest{NetworkId: networkId, StaffId: otherStaffId})
		if again.Changed || again.Hold.Version != 3 || *again.Hold.ResumeStaffId != staffId {
			t3.t.Fatalf("a second resume changed the row: %+v", again.Hold)
		}

		// a hold placed after a resume is a new hold, begun now
		renewed := computeT3AbuseHoldAct(t3, sidecarcontroller.ComputeAbuseHoldSuspend, &sidecarcontroller.ComputeAbuseHoldRequest{NetworkId: networkId, Reason: model.ComputeAbuseHoldReasonTakedown, StaffId: staffId})
		if !renewed.Changed || renewed.Hold.Version != 4 || !renewed.Hold.Held() || renewed.Hold.ResumeTime != nil || renewed.Hold.HoldTime.Before(holdTime) {
			t3.t.Fatalf("a hold after a resume answered %+v", renewed.Hold)
		}

		// the row is what the broker's store reads, and the ledger never moved
		read, err := model.ComputeAbuseHoldGet(t3.env, t3.ctx, networkId)
		if err != nil || read == nil || read.Version != 4 || read.Reason != model.ComputeAbuseHoldReasonTakedown || !read.Held() {
			t3.t.Fatalf("the store read %+v %v", read, err)
		}
		for _, table := range []string{"compute_account", "compute_lot", "compute_op", "compute_charge"} {
			if count := t3.queryInt(`SELECT count(*) FROM `+table+` WHERE network_id = $1`, networkId); count != 0 {
				t3.t.Errorf("the hold wrote %d rows of %s", count, table)
			}
		}
	})
}

// A resume of a network never held writes no row and projects it clear, version 0; a hold the projection never took,
// its key lost after the commit, is read again from the row by the next fill; a listener that fails is said, and a
// retry asks it again.
func TestComputeAbuseHoldEdgesOnTheStores(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		registerComputeT3AbuseHoldListener()
		networkId := operator.Id(sidecar.NewId())
		staffId := operator.Id(sidecar.NewId())

		never := computeT3AbuseHoldAct(t3, sidecarcontroller.ComputeAbuseHoldResume, &sidecarcontroller.ComputeAbuseHoldRequest{NetworkId: networkId, StaffId: staffId})
		if never.Changed || never.Hold != nil || !never.Projected {
			t3.t.Fatalf("a resume of a network never held answered %+v", never)
		}
		if count := t3.queryInt(`SELECT count(*) FROM compute_abuse_hold WHERE network_id = $1`, networkId); count != 0 {
			t3.t.Fatalf("a resume of a network never held wrote %d rows", count)
		}
		if value := computeT3AbuseHoldProjection(t3, networkId); value != "0:0" {
			t3.t.Fatalf("a network never held projected %q", value)
		}

		computeT3AbuseHoldListener.fail(networkId)
		failed := computeT3AbuseHoldAct(t3, sidecarcontroller.ComputeAbuseHoldSuspend, &sidecarcontroller.ComputeAbuseHoldRequest{NetworkId: networkId, Reason: model.ComputeAbuseHoldReasonFraud, StaffId: staffId})
		if !failed.Changed || failed.Enclaves != sidecarcontroller.ComputeAbuseHoldEnclavesFailed || !failed.Hold.Held() {
			t3.t.Fatalf("a failing listener answered %+v", failed)
		}
		// the version-0 projection of the network never held was replaced by the hold's
		if value := computeT3AbuseHoldProjection(t3, networkId); value != "1:4" {
			t3.t.Fatalf("the hold projected %q over the clear one", value)
		}

		// the projection lost after the commit: a fill reads the row and projects it again, never an older version
		if err := t3.redis.Delete(t3.ctx, []string{model.ComputeAbuseHoldRedisKey(networkId)}); err != nil {
			t3.t.Fatal(err)
		}
		hold, err := model.ComputeAbuseHoldGet(t3.env, t3.ctx, networkId)
		if err != nil {
			t3.t.Fatal(err)
		}
		settings := model.DefaultComputeAbuseHoldSettings()
		if projected, err := model.ComputeAbuseHoldProject(t3.ctx, t3.redis, settings, networkId, model.ComputeAbuseHoldStateOf(hold)); err != nil || !projected {
			t3.t.Fatalf("the fill: %v %v", projected, err)
		}
		if projected, err := model.ComputeAbuseHoldProject(t3.ctx, t3.redis, settings, networkId, &model.ComputeAbuseHoldState{}); err != nil || !projected {
			t3.t.Fatalf("an older fill: %v %v", projected, err)
		}
		if value := computeT3AbuseHoldProjection(t3, networkId); value != "1:4" {
			t3.t.Fatalf("an older fill replaced the hold: %q", value)
		}
	})
}
