package perfvar

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

const (
	perfvarEnduranceDownloadDuration    = 5 * time.Minute
	perfvarEnduranceProgressInterval    = 10 * time.Second
	perfvarEnduranceMaximumProgressGap  = 30 * time.Second
	perfvarEnduranceMaximumDrain        = 2 * time.Minute
	perfvarEnduranceFixtureTimeout      = 10 * time.Minute
	perfvarEnduranceDownloadRateBits    = int64(5_000_000)
	perfvarEnduranceDownloadProfileSeed = int64(20261112)
)

type perfvarEnduranceDownloadResult struct {
	observation perfvarCorrectnessObservation
	err         error
}

// One loss-free cell-edge profile isolates long-lived transfer liveness from
// probabilistic recovery. The one-packet burst prevents setup credit from
// hiding a stalled long-lived workload behind a short initial burst.
func perfvarEnduranceDownloadProfile() networkProfile {
	profile := cellEdgeNetworkProfiles(perfvarEnduranceDownloadProfileSeed)[cellEdge5mDown1mUpName]
	profile.Name = "synthetic-h1-endurance-5m"
	profile.SourceNote = "deterministic five-minute synthetic endurance download; no random loss"
	for _, link := range []*linkProfile{&profile.Forward, &profile.Reverse} {
		link.LossModel = lossModelNone
		link.LossProbability = 0
		link.DropEveryPacketCount = 0
		link.BurstLoss = nil
		link.DuplicateProbability = 0
		link.ReorderProbability = 0
		link.AllowQueueDrops = false
	}
	return profile
}

// Deterministic pattern validation retains no body-sized allocation and works
// across arbitrary socket fragmentation and payload-block wraparound.
func perfvarValidateEnduranceDownloadChunk(payload, chunk []byte, offset int64) bool {
	for len(chunk) > 0 {
		start := int(offset % int64(len(payload)))
		count := min(len(chunk), len(payload)-start)
		if !bytes.Equal(chunk[:count], payload[start:start+count]) {
			return false
		}
		chunk = chunk[count:]
		offset += int64(count)
	}
	return true
}

