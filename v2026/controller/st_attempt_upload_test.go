// Testnet and mainnet select independent explicit artifact-upload capacities.
package controller

import (
	"context"
	"errors"
	"testing"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	stconn "github.com/urnetwork/server/v2026/st"
	"gopkg.in/yaml.v3"
)

// Parsing and final configuration preserve all four counters in one namespace.
func TestStAttemptUploadConfigProfilesDoNotShareBudgets(t *testing.T) {
	t.Parallel()
	mainnet := model.StAttemptUploadBudget{RequestsPerHour: 7, BytesPerHour: 80, AccountRequestsPerHour: 3, AccountBytesPerHour: 40}
	testnet := model.StAttemptUploadBudget{RequestsPerHour: 11, BytesPerHour: 120, AccountRequestsPerHour: 5, AccountBytesPerHour: 60}
	var file stVaultFile
	if err := yaml.Unmarshal([]byte("attempt_upload:\n  requests_per_hour: 7\n  bytes_per_hour: 80\n  account_requests_per_hour: 3\n  account_bytes_per_hour: 40\ntestnet-attempt-upload:\n  requests_per_hour: 11\n  bytes_per_hour: 120\n  account_requests_per_hour: 5\n  account_bytes_per_hour: 60\n"), &file); err != nil {
		t.Fatal(err)
	}
	for _, input := range []struct {
		profile  string
		expected model.StAttemptUploadBudget
	}{{profile: stconn.ProfileMainnet, expected: mainnet}, {profile: stconn.ProfileTestnet, expected: testnet}} {
		selected, err := selectStConfig(input.profile, file)
		if err != nil || selected.AttemptUploadBudget != input.expected {
			t.Fatalf("profile budget selection differs: %v", err)
		}
		cfg, err := stConfigForProfile(input.profile, file, nil)
		if err != nil || cfg.AttemptUploadBudget != input.expected {
			t.Fatalf("profile budget conversion differs: %v", err)
		}
	}
}

// Canceled and absent owners cannot trigger lazy vault or quota acquisition.
func TestStAttemptUploadConfigPreCancellationNeedsNoVault(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := StReserveAttemptUpload(ctx, server.NewId(), 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled upload loaded configuration: %v", err)
	}
	if err := StReserveAttemptUpload(nil, server.NewId(), 1); err == nil {
		t.Fatal("missing context was admitted")
	}
}

// Missing testnet values never inherit enabled mainnet storage permissions.
func TestStAttemptUploadConfigMissingProfileBudgetStaysDisabled(t *testing.T) {
	t.Parallel()
	file := stVaultFile{AttemptUploadBudget: model.StAttemptUploadBudget{RequestsPerHour: 7, BytesPerHour: 80, AccountRequestsPerHour: 3, AccountBytesPerHour: 40}}
	selected, err := selectStConfig(stconn.ProfileTestnet, file)
	if err != nil || selected.AttemptUploadBudget != (model.StAttemptUploadBudget{}) || selected.AttemptUploadBudget.Validate() == nil {
		t.Fatalf("missing testnet budget gained a mainnet fallback: %v", err)
	}
	if _, err := selectStConfig("unknown", file); err == nil {
		t.Fatal("unknown budget profile accepted")
	}
}
