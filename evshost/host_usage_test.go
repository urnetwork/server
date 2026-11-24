package evshost

// The block's usage producer runs on HostSettings.Usage, which T3's evs tier fills from the evs world's settings
// (fault.EvsTierDeps.Usage): a report held on its way to the Quota while the clock moves 26 h, as an evs row's Advance
// moves it, reaches the Quota on the world's settings, which bound no report, and on sidecar's defaults the move passes
// the report's 30 s bound and cancels it before the Quota counts it, the write the unknown rule's dry run counted then
// never made (sidecar's sidecartest/fault/evs_world_usage_test.go holds the same of T2's block). The report is sent by
// the producer's Drain, so nothing here waits on a timer: the bound is armed before the call reaches the Quota.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"
)

// A Quota that keeps the first ReportUsage on its way to the fake until Release, telling arrived when it has it and
// answered what the fake said, and passes every other call straight through.
type hostTestKeptQuota struct {
	sidecar.Quota
	keepOnce    sync.Once
	releaseOnce sync.Once
	arrived     chan struct{}
	release     chan struct{}
	answered    chan error
}

func newHostTestKeptQuota(quota sidecar.Quota) *hostTestKeptQuota {
	return &hostTestKeptQuota{
		Quota:    quota,
		arrived:  make(chan struct{}),
		release:  make(chan struct{}),
		answered: make(chan error, 1),
	}
}

func (self *hostTestKeptQuota) ReportUsage(ctx context.Context, batch *sidecar.UsageBatch) error {
	kept := false
	self.keepOnce.Do(func() {
		kept = true
	})
	if !kept {
		return self.Quota.ReportUsage(ctx, batch)
	}
	close(self.arrived)
	<-self.release
	err := self.Quota.ReportUsage(ctx, batch)
	self.answered <- err
	return err
}

// Lets the kept call go on to the fake. Safe more than once, so a test that fails first still frees the producer
// the block's Close joins.
func (self *hostTestKeptQuota) Release() {
	self.releaseOnce.Do(func() {
		close(self.release)
	})
}

// A block on usage, its producer holding 100 bytes, drained, its report kept on its way to the Quota while the clock
// moves 26 h and then let go: what the report answered, and how many batches reached the fake Quota.
func hostTestUsageReportInMove(t *testing.T, usage *sidecar.QuotaUsageBatcherSettings) (answer error, sentCount int) {
	t.Helper()
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	fakes := sidecartest.NewFakeHost(t.Context(), clock, nil, nil, nil)
	quota := newHostTestKeptQuota(fakes.Quota)
	host := fakes.ServiceHost()
	host.Quota = quota
	settings := DefaultHostSettings()
	settings.Usage = usage
	evsHost, err := NewHost(t.Context(), host, settings, &HostDeps{
		AuthState:   hostTestAuthState{},
		GateViews:   hostTestGateViews{},
		ReceiptKeys: hostTestReceiptKeys(t, clock.Now()),
	})
	if err != nil {
		t.Fatal(err)
	}
	// deferred calls run last first: the kept call goes on before Close joins the producer
	defer evsHost.Close()
	defer quota.Release()
	awaitHostTestProbe(t, evsHost.Engine())
	if err := evsHost.usage.AddTransfer(connect.NewId(), 100); err != nil {
		t.Fatal(err)
	}

	drainCtx, cancelDrain := context.WithCancel(t.Context())
	drained := make(chan error, 1)
	go func() {
		drained <- evsHost.usage.Drain(drainCtx)
	}()
	// a drain the cancelled report leaves waiting on its retry ends with the test
	defer func() {
		cancelDrain()
		<-drained
	}()
	select {
	case <-quota.arrived:
	case <-time.After(hostTestStepTimeout):
		t.Fatal("the drained window's report never reached the Quota")
	}
	clock.Advance(26 * time.Hour)
	quota.Release()
	select {
	case answer = <-quota.answered:
	case <-time.After(hostTestStepTimeout):
		t.Fatal("the kept report never answered")
	}
	return answer, len(fakes.Quota.SentBatches())
}

// On settings that bound no report, as T3's tier gives the block from the evs world, the report outlives the move.
func TestHostUsageReportOutlivesAMoveOnItsSettings(t *testing.T) {
	usage := sidecar.DefaultQuotaUsageBatcherSettings()
	usage.ReportTimeout = 0
	answer, sentCount := hostTestUsageReportInMove(t, usage)
	if answer != nil || sentCount != 1 {
		t.Fatalf("the report answered %v with %d batches at the Quota, want nil and 1", answer, sentCount)
	}
}

// On sidecar's defaults, which the block took before HostSettings.Usage, the move cancels the report before the Quota
// counts it.
func TestHostUsageReportBoundCancelledByAMove(t *testing.T) {
	answer, sentCount := hostTestUsageReportInMove(t, nil)
	if !errors.Is(answer, context.Canceled) || sentCount != 0 {
		t.Fatalf("the report answered %v with %d batches at the Quota, want its bound's cancel and none", answer, sentCount)
	}
}
