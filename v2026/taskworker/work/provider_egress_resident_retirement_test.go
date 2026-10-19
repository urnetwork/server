package work

import (
	"context"
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"

	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

func TestProviderEgressResidentRetirementOwnedCaptureAndSqlFence(t *testing.T) {
	credentials, _ := providerEgressCredentialFixture(t)
	child, instance := connect.NewId(), connect.NewId()
	credentials.children.Store(server.Id(child), struct{}{})
	token := &model.NetworkClientResidentRetirement{}
	captures, commits := 0, 0
	credentials.captureResident = func(ctx context.Context, c, i server.Id) (*model.NetworkClientResidentRetirement, error) {
		captures++
		if c != server.Id(child) || i != server.Id(instance) {
			t.Error("lost exact owned instance")
		}
		return token, nil
	}
	credentials.removeResident = func(ctx context.Context, got *model.NetworkClientResidentRetirement) (bool, error) {
		commits++
		if got != token {
			t.Error("changed original capture")
		}
		return true, nil
	}
	credentials.retire = func(*model.RemoveNetworkClientArgs, *session.ClientSession) (*model.RemoveNetworkClientResult, error) {
		return &model.RemoveNetworkClientResult{}, nil
	}
	commit, err := credentials.PrepareResidentRetirement(t.Context(), child, instance)
	if err != nil || commit == nil || captures != 1 {
		t.Fatal("owned capture unavailable")
	}
	if err := commit(t.Context()); err == nil || commits != 0 {
		t.Fatal("pre-SQL callback removed resident")
	}
	if _, err := credentials.RemoveNetworkClient(t.Context(), &connect.RemoveNetworkClientArgs{ClientId: child}); err != nil {
		t.Fatal(err)
	}
	if err := commit(t.Context()); err != nil || commits != 1 {
		t.Fatal("successful SQL did not permit exact capture commit")
	}
}

func TestProviderEgressResidentRetirementRejectsForeignAndParent(t *testing.T) {
	credentials, _ := providerEgressCredentialFixture(t)
	calls := 0
	credentials.captureResident = func(context.Context, server.Id, server.Id) (*model.NetworkClientResidentRetirement, error) {
		calls++
		return nil, nil
	}
	for _, child := range []connect.Id{{}, connect.Id(credentials.clientId), connect.NewId()} {
		commit, _ := credentials.PrepareResidentRetirement(t.Context(), child, connect.NewId())
		if commit != nil {
			t.Fatal("foreign child gained cleanup authority")
		}
	}
	owned := connect.NewId()
	credentials.children.Store(server.Id(owned), struct{}{})
	if commit, err := credentials.PrepareResidentRetirement(t.Context(), owned, connect.Id{}); commit != nil || err == nil {
		t.Fatal("zero instance was admitted")
	}
	if calls != 0 {
		t.Fatal("foreign or invalid capture reached Redis")
	}
}

func TestProviderEgressResidentRetirementFailureAndIndependentOwners(t *testing.T) {
	for _, scenario := range []string{"sql-error", "sql-rejected", "revoked", "capture-missing", "capture-error"} {
		t.Run(scenario, func(t *testing.T) {
			credentials, _ := providerEgressCredentialFixture(t)
			child, other := connect.NewId(), connect.NewId()
			credentials.children.Store(server.Id(child), struct{}{})
			credentials.children.Store(server.Id(other), struct{}{})
			commits := 0
			credentials.captureResident = func(context.Context, server.Id, server.Id) (*model.NetworkClientResidentRetirement, error) {
				switch scenario {
				case "capture-missing":
					return nil, nil
				case "capture-error":
					return nil, context.DeadlineExceeded
				default:
					return &model.NetworkClientResidentRetirement{}, nil
				}
			}
			credentials.removeResident = func(context.Context, *model.NetworkClientResidentRetirement) (bool, error) {
				commits++
				return true, nil
			}
			credentials.retire = func(*model.RemoveNetworkClientArgs, *session.ClientSession) (*model.RemoveNetworkClientResult, error) {
				if scenario == "sql-rejected" {
					return &model.RemoveNetworkClientResult{Error: &model.RemoveNetworkClientError{Message: "refused"}}, nil
				}
				return nil, errors.New("SQL failed")
			}
			commit, err := credentials.PrepareResidentRetirement(t.Context(), child, connect.NewId())
			if scenario == "capture-error" {
				if !errors.Is(err, context.DeadlineExceeded) || commit != nil {
					t.Fatal("failed capture became callback")
				}
			} else if scenario == "capture-missing" {
				if err != nil || commit != nil {
					t.Fatal("missing capture became callback")
				}
			} else {
				if err != nil || commit == nil {
					t.Fatal("capture failed")
				}
				if scenario == "revoked" {
					credentials.validate = func(context.Context, *session.ByJwt, bool) error { return errors.New("revoked") }
				}
				if _, err := credentials.RemoveNetworkClient(t.Context(), &connect.RemoveNetworkClientArgs{ClientId: child}); err == nil {
					t.Fatal("failed SQL was accepted")
				}
				if err := commit(t.Context()); err == nil {
					t.Fatal("failed SQL permitted commit")
				}
			}
			if commits != 0 {
				t.Fatal("failure removed resident")
			}
			if _, ok := credentials.children.Load(server.Id(other)); !ok {
				t.Fatal("independent child changed")
			}
		})
	}
}

func TestProviderEgressResidentRetirementMetricsStayFinite(t *testing.T) {
	recordProviderEgressResidentRetirement("unknown-private-label", true, nil)
	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(egressProbeResidentRetirementTotal)
	families, err := registry.Gather()
	if err != nil || len(families) != 1 || len(families[0].Metric) != 10 {
		t.Fatal("resident telemetry lost finite ten cells")
	}
	for _, metric := range families[0].Metric {
		if len(metric.Label) != 2 || metric.Counter == nil {
			t.Fatal("resident telemetry gained identity labels")
		}
	}
}
