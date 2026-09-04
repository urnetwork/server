package connect_test

//
// Directional TCP throughput through the full multi-client stack, splitting the
// echo test's coupled directions so a fix's effect can be attributed:
//
//   - upload: device writes to a discard sink. data device->provider, acks
//     provider->device.
//   - download: device reads from a source server. data provider->device, acks
//     device->provider (the direction the shared-FIFO ack queueing hits).
//
// Reported with "[mctcpdir]" lines. Uses the same env/floors philosophy as the
// mctcp test: floors are collapse detectors, extra evidence runs while unmet.

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

const (
	mcDirStreamByteCount   = 64 * 1024 * 1024
	mcDirRunCount          = 3
	mcDirExtraRunCount     = 2
	mcDirMinGoodput        = 0.35
	mcDirPerRunReadTimeout = 60 * time.Second
)

func TestConnectMultiClientTcpDirectionalPerformance(t *testing.T) {
	perfTestEnv().Run(t, func(t testing.TB) {
		testConnectMultiClientTcpDirectionalPerformance(t)
	})
}

func testConnectMultiClientTcpDirectionalPerformance(t testing.TB) {
	if testing.Short() {
		return
	}

	os.Setenv("WARP_SERVICE", "test")
	os.Setenv("WARP_BLOCK", "test")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fmt.Printf("[mctcpdir]go=%s cpus=%d gomaxprocs=%d\n", runtime.Version(), runtime.NumCPU(), runtime.GOMAXPROCS(0))

	stack, cleanup := setupMcStack(ctx, "mctcpdir")
	defer cleanup()

	streamByteCount := int64(mcDirStreamByteCount)
	if serverConnectRaceEnabled {
		// See the coupled full-stack TCP test. Under race this is a bounded
		// grouped-path correctness workload rather than a capacity measurement.
		streamByteCount = 4 * 1024 * 1024
	}

	// ---- sink server: reads and discards (upload target) ----------------------
	sinkListener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	defer sinkListener.Close()
	sinkAddr := sinkListener.Addr().String()
	go func() {
		for {
			conn, err := sinkListener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				io.Copy(io.Discard, conn)
			}()
		}
	}()

	// ---- source server: writes mcDirStreamByteCount then closes (download) ----
	sourceListener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	defer sourceListener.Close()
	sourceAddr := sourceListener.Addr().String()
	go func() {
		chunk := make([]byte, 64*1024)
		for {
			conn, err := sourceListener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				remaining := streamByteCount
				for 0 < remaining {
					n := min(int64(len(chunk)), remaining)
					conn.SetWriteDeadline(time.Now().Add(mcDirPerRunReadTimeout))
					written, err := conn.Write(chunk[:n])
					remaining -= int64(written)
					if err != nil {
						return
					}
				}
			}()
		}
	}()

	// ---- device tun bridged to the multi client (same shape as mctcp) ---------
	tun, err := connect.CreateTunWithDefaults(ctx)
	if err != nil {
		panic(err)
	}
	defer tun.Close()

	deviceStrategySettings := connect.DefaultClientStrategySettings()
	deviceStrategySettings.EnableResilient = false
	deviceStrategy := connect.NewClientStrategy(ctx, deviceStrategySettings)

	providerClientIdConnect := connect.Id(stack.providerClientId)
	deviceClientIdConnect := connect.Id(stack.deviceClientId)
	specs := []*connect.ProviderSpec{
		{ClientId: &providerClientIdConnect},
	}
	generator := connect.NewApiMultiClientGenerator(
		ctx,
		specs,
		deviceStrategy,
		nil,
		stack.apiUrl,
		stack.deviceByJwt,
		stack.devicePlatformUrl,
		"mctcpdir",
		"mctcpdir",
		"0.0.0",
		&deviceClientIdConnect,
		newLocalPerformanceClientSettings,
		connect.DefaultApiMultiClientGeneratorSettings(),
	)

	// received packets inject through the receive-dispatch batch: the
	// ReceiveSequence delivers each drain burst's frames in one callback, the
	// multi client collects the burst's committed-flow packets, and
	// tun.WriteBatch GRO-coalesces them into super-segments (one
	// dispatch/enqueue/ack cycle per super-segment). Rare paths (uncommitted
	// flows) fall back to the per-packet callback. Both are synchronous, so
	// the inject remains the path's flow control (the provider nat does not
	// retransmit toward the device).
	receivePacket := func(source connect.TransferPath, provideMode protocol.ProvideMode, ipPath *connect.IpPath, packet []byte) {
		tun.Write(packet)
	}
	multiClient := connect.NewRemoteUserNatMultiClientWithDefaults(
		ctx,
		generator,
		receivePacket,
		protocol.ProvideMode_Network,
	)
	defer multiClient.Close()
	multiClient.SetReceivePacketsCallback(func(source connect.TransferPath, provideMode protocol.ProvideMode, ipPath *connect.IpPath, packets [][]byte) {
		tun.WriteBatch(packets)
	})

	deviceSource := connect.SourceId(deviceClientIdConnect)
	go func() {
		packets := make([][]byte, 64)
		for {
			n, err := tun.ReadBatch(packets)
			if err != nil {
				return
			}
			multiClient.SendPacketBatch(
				deviceSource,
				protocol.ProvideMode_Network,
				packets[:n],
				-1,
			)
		}
	}()

	// runUpload writes the volume to the sink; goodput from the write side.
	runUpload := func() (float64, bool) {
		dialCtx, dialCancel := context.WithTimeout(ctx, 60*time.Second)
		conn, err := tun.DialContext(dialCtx, "tcp", sinkAddr)
		dialCancel()
		if err != nil {
			fmt.Printf("[mctcpdir]upload dial failed: %v (dropping sample)\n", err)
			return 0, false
		}
		defer conn.Close()
		chunk := make([]byte, 64*1024)
		start := time.Now()
		written := int64(0)
		for written < streamByteCount {
			n := min(int64(len(chunk)), streamByteCount-written)
			conn.SetWriteDeadline(time.Now().Add(mcDirPerRunReadTimeout))
			wn, err := conn.Write(chunk[:n])
			written += int64(wn)
			if err != nil {
				fmt.Printf("[mctcpdir]upload stalled after %dMiB: %v (dropping sample)\n", written/(1024*1024), err)
				return 0, false
			}
		}
		elapsed := time.Since(start)
		goodput := float64(written) / (1024 * 1024) / elapsed.Seconds()
		fmt.Printf("[mctcpdir]upload bytes=%dMiB elapsed=%.2fs goodput=%.2f MiB/s\n", written/(1024*1024), elapsed.Seconds(), goodput)
		return goodput, true
	}

	// runDownload reads the volume from the source; goodput from the read side.
	runDownload := func() (float64, bool) {
		dialCtx, dialCancel := context.WithTimeout(ctx, 60*time.Second)
		conn, err := tun.DialContext(dialCtx, "tcp", sourceAddr)
		dialCancel()
		if err != nil {
			fmt.Printf("[mctcpdir]download dial failed: %v (dropping sample)\n", err)
			return 0, false
		}
		defer conn.Close()
		buffer := make([]byte, 256*1024)
		start := time.Now()
		var readByteCount int64
		for readByteCount < streamByteCount {
			conn.SetReadDeadline(time.Now().Add(mcDirPerRunReadTimeout))
			n, err := conn.Read(buffer)
			if 0 < n {
				atomic.AddInt64(&readByteCount, int64(n))
			}
			if err != nil {
				if err == io.EOF && readByteCount == streamByteCount {
					break
				}
				fmt.Printf("[mctcpdir]download stalled after %dMiB: %v (dropping sample)\n", readByteCount/(1024*1024), err)
				return 0, false
			}
		}
		elapsed := time.Since(start)
		goodput := float64(readByteCount) / (1024 * 1024) / elapsed.Seconds()
		fmt.Printf("[mctcpdir]download bytes=%dMiB elapsed=%.2fs goodput=%.2f MiB/s\n", readByteCount/(1024*1024), elapsed.Seconds(), goodput)
		return goodput, true
	}

	measure := func(label string, run func() (float64, bool)) float64 {
		best := 0.0
		okRuns := 0
		runs := 0
		for runs < mcDirRunCount ||
			(runs < mcDirRunCount+mcDirExtraRunCount && (okRuns == 0 || best < mcDirMinGoodput)) {
			runs += 1
			if g, ok := run(); ok {
				okRuns += 1
				best = max(best, g)
			}
		}
		fmt.Printf("[mctcpdir]%s goodput=%.2f MiB/s (max of %d/%d completed runs)\n", label, best, okRuns, runs)
		if okRuns == 0 {
			panic(fmt.Errorf("%s: all %d runs stalled before completing", label, runs))
		}
		if !serverConnectRaceEnabled && best < mcDirMinGoodput {
			panic(fmt.Errorf("%s goodput too low: %.2f MiB/s (%d/%d runs completed)", label, best, okRuns, runs))
		}
		return best
	}

	uploadGoodput := measure("upload", runUpload)

	profileDir := "profile"
	os.MkdirAll(profileDir, 0755)
	downloadCpuPath := filepath.Join(profileDir, "mctcpdir_download_cpu.pprof")
	downloadCpuFile, _ := os.Create(downloadCpuPath)
	downloadCpuActive := pprof.StartCPUProfile(downloadCpuFile) == nil

	downloadGoodput := measure("download", runDownload)

	if downloadCpuActive {
		pprof.StopCPUProfile()
		downloadCpuFile.Close()
		fmt.Printf("[mctcpdir]download cpu profile %s\n", downloadCpuPath)
	}

	fmt.Printf("[mctcpdir]==== summary ====\n")
	fmt.Printf("[mctcpdir]upload=%.2f MiB/s download=%.2f MiB/s\n", uploadGoodput, downloadGoodput)
}
