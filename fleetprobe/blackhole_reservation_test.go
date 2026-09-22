// The blackhole pass budgets tiny reachability requests without changing the
// full/bandwidth pass's shared caller configuration.
package fleetprobe

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/operator-proxy/providertunnel"
)

// Each worker copies its configuration, including when another pass has an
// explicit full-probe budget. Only the cheap pass receives the small target.
func TestBlackholeContractReservationIsLocalToProbe(t *testing.T) {
	for _, configured := range []connect.ByteCount{0, 64 * 1024 * 1024} {
		shared := providertunnel.Config{
			ApiURL:                       "https://api.invalid",
			ContractReservationByteCount: configured,
		}
		full := FullOptions{TunnelConfig: shared}
		options := BlackholeOptions{TunnelConfig: shared, Pins: testPins}
		for range 32 {
			config := blackholeTunnelConfig(options)
			if config.ContractReservationByteCount != 1024*1024 {
				t.Fatalf("blackhole contract reservation=%d, want 1MiB", config.ContractReservationByteCount)
			}
			if !reflect.DeepEqual(config.Pins, testPins()) {
				t.Fatal("blackhole config lost refreshed certificate pins")
			}
			config.ContractReservationByteCount = 1
		}
		if !reflect.DeepEqual(options.TunnelConfig, shared) || !reflect.DeepEqual(full.TunnelConfig, shared) {
			t.Fatal("blackhole policy mutated the caller's full/bandwidth configuration")
		}
	}
}

// Overriding the cheap probe policy must not hide an invalid caller config.
func TestBlackholeContractReservationRejectsNegative(t *testing.T) {
	err := validateBlackholeOptions(BlackholeOptions{
		TunnelConfig: providertunnel.Config{ContractReservationByteCount: -1},
		Pins:         testPins,
		Timeout:      time.Second,
		Concurrency:  1,
	})
	if !errors.Is(err, providertunnel.ErrContractReservation) {
		t.Fatalf("negative contract reservation error=%v", err)
	}
}
