package work

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"

	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

func providerEgressCredentialFixture(t *testing.T) (*providerEgressCredentials, *session.ByJwt) {
	t.Helper()
	network, user, client, device := server.NewId(), server.NewId(), server.NewId(), server.NewId()
	credentials, err := newProviderEgressCredentials(&model.ProberIdentity{
		NetworkId: &network, UserId: &user, ClientId: &client, ByClientJwt: "synthetic-parent-token",
	})
	if err != nil {
		t.Fatal(err)
	}
	claims := &session.ByJwt{
		NetworkId: network, UserId: user, ClientId: &client, DeviceId: &device,
		CreateTime: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		Roles:      []string{"synthetic-role"}, Principal: "synthetic-principal",
	}
	credentials.parse = func(ctx context.Context, token, audience string) (*session.ByJwt, error) {
		if token != "synthetic-parent-token" || audience != session.ByJwtAudienceApi {
			t.Error("internal mint used another credential or audience")
		}
		return claims, nil
	}
	credentials.validate = func(ctx context.Context, value *session.ByJwt, requireClient bool) error {
		if value != claims || !requireClient {
			t.Error("internal mint bypassed parent-client state validation")
		}
		return nil
	}
	return credentials, claims
}

func TestProviderEgressCredentialsMintUsesExistingModelAndLineage(t *testing.T) {
	credentials, claims := providerEgressCredentialFixture(t)
	child := server.NewId()
	token := "synthetic-model-signed-token"
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	mints := 0
	var parent *session.ClientSession
	credentials.mint = func(args *model.AuthNetworkClientArgs, s *session.ClientSession) (*model.AuthNetworkClientResult, error) {
		mints++
		parent = s
		if args.ClientId != nil || args.SourceClientId == nil || *args.SourceClientId != credentials.clientId ||
			len(args.Roles) != 0 || args.Principal != "" || args.ProxyConfig != nil ||
			args.Description != model.ProberClientDescription || args.DeviceSpec != model.ProberClientDeviceSpec {
			t.Error("mint widened the prober's derived-client arguments")
		}
		if s.ByJwt != claims || s.ByJwt.CreateTime != claims.CreateTime || !slices.Equal(s.ByJwt.Roles, claims.Roles) || s.ByJwt.Principal != claims.Principal {
			t.Error("mint fabricated a fresh privileged parent instead of preserving lineage")
		}
		deadline, ok := s.Ctx.Deadline()
		want, _ := ctx.Deadline()
		if !ok || deadline != want || s.Ctx.Err() != nil {
			t.Error("mint changed the caller's deadline")
		}
		return &model.AuthNetworkClientResult{ClientId: &child, ByClientJwt: &token}, nil
	}
	source := connect.Id(credentials.clientId)
	result, err := credentials.AuthNetworkClient(ctx, &connect.AuthNetworkClientArgs{
		SourceClientId: &source, Description: "ignored caller metadata", DeviceSpec: "ignored",
	})
	if err != nil || result == nil || result.ByClientJwt != token || mints != 1 || parent.Ctx.Err() != context.Canceled || ctx.Err() != nil {
		t.Fatal("internal mint did not return exactly the model's credential and release its child session")
	}
}

// Exact stored ownership, normal signature/expiry parsing and current state
// validation all precede model writes. Invalid credentials never gain a
// network-level session by reconstructing claims from stored IDs alone.
func TestProviderEgressCredentialsRejectInvalidParentAndForeignSource(t *testing.T) {
	for _, scenario := range []string{"signature", "revoked", "network", "user", "client", "device", "source", "reauth", "canceled"} {
		credentials, claims := providerEgressCredentialFixture(t)
		ctx, cancel := context.WithCancel(t.Context())
		source := connect.Id(credentials.clientId)
		args := &connect.AuthNetworkClientArgs{SourceClientId: &source}
		mints := 0
		credentials.mint = func(*model.AuthNetworkClientArgs, *session.ClientSession) (*model.AuthNetworkClientResult, error) {
			mints++
			return nil, errors.New("unexpected mint")
		}
		switch scenario {
		case "signature":
			credentials.parse = func(context.Context, string, string) (*session.ByJwt, error) {
				return nil, errors.New("synthetic signature rejection")
			}
		case "revoked":
			credentials.validate = func(context.Context, *session.ByJwt, bool) error { return errors.New("synthetic revoked parent") }
		case "network":
			claims.NetworkId = server.NewId()
		case "user":
			claims.UserId = server.NewId()
		case "client":
			foreign := server.NewId()
			claims.ClientId = &foreign
		case "device":
			claims.DeviceId = nil
		case "source":
			source = connect.NewId()
		case "reauth":
			args.ClientId = &source
		case "canceled":
			cancel()
		}
		_, err := credentials.AuthNetworkClient(ctx, args)
		cancel()
		if err == nil || mints != 0 {
			t.Fatalf("%s reached the client mint", scenario)
		}
	}
}

