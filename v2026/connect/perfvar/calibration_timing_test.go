package perfvar

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// A joined worker still belongs to the helper, but its post-delivery cleanup
// is not bulk transfer time. Advance only the bulk clock at exact lifecycle
// barriers so this regression has no wall-clock performance threshold.
func TestPerfvarCalibrationLatencyBulkDurationExcludesWorkerJoin(t *testing.T) {
	const bulkByteCount = 64 * 1024
	const bulkDuration = time.Second
	const cleanupDuration = 98 * time.Second
	for _, forward := range []bool{true, false} {
		name := "upload"
		if !forward {
			name = "download"
		}
		t.Run(name, func(t *testing.T) {
			profile := initialNetworkProfiles(20260818)["clean-lan"]
			ctx, cancel := context.WithTimeout(
				t.Context(),
				calibrationWorkloadTestTimeout(profile, bulkByteCount, 45*time.Second),
			)
			defer cancel()
			var bulkClockElapsed atomic.Int64
			var workerJoined atomic.Bool
			fifthLoadedAttempt := make(chan struct{})
			testSettings := &workloadTCPFlowTestSettings{
				bulkNowForTest: func() time.Time {
					return time.Unix(100, 0).Add(time.Duration(bulkClockElapsed.Load()))
				},
				beforeBulkReceiverDoneHook: func() {
					// Keep the loaded phase alive for its correctness samples.
					select {
					case <-fifthLoadedAttempt:
					case <-ctx.Done():
					}
					bulkClockElapsed.Store(int64(bulkDuration))
				},
				afterLoadedProbeAttemptHook: func(attemptCount int) {
					if attemptCount == 5 {
						close(fifthLoadedAttempt)
					}
				},
				beforeBulkSenderWaitHook: func() {
					bulkClockElapsed.Add(int64(cleanupDuration))
					workerJoined.Store(true)
				},
			}
			result, err := measureLatencyUnderLoadWithFlowTestSettingsDirection(
				ctx,
				profile,
				defaultTunResourceProfile(),
				bulkByteCount,
				forward,
				nil,
				testSettings,
			)
			if err != nil {
				t.Fatal(err)
			}
			if !workerJoined.Load() {
				t.Fatal("helper returned without joining its bulk sender")
			}
			if result.Duration != bulkDuration {
				t.Fatalf("bulk duration=%s want=%s; post-delivery cleanup=%s must be excluded", result.Duration, bulkDuration, cleanupDuration)
			}
		})
	}
}