// One duration-owned download streams until the receiver publishes its stop
// byte. The provider-origin writer and stop reader share the real full-TUN TCP
// flow; EOF is success only after that stop byte is received. Every client read
// validates the deterministic pattern and a bounded read deadline makes a
// thirty-second payload gap fail before the broader fixture timeout.
func measureFullTunEnduranceDownload(
	ctx context.Context,
	path *fullTunPath,
	duration time.Duration,
	maximumProgressGap time.Duration,
	startHook func(),
) (workloadResult, error) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return workloadResult{}, err
	}
	const flowID = logicalTCPFlowId(0)
	payload := deterministicPayload()
	startMeasured := make(chan struct{})
	var startMeasuredOnce sync.Once
	releaseMeasured := func() { startMeasuredOnce.Do(func() { close(startMeasured) }) }
	flowServer := newLogicalTCPFlowServer(
		ctx,
		listener,
		1,
		func(receivedFlowID logicalTCPFlowId, connection net.Conn) error {
			if receivedFlowID != flowID {
				return fmt.Errorf("endurance download flow id=%d, want=%d", receivedFlowID, flowID)
			}
			if deadline, ok := ctx.Deadline(); ok {
				if err := connection.SetDeadline(deadline); err != nil {
					return err
				}
			}
			stopResult := make(chan error, 1)
			go func() {
				var stop [1]byte
				_, readErr := io.ReadFull(connection, stop[:])
				if readErr == nil && stop[0] != 1 {
					readErr = fmt.Errorf("endurance download stop value=%d, want=1", stop[0])
				}
				stopResult <- readErr
			}()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-startMeasured:
			}
			for {
				select {
				case stopErr := <-stopResult:
					return stopErr
				default:
				}
				if writeErr := writeFullTunAll(connection, payload); writeErr != nil {
					_ = connection.SetDeadline(time.Now())
					stopErr := <-stopResult
					if ctx.Err() != nil {
						return ctx.Err()
					}
					return fmt.Errorf("endurance download origin write: %w (stop read: %v)", writeErr, stopErr)
				}
			}
		},
		path.workloadFlowServerSettingsForTest,
	)
	defer func() {
		releaseMeasured()
		flowServer.CloseAndWait()
	}()
	if path.beforeWorkloadClientDialForTest != nil {
		if err := path.beforeWorkloadClientDialForTest(ctx, listener.Addr().String()); err != nil {
			return workloadResult{}, err
		}
	}
	dialCtx, dialCancel := context.WithTimeout(ctx, fullTunRouteReadinessTimeout(path))
	setupStart := time.Now()
	connection, err := path.appTun.DialContext(dialCtx, "tcp", listener.Addr().String())
	dialCancel()
	if err != nil {
		return workloadResult{}, err
	}
	defer connection.Close()
	if deadline, ok := ctx.Deadline(); ok {
		if err := connection.SetDeadline(deadline); err != nil {
			return workloadResult{}, err
		}
	}
	stopInterrupt := interruptDeadlineOnContext(ctx, connection)
	defer stopInterrupt()
	if err := writeLogicalTCPFlowPreface(connection, flowID); err != nil {
		return workloadResult{}, contextBoundWorkloadError(ctx, err)
	}
	select {
	case <-flowServer.Ready():
	case <-flowServer.Done():
		if serverErr := flowServer.Wait(); serverErr != nil {
			return workloadResult{}, contextBoundWorkloadError(ctx, serverErr)
		}
		return workloadResult{}, fmt.Errorf("endurance download flow completed before readiness")
	case <-ctx.Done():
		return workloadResult{}, ctx.Err()
	}
	setupDuration := time.Since(setupStart)
	if err := path.waitForSetupBoundary(ctx); err != nil {
		return workloadResult{}, fmt.Errorf("join endurance download setup boundary: %w", err)
	}
	carrierMeasurementStart, err := beginPerfvarCarrierMeasurement(path)
	if err != nil {
		return workloadResult{}, err
	}
	path.setCarrierMeasurementStart(carrierMeasurementStart)
	path.workloadProgressBytes.Store(0)
	if startHook != nil {
		startHook()
	}

	var memoryBefore runtime.MemStats
	runtime.ReadMemStats(&memoryBefore)
	started := time.Now()
	stopAt := started.Add(duration)
	releaseMeasured()
	hash := sha256.New()
	buffer := make([]byte, 32*1024)
	readByteCount := int64(0)
	stopSent := false
	var drainDeadline time.Time
	for {
		if !stopSent {
			readDeadline := time.Now().Add(maximumProgressGap)
			if stopAt.Before(readDeadline) {
				readDeadline = stopAt
			}
			if err := connection.SetReadDeadline(readDeadline); err != nil {
				return workloadResult{}, err
			}
		} else {
			readDeadline := time.Now().Add(maximumProgressGap)
			if drainDeadline.Before(readDeadline) {
				readDeadline = drainDeadline
			}
			if err := connection.SetReadDeadline(readDeadline); err != nil {
				return workloadResult{}, err
			}
		}
		count, readErr := connection.Read(buffer)
		if count > 0 {
			if !perfvarValidateEnduranceDownloadChunk(payload, buffer[:count], readByteCount) {
				return workloadResult{}, fmt.Errorf("endurance download content mismatch at byte %d", readByteCount)
			}
			_, _ = hash.Write(buffer[:count])
			readByteCount += int64(count)
			path.workloadProgressBytes.Add(int64(count))
		}
		if !stopSent && !time.Now().Before(stopAt) {
			if readErr != nil {
				if timeout, ok := readErr.(net.Error); !ok || !timeout.Timeout() {
					return workloadResult{}, contextBoundWorkloadError(ctx, readErr)
				}
				readErr = nil
			}
			if err := connection.SetDeadline(boundedWorkloadDeadline(ctx, maximumProgressGap)); err != nil {
				return workloadResult{}, err
			}
			if err := writeFullTunAll(connection, []byte{1}); err != nil {
				return workloadResult{}, contextBoundWorkloadError(ctx, err)
			}
			stopSent = true
			drainDeadline = time.Now().Add(perfvarEnduranceMaximumDrain)
		}
		if readErr == io.EOF {
			if !stopSent {
				return workloadResult{}, fmt.Errorf("endurance download ended before its five-minute stop")
			}
			break
		}
		if readErr != nil {
			return workloadResult{}, contextBoundWorkloadError(ctx, readErr)
		}
	}
	if err := flowServer.Wait(); err != nil {
		return workloadResult{}, contextBoundWorkloadError(ctx, err)
	}
	elapsed := time.Since(started)
	if elapsed < duration {
		return workloadResult{}, fmt.Errorf("endurance download stopped after %s, want at least %s", elapsed, duration)
	}
	contentHash := hex.EncodeToString(hash.Sum(nil))
	if contentHash != deterministicPayloadHash(readByteCount) {
		return workloadResult{}, fmt.Errorf("endurance download final hash mismatch after %d bytes", readByteCount)
	}
	var memoryAfter runtime.MemStats
	runtime.ReadMemStats(&memoryAfter)
	return finishWorkloadResult(workloadResult{
		UsefulByteCount:        readByteCount,
		DeliveredPacketCount:   1,
		Duration:               elapsed,
		SetupDuration:          setupDuration,
		ContentHash:            contentHash,
		AllocatedByteCount:     memoryAfter.TotalAlloc - memoryBefore.TotalAlloc,
		AllocationCount:        memoryAfter.Mallocs - memoryBefore.Mallocs,
		GarbageCollectionCount: memoryAfter.NumGC - memoryBefore.NumGC,
		GarbageCollectionPause: time.Duration(memoryAfter.PauseTotalNs - memoryBefore.PauseTotalNs),
	}), nil
}

