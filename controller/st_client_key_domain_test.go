// Actual provider settings, key publication, signed registration and close
// accounting share one policy domain through the production Http/controller path.
package controller

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/urfoundation/sn/miner"
	"github.com/urfoundation/sn/payoutartifact"
	snprotocol "github.com/urfoundation/sn/protocol"
	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
	"google.golang.org/protobuf/proto"
)

// This joins the actual launch parser/settings and Core Http producer to the
// independently signed Server history, then verifies the same original in SQL.
func TestProviderDomainActualEnrollmentRotationAndClosedWork(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		fixture, credential, _ := newStClientKeyHistoryControllerFixture(t)
		owner, stopOwner := context.WithTimeout(t.Context(), 45*time.Second)
		defer stopOwner()
		domainBytes, err := json.Marshal(fixture.domain)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "provider-domain.json")
		if err := os.WriteFile(path, domainBytes, 0644); err != nil {
			t.Fatal(err)
		}
		domainHash, err := miner.ReadProviderCloseReportDomain(path)
		wantDomain, _ := fixture.domain.Digest()
		if err != nil || domainHash != wantDomain {
			t.Fatal("actual provider parser changed enrollment domain", err)
		}
		closed := make(chan *protocol.CloseContract, 4)
		endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/hello" {
				w.WriteHeader(http.StatusOK)
				return
			}
			if r.Method != http.MethodPost || r.URL.Path != "/connect/control" {
				http.NotFound(w, r)
				return
			}
			raw, err := io.ReadAll(io.LimitReader(r.Body, 64*1024))
			if err != nil {
				http.Error(w, "synthetic bounded request read", 400)
				return
			}
			r.Body.Close()
			r.Body = io.NopCloser(bytes.NewReader(raw))
			var args ConnectControlArgs
			var pack protocol.Pack
			var reports []*protocol.CloseContract
			if json.Unmarshal(raw, &args) == nil {
				wire, err := base64.StdEncoding.DecodeString(args.Pack)
				if err == nil && proto.Unmarshal(wire, &pack) == nil {
					for _, frame := range pack.Frames {
						message, err := connect.FromFrame(frame)
						if report, ok := message.(*protocol.CloseContract); err == nil && ok {
							reports = append(reports, proto.Clone(report).(*protocol.CloseContract))
						}
					}
				}
			}
			router.WrapWithInputRequireClient(ConnectControl, w, r)
			for _, report := range reports {
				select {
				case closed <- report:
				case <-owner.Done():
				}
			}
		}))
		defer endpoint.Close()
		strategy := connect.NewClientStrategyWithDefaults(owner)
		defer strategy.Close()
		control := connect.NewApiOutOfBandControl(owner, strategy, credential.Sign(), endpoint.URL)
		settings := miner.ProviderDeviceSettings(domainHash).ClientSettings
		settings.ControlPingTimeout = 0
		settings.Log = connect.NewNoopLogger()
		settings.EncryptionSettings.Mode = connect.EncryptionModeOff
		settings.ClientKeySeed = bytes.Repeat([]byte{51}, ed25519.SeedSize)
		// This existing opt-in exposes processed completion to this deterministic
		// test. The provider's normal native publisher uses the same wire message.
		settings.ClientKeyRegistrationRequired = true
		clientCtx, cancelClient := context.WithCancel(owner)
		client := connect.NewClient(clientCtx, connect.Id(*credential.ClientId), control, &settings)
		defer func() {
			cancelClient()
			join, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := client.CloseAndWait(join); err != nil {
				t.Error(err)
			}
			if err := control.CloseAndWait(join); err != nil {
				t.Error(err)
			}
		}()
		if client.ClientKeyManager() == nil {
			t.Fatal("actual provider key manager did not start")
		}
		manager := client.ClientKeyManager()
		if err := manager.WaitForRegistration(owner); err != nil {
			t.Fatal("actual provider domain did not reach signed enrollment", err)
		}
		settings.ContractManagerSettings.CloseReportDomainHash[0]++
		if err := manager.SetSeed(bytes.Repeat([]byte{52}, ed25519.SeedSize)); err != nil {
			t.Fatal(err)
		}
		if err := manager.WaitForRegistration(owner); err != nil {
			t.Fatal("actual provider rotation lost its original enrollment domain", err)
		}
		history, err := model.LoadStClientKeyHistory(owner, fixture.domain, *credential.ClientId, 4, 32*1024)
		if err != nil || len(history) != 2 || history[1].Registration.PublicKey != [32]byte(manager.PublicKey()) {
			t.Fatal("actual key enrollment and rotation were not retained in original domain", len(history), err)
		}
		payer := server.NewId()
		model.Testing_CreateDevice(owner, credential.NetworkId, server.NewId(), payer, "synthetic payer", "test")
		contractId, err := model.CreateContractNoEscrow(owner, credential.NetworkId, payer, credential.NetworkId, *credential.ClientId, 1000)
		if err != nil {
			t.Fatal(err)
		}
		start := server.NowUtc().Add(-time.Minute)
		cancelClient()
		if err := client.CloseAndWait(owner); err != nil {
			t.Fatal(err)
		}
		client.ContractManager().CloseContract(connect.Id(contractId), 121, 7)
		var report *protocol.CloseContract
		select {
		case report = <-closed:
		case <-owner.Done():
			t.Fatal("actual signed provider close did not finish its Http controller", owner.Err())
		}
		original, err := protocol.DecodeOriginalCloseReport(report.OriginalReport)
		if err != nil || original.DomainHash != wantDomain || original.PublicKey != history[1].Registration.PublicKey || original.ClientId != [16]byte(*credential.ClientId) {
			t.Fatal("provider close departed from the actually enrolled domain/key", err)
		}
		inventory, err := protocol.DecodeOriginalCloseInventory(report.OriginalInventory)
		if err != nil || !inventory.Matches(original) || inventory.Sequence != 1 || inventory.CumulativeAckedBytes != 121 || !inventory.Terminal {
			t.Fatal("actual provider Http close lost its original inventory companion", err)
		}
		if err := CloseContract(owner, payer, &protocol.CloseContract{ContractId: contractId.Bytes(), ReportId: server.NewId().Bytes(), AckedByteCount: 121}); err != nil {
			t.Fatal(err)
		}
		usages, census, err := model.GetStEpochProviderUsageCensus(owner, 17, start, start.Add(time.Hour))
		if err != nil || census == nil || len(census.Records) != 1 || len(usages) != 1 || usages[0].ClientId != *credential.ClientId || usages[0].NetworkId != credential.NetworkId || usages[0].PayoutByteCount != 121 {
			t.Fatal("enrolled provider identity did not reach exact completed-work census", usages, err)
		}
		var originals payoutartifact.ClosedWorkReports
		if err := json.Unmarshal(census.Records[0].OriginalReports, &originals); err != nil || originals.Count != 2 {
			t.Fatal("original SQL census omitted complete admitted close identities", originals.Count, err)
		}
		found := false
		for _, retained := range originals.Reports {
			if bytes.Equal(retained.Original, report.OriginalReport) {
				if !bytes.Equal(retained.Inventory, report.OriginalInventory) {
					t.Fatal("actual Http accounting did not retain the signed original inventory")
				}
				registration, err := snprotocol.DecodeClientKeyRegistration(retained.KeyRegistration)
				if err != nil || registration.Domain != fixture.domain || registration.ClientID != [16]byte(*credential.ClientId) || registration.PublicKey != original.PublicKey || !bytes.Equal(retained.KeyRegistration, history[1].RegistrationBytes) {
					t.Fatal("closed work substituted current or foreign enrolled identity", err)
				}
				found = true
			}
		}
		if !found {
			t.Fatal("closed-work census omitted actual enrolled provider original")
		}
	})
}