func TestProviderEgressCredentialsRetireFencesParentAndNetwork(t *testing.T) {
	credentials, _ := providerEgressCredentialFixture(t)
	child := connect.NewId()
	retirements := 0
	credentials.retire = func(args *model.RemoveNetworkClientArgs, s *session.ClientSession) (*model.RemoveNetworkClientResult, error) {
		retirements++
		if args.ClientId != server.Id(child) || s.ByJwt.NetworkId != credentials.networkId || s.ByJwt.ClientId == nil || *s.ByJwt.ClientId != credentials.clientId {
			t.Error("retirement lost the existing model network/parent fence")
		}
		return &model.RemoveNetworkClientResult{}, nil
	}
	if _, err := credentials.RemoveNetworkClient(t.Context(), &connect.RemoveNetworkClientArgs{ClientId: connect.Id(credentials.clientId)}); err == nil || retirements != 0 {
		t.Fatal("derived cleanup revoked the durable parent")
	}
	if _, err := credentials.RemoveNetworkClient(t.Context(), &connect.RemoveNetworkClientArgs{ClientId: child}); err != nil || retirements != 1 {
		t.Fatal("derived cleanup bypassed the existing network-scoped model")
	}
	credentials.retire = func(*model.RemoveNetworkClientArgs, *session.ClientSession) (*model.RemoveNetworkClientResult, error) {
		return &model.RemoveNetworkClientResult{Error: &model.RemoveNetworkClientError{Message: "synthetic foreign network"}}, nil
	}
	if _, err := credentials.RemoveNetworkClient(t.Context(), &connect.RemoveNetworkClientArgs{ClientId: connect.NewId()}); err == nil {
		t.Fatal("model ownership refusal became successful retirement")
	}
}

func TestProviderEgressCredentialsErrorsDoNotRetryOrCountSuccess(t *testing.T) {
	for _, scenario := range []string{"returned", "panic", "refused", "empty"} {
		credentials, _ := providerEgressCredentialFixture(t)
		mints := 0
		credentials.mint = func(*model.AuthNetworkClientArgs, *session.ClientSession) (*model.AuthNetworkClientResult, error) {
			mints++
			switch scenario {
			case "returned":
				return nil, context.DeadlineExceeded
			case "panic":
				panic(context.Canceled)
			case "refused":
				return &model.AuthNetworkClientResult{Error: &model.AuthNetworkClientError{Message: "synthetic refusal"}}, nil
			default:
				return &model.AuthNetworkClientResult{}, nil
			}
		}
		source := connect.Id(credentials.clientId)
		if _, err := credentials.AuthNetworkClient(t.Context(), &connect.AuthNetworkClientArgs{SourceClientId: &source}); err == nil || mints != 1 {
			t.Fatalf("%s was swallowed or retried", scenario)
		}
	}
}

func TestProviderEgressCredentialsMetricsStayFixed(t *testing.T) {
	recordProviderEgressCredentialResult("synthetic.unrecognized.example", errors.New("synthetic error"))
	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(egressProbeCredentialsTotal)
	families, err := registry.Gather()
	if err != nil || len(families) != 1 || len(families[0].Metric) != 8 {
		t.Fatal("internal credential telemetry lost its fixed eight cells")
	}
	for _, metric := range families[0].Metric {
		if len(metric.Label) != 2 || metric.Counter == nil {
			t.Fatal("credential telemetry gained identity labels or changed type")
		}
	}
}