// TestPerfvarSyntheticH1EnduranceDownload keeps one exact download active for
// at least five wall-clock minutes through the complete app-TUN, exchange-H1,
// provider-NAT path. It fails on corruption, truncation, route bypass, early
// completion, or a thirty-second interval without delivered payload progress.
func TestPerfvarSyntheticH1EnduranceDownload(t *testing.T) {
	if testing.Short() {
		t.Skip("five-minute endurance transfer is part of the non-short correctness tier")
	}
	profile := perfvarEnduranceDownloadProfile()
	if profile.Reverse.RateBitsPerSecond != perfvarEnduranceDownloadRateBits ||
		profile.Reverse.BurstByteCount > profile.Reverse.OuterMtu {
		t.Fatalf("endurance profile lost its shaped download boundary: %+v", profile.Reverse)
	}

	testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	testEnvironment.Run(t, func(t testing.TB) {
		profiles := initialNetworkProfiles(perfvarEnduranceDownloadProfileSeed)
		providerProfile := profiles["clean-lan"]
		providerProfile.SourceNote = "synthetic provider colocated with server/connect"
		fixture, err := newPerfvarCorrectnessFixture(
			t,
			fullTunRouteExchangeH1,
			profile,
			profile,
			providerProfile,
			defaultTunResourceProfile(),
			perfvarEnduranceFixtureTimeout,
		)
		if err != nil {
			t.Fatalf("construct synthetic H1 endurance fixture: %v", err)
		}
		defer fixture.close()

		measurementStarted := make(chan time.Time, 1)
		measurementDone := make(chan perfvarEnduranceDownloadResult, 1)
		go func() {
			observation, measureErr := fixture.measure(
				perfvarWorkloadTCP,
				perfvarDirectionDownload,
				func(ctx context.Context, path *fullTunPath) (workloadResult, error) {
					return measureFullTunEnduranceDownload(
						ctx,
						path,
						perfvarEnduranceDownloadDuration,
						perfvarEnduranceMaximumProgressGap,
						func() {
							measurementStarted <- time.Now()
						},
					)
				},
			)
			measurementDone <- perfvarEnduranceDownloadResult{
				observation: observation,
				err:         measureErr,
			}
		}()

		var started time.Time
		select {
		case started = <-measurementStarted:
		case result := <-measurementDone:
			if result.err == nil {
				result.err = fmt.Errorf("download completed before publishing its measurement start")
			}
			t.Fatalf("start synthetic H1 endurance download: %v", result.err)
		case <-fixture.ctx.Done():
			t.Fatalf("start synthetic H1 endurance download: %v", fixture.ctx.Err())
		}

		ticker := time.NewTicker(perfvarEnduranceProgressInterval)
		defer ticker.Stop()
		lastProgressAt := started
		lastByteCount := int64(0)
		progressSampleCount := 0
		maximumProgressGap := time.Duration(0)
		var stalledErr error
		var result perfvarEnduranceDownloadResult
	measure:
		for {
			select {
			case result = <-measurementDone:
				break measure
			case sampledAt := <-ticker.C:
				progressSampleCount++
				currentByteCount := fixture.path.workloadProgressBytes.Load()
				if lastByteCount < currentByteCount {
					maximumProgressGap = max(maximumProgressGap, sampledAt.Sub(lastProgressAt))
					lastProgressAt = sampledAt
					lastByteCount = currentByteCount
				}
				if perfvarEnduranceMaximumProgressGap < sampledAt.Sub(lastProgressAt) {
					stalledErr = fmt.Errorf(
						"synthetic H1 endurance download made no progress for %s after %d bytes",
						sampledAt.Sub(lastProgressAt),
						currentByteCount,
					)
					fixture.cancel()
					result = <-measurementDone
					break measure
				}
			case <-fixture.ctx.Done():
				result = <-measurementDone
				break measure
			}
		}
		if stalledErr != nil {
			t.Fatal(stalledErr)
		}
		if result.err != nil {
			t.Fatalf("synthetic H1 endurance download: %v", result.err)
		}

		workload := result.observation.Result
		if workload.Duration < perfvarEnduranceDownloadDuration {
			t.Fatalf(
				"synthetic H1 endurance download ended after %s, before the %s endurance boundary",
				workload.Duration,
				perfvarEnduranceDownloadDuration,
			)
		}
		if workload.UsefulByteCount <= 0 || workload.ContentHash != deterministicPayloadHash(workload.UsefulByteCount) {
			t.Fatalf(
				"synthetic H1 endurance body changed: bytes=%d hash=%q/%q",
				workload.UsefulByteCount,
				workload.ContentHash,
				deterministicPayloadHash(workload.UsefulByteCount),
			)
		}
		minimumSamples := int(perfvarEnduranceDownloadDuration/perfvarEnduranceProgressInterval) - 1
		if progressSampleCount < minimumSamples || lastByteCount <= 0 ||
			perfvarEnduranceMaximumProgressGap < maximumProgressGap {
			t.Fatalf(
				"synthetic H1 endurance progress changed: samples=%d/%d last_bytes=%d max_gap=%s/%s",
				progressSampleCount,
				minimumSamples,
				lastByteCount,
				maximumProgressGap,
				perfvarEnduranceMaximumProgressGap,
			)
		}
		t.Logf(
			"[synthetic-h1-endurance] duration=%s bytes=%d samples=%d max_progress_gap=%s carrier_wire_bytes=%d exact_hash=true",
			workload.Duration,
			workload.UsefulByteCount,
			progressSampleCount,
			maximumProgressGap,
			result.observation.Carrier.WireByteCount,
		)
	})
}