// Every namespace dimension is checked against the captured owner, before
// actual reads can sign anything. Rejected peers cannot erase the prior key.
func TestClientKeyEnrollmentRefusesEveryForeignDomainAndKeepsLegacy(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		fixture, credential, _ := newStClientKeyHistoryControllerFixture(t)
		clientId := *credential.ClientId
		key := bytes.Repeat([]byte{53}, 32)
		domain, _ := fixture.domain.Digest()
		if err := SetClientKey(t.Context(), clientId, &protocol.ClientKey{PublicKey: key, HistoryDomainHash: domain[:]}); err != nil {
			t.Fatal(err)
		}
		for _, change := range []func(*snprotocol.ClientKeyHistoryDomain){
			func(d *snprotocol.ClientKeyHistoryDomain) { d.ChainID++ },
			func(d *snprotocol.ClientKeyHistoryDomain) { d.GenesisHash[0]++ },
			func(d *snprotocol.ClientKeyHistoryDomain) { d.Netuid++ },
			func(d *snprotocol.ClientKeyHistoryDomain) { d.Coordinator[0]++ },
			func(d *snprotocol.ClientKeyHistoryDomain) { d.SettlementVault[0]++ },
			func(d *snprotocol.ClientKeyHistoryDomain) { d.DeploymentIDHash[0]++ },
			func(d *snprotocol.ClientKeyHistoryDomain) { d.PolicyHash[0]++ },
			func(d *snprotocol.ClientKeyHistoryDomain) { d.NoID++ },
		} {
			foreign := fixture.domain
			change(&foreign)
			hash, err := foreign.Digest()
			if err != nil {
				t.Fatal(err)
			}
			fixture.fault = "chain"
			err = SetClientKey(t.Context(), clientId, &protocol.ClientKey{PublicKey: bytes.Repeat([]byte{54}, 32), HistoryDomainHash: hash[:]})
			fixture.fault = ""
			if !errors.Is(err, ErrStClientKeyDomain) {
				t.Fatal("foreign domain reached chain or key mutation instead of scoped refusal", err)
			}
		}
		for _, raw := range [][]byte{{1}, make([]byte, 31), make([]byte, 33), make([]byte, 32)} {
			if err := SetClientKey(t.Context(), clientId, &protocol.ClientKey{PublicKey: key, HistoryDomainHash: raw}); !errors.Is(err, ErrStClientKeyDomain) {
				t.Fatal("malformed or zero supplied domain acquired enrollment authority", err)
			}
		}
		retained, err := model.GetClientPublicKey(t.Context(), clientId)
		if err != nil || !bytes.Equal(retained, key) {
			t.Fatal("foreign enrollment altered the original accepted key", err)
		}
		legacy := bytes.Repeat([]byte{55}, 32)
		if err := SetClientKey(t.Context(), clientId, &protocol.ClientKey{PublicKey: legacy}); err != nil {
			t.Fatal("rolling legacy enrollment was rejected", err)
		}
		history, err := model.LoadStClientKeyHistory(t.Context(), fixture.domain, clientId, 4, 32*1024)
		if err != nil || len(history) != 2 || history[1].Registration.PublicKey != [32]byte(legacy) {
			t.Fatal("failed optional namespace operations blocked healthy legacy history", len(history), err)
		}
	})
}

// A wrong-domain operation in a real shared frame batch cannot suppress the
// healthy following key. Canceled owner state is not a domain-integrity claim.
func TestClientKeyEnrollmentScopedBatchCancellationAndRecovery(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		fixture, credential, _ := newStClientKeyHistoryControllerFixture(t)
		domain, _ := fixture.domain.Digest()
		foreign := domain
		foreign[0]++
		messages := []*protocol.ClientKey{{PublicKey: bytes.Repeat([]byte{56}, 32), HistoryDomainHash: foreign[:]}, {PublicKey: bytes.Repeat([]byte{57}, 32), HistoryDomainHash: domain[:]}}
		var frames []*protocol.Frame
		for _, message := range messages {
			frame, err := connect.ToFrame(message, connect.DefaultProtocolVersion)
			if err != nil {
				t.Fatal(err)
			}
			frames = append(frames, frame)
		}
		defer returnConnectControlFrames(frames)
		results, err := ConnectControlFrames(t.Context(), *credential.ClientId, frames, connect.DefaultContractManagerSettings())
		returnConnectControlFrames(results)
		if !errors.Is(err, ErrStClientKeyDomain) {
			t.Fatal("shared frame lost exact domain disagreement", err)
		}
		key, err := model.GetClientPublicKey(t.Context(), *credential.ClientId)
		if err != nil || !bytes.Equal(key, messages[1].PublicKey) {
			t.Fatal("wrong-domain frame suppressed its healthy enrollment sibling", err)
		}
		canceled, cancel := context.WithCancel(t.Context())
		cancel()
		err = SetClientKey(canceled, *credential.ClientId, messages[1])
		if !errors.Is(err, context.Canceled) || errors.Is(err, ErrStClientKeyDomain) {
			t.Fatal("canceled enrollment manufactured namespace contradiction", err)
		}
		if err := SetClientKey(t.Context(), *credential.ClientId, messages[1]); err != nil {
			t.Fatal("healthy same-authority enrollment did not recover", err)
		}
		history, err := model.LoadStClientKeyHistory(t.Context(), fixture.domain, *credential.ClientId, 4, 32*1024)
		if err != nil || len(history) != 1 {
			t.Fatal("duplicate/canceled key requests changed original generation census", len(history), err)
		}
	})
}
