// The real authenticated key-control/controller/model/publication path reads
// deterministic raw RPC bytes. Faults change actual ABI or canonical headers.
package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	gethrpc "github.com/ethereum/go-ethereum/rpc"
	"github.com/urfoundation/sn/v2026/protocol"
	"github.com/urfoundation/sn/v2026/stabi"
	"github.com/urfoundation/sn/v2026/validator"
	connectprotocol "github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/jwt"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/router"
	"github.com/urnetwork/server/v2026/session"
)

// Values are changed only between fully joined requests, not under RPC reads.
type stClientKeyHistoryRPCFixture struct {
	domain   protocol.ClientKeyHistoryDomain
	boundary protocol.ClientKeyEffectiveBoundary
	root     common.Address
	fault    string
}

// The chain ID is an independent raw-RPC observation, never a caller verdict.
func (self *stClientKeyHistoryRPCFixture) ChainId(context.Context) (hexutil.Uint64, error) {
	if self.fault == "chain" {
		return hexutil.Uint64(self.domain.ChainID + 1), nil
	}
	return hexutil.Uint64(self.domain.ChainID), nil
}

// The endpoint exposes native RPC independently while EVM block zero is null.
func (self *stClientKeyHistoryRPCFixture) GetBlockHash(_ context.Context, block uint64) (*common.Hash, error) {
	if block != 0 || self.fault == "native-unavailable" {
		return nil, errors.New("native identity method unavailable")
	}
	if self.fault == "native-null" {
		return nil, nil
	}
	hash := common.Hash(self.domain.GenesisHash)
	if self.fault == "native-wrong" {
		hash[1]++
	}
	return &hash, nil
}

// Raw Subtensor-style hashes are explicit; no Ethereum header reconstruction.
func (self *stClientKeyHistoryRPCFixture) GetBlockByNumber(_ context.Context, tag gethrpc.BlockNumber, full bool) (map[string]any, error) {
	if full {
		return nil, errors.New("unexpected full block")
	}
	block, hash := self.boundary.Block, self.boundary.Hash
	if tag == 0 {
		return nil, nil
	} else if tag != gethrpc.FinalizedBlockNumber && uint64(tag) != block {
		return nil, errors.New("unknown fixture block")
	}
	if self.fault == "canonical" && tag != gethrpc.FinalizedBlockNumber && tag != 0 {
		hash[1]++
	}
	return map[string]any{"number": hexutil.EncodeUint64(block), "hash": common.Hash(hash), "timestamp": "0x65000000"}, nil
}

// Every authority field is encoded by the actual checked-in coordinator ABI.
func (self *stClientKeyHistoryRPCFixture) Call(_ context.Context, call map[string]hexutil.Bytes, block gethrpc.BlockNumberOrHash) (hexutil.Bytes, error) {
	if block.BlockHash == nil || block.BlockNumber != nil || !block.RequireCanonical || *block.BlockHash != common.Hash(self.boundary.Hash) {
		return nil, errors.New("wrong canonical authority selector")
	}
	data := call["data"]
	if len(data) == 0 {
		data = call["input"]
	}
	coordinator := stabi.NewSTCoordinator()
	anchor := common.Address{0x77}
	if common.BytesToAddress(call["to"]) == anchor {
		companion := stabi.NewSTValidatorEvidence()
		fields := []struct {
			data  []byte
			value [32]byte
		}{
			{data: companion.PackCoordinator(), value: [32]byte(common.BytesToHash(self.domain.Coordinator[:]))},
			{data: companion.PackSettlementVault(), value: [32]byte(common.BytesToHash(self.domain.SettlementVault[:]))},
			{data: companion.PackChainId(), value: [32]byte(common.BigToHash(new(big.Int).SetUint64(self.domain.ChainID)))},
			{data: companion.PackNetuid(), value: [32]byte(common.BigToHash(new(big.Int).SetUint64(uint64(self.domain.Netuid))))},
			{data: companion.PackGenesisHash(), value: self.domain.GenesisHash},
			{data: companion.PackDeploymentIdHash(), value: self.domain.DeploymentIDHash},
		}
		for _, field := range fields {
			if bytes.Equal(data, field.data) {
				if self.fault == "companion" {
					field.value[1]++
				}
				return field.value[:], nil
			}
		}
		return nil, errors.New("unknown companion field")
	}
	if common.BytesToAddress(call["to"]) != self.domain.Coordinator {
		return nil, errors.New("wrong coordinator address")
	}
	parsed, err := stabi.STCoordinatorMetaData.ParseABI()
	if err != nil {
		return nil, err
	}
	policyHash, root := self.domain.PolicyHash, self.root
	if self.fault == "policy" {
		policyHash[1]++
	}
	if self.fault == "root" {
		root[1]++
	}
	epoch := new(big.Int).SetUint64(self.boundary.Epoch)
	methods := []struct {
		method string
		data   []byte
		value  any
	}{
		{method: "validatorEvidence", data: coordinator.PackValidatorEvidence(), value: anchor},
		{method: "currentEpoch", data: coordinator.PackCurrentEpoch(), value: epoch},
		{method: "netuid", data: coordinator.PackNetuid(), value: self.domain.Netuid},
		{method: "settlementVault", data: coordinator.PackSettlementVault(), value: self.domain.SettlementVault},
		{method: "policyAt", data: coordinator.PackPolicyAt(epoch), value: stabi.STCoordinatorPolicySnapshot{PolicyHash: policyHash, EpochDepositCapRao: big.NewInt(1), CampaignDepositCapRao: big.NewInt(1)}},
		{method: "operatorAt", data: coordinator.PackOperatorAt(new(big.Int).SetUint64(self.domain.NoID), epoch), value: stabi.STCoordinatorOperatorVersion{RootSigner: root, Active: self.fault != "inactive"}},
	}
	for _, method := range methods {
		if bytes.Equal(data, method.data) {
			encoded, err := parsed.Methods[method.method].Outputs.Pack(method.value)
			if self.fault == "trailing" {
				encoded = append(encoded, make([]byte, 32)...)
			}
			return encoded, err
		}
	}
	return nil, errors.New("unknown authority method")
}

// Installs actual concrete runtime owners; the only external fixture is raw
// Ethereum JSON-RPC. Each private signer and blob store belongs to this test.
func newStClientKeyHistoryControllerFixture(t testing.TB) (*stClientKeyHistoryRPCFixture, *jwt.ByJwt, *StConfig) {
	t.Helper()
	root, err := crypto.HexToECDSA(strings.Repeat("12", 32))
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := crypto.HexToECDSA(strings.Repeat("13", 32))
	if err != nil {
		t.Fatal(err)
	}
	domain := protocol.ClientKeyHistoryDomain{ChainID: 945, GenesisHash: [32]byte{1}, Netuid: 521, Coordinator: common.Address{2}, SettlementVault: common.Address{3}, DeploymentIDHash: sha256.Sum256([]byte("key-history-test")), PolicyHash: [32]byte{4}, NoID: 1}
	fixture := &stClientKeyHistoryRPCFixture{domain: domain, boundary: protocol.ClientKeyEffectiveBoundary{Block: 100, Hash: [32]byte{5}}, root: crypto.PubkeyToAddress(root.PublicKey)}
	rpc := gethrpc.NewServer()
	if err := rpc.RegisterName("eth", fixture); err != nil {
		t.Fatal(err)
	}
	if err := rpc.RegisterName("chain", fixture); err != nil {
		t.Fatal(err)
	}
	httpRPC := httptest.NewServer(rpc)
	cfg := &StConfig{Enabled: true, ChainId: domain.ChainID, GenesisHash: domain.GenesisHash, Netuid: uint64(domain.Netuid), ContractAddress: domain.Coordinator, SettlementVault: domain.SettlementVault, DeploymentId: "key-history-test", PolicyHash: domain.PolicyHash, NoId: domain.NoID, RootKey: root, ArtifactKey: artifact, RpcUrls: []string{httpRPC.URL}}
	client := &CoreStClient{cfg: cfg, coordinator: stabi.NewSTCoordinator(), clients: map[string]*ethclient.Client{}, clientKeyRegistrations: newStClientKeyRegistrationCohorts(0)}
	SetStConfig(cfg)
	SetStClient(client)
	t.Cleanup(func() {
		for _, rpcClient := range client.clients {
			rpcClient.Close()
		}
		SetStClient(nil)
		SetStConfig(nil)
		httpRPC.Close()
		rpc.Stop()
	})
	pop := server.Vault.PushSimpleResource("minio.yml", []byte(fmt.Sprintf("authority: local\npath: %s\nprefix: key-history\nmax_bytes: %d\n", t.TempDir(), 32*1024*1024)))
	t.Cleanup(pop)
	networkID, userID, clientID, deviceID := server.NewId(), server.NewId(), server.NewId(), server.NewId()
	model.Testing_CreateNetwork(t.Context(), networkID, "key-history-"+networkID.String(), userID)
	model.Testing_CreateDevice(t.Context(), networkID, deviceID, clientID, "key-history", "test")
	credential := jwt.NewByJwt(networkID, userID, "key-history", false, false).Client(deviceID, clientID)
	return fixture, credential, cfg
}

// The real validator HTTP reader consumes exact independently signed bytes
// from the actual controller, SQL history and immutable blob publication.
func TestStClientKeyHistoryActualControlAndValidatorHTTPReader(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		fixture, credential, _ := newStClientKeyHistoryControllerFixture(tb)
		key := bytes.Repeat([]byte{9}, 32)
		if err := SetClientKey(tb.Context(), *credential.ClientId, &connectprotocol.ClientKey{PublicKey: key}); err != nil {
			tb.Fatal(err)
		}
		endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			router.WrapWithInputRequireClient(SnClientKeyObservation, w, r)
		}))
		defer endpoint.Close()
		reader, err := validator.NewHTTPClientKeyHistoryReader(endpoint.URL, func() string { return credential.Sign() })
		if err != nil {
			tb.Fatal(err)
		}
		request := protocol.ClientKeyObservationRequest{ClientID: [16]byte(*credential.ClientId), ValidatorHotkey: [32]byte{6}, NativeBlock: 101, NativeHash: [32]byte{7}, NativeEpoch: 2, DecisionBoundary: fixture.boundary, Nonce: [32]byte{8}}
		var original []byte
		for index, next := range [][]byte{key, bytes.Repeat([]byte{10}, 32), nil} {
			if index > 0 {
				if err := SetClientKey(tb.Context(), *credential.ClientId, &connectprotocol.ClientKey{PublicKey: next}); err != nil {
					tb.Fatal(err)
				}
			}
			request.Nonce[1] = byte(index)
			encoded, err := reader.Read(tb.Context(), request, protocol.MaxClientKeyHistoryResponseBytes)
			if err != nil {
				tb.Fatal(err)
			}
			response, err := protocol.DecodeClientKeyHistoryResponse(encoded, protocol.MaxClientKeyHistoryResponseBytes)
			if err != nil || len(response.History) != index+1 {
				tb.Fatalf("real HTTP history census differs: %v", err)
			}
			if index == 0 {
				original = bytes.Clone(response.History[0])
			} else if !bytes.Equal(original, response.History[0]) {
				tb.Fatal("real rotation rewrote the first signed evidence")
			}
			wrapper, err := protocol.DecodeClientKeyEvidence(response.History[index], fixture.domain, protocol.ClientKeyRegistrationEvidenceKind)
			if err != nil {
				tb.Fatal(err)
			}
			registration, err := protocol.DecodeClientKeyRegistration(wrapper.Payload)
			if err != nil || registration.Present != (len(next) > 0) {
				tb.Fatalf("real signed state differs: %v", err)
			}
			wrapper, err = protocol.DecodeClientKeyEvidence(response.Observation, fixture.domain, protocol.ClientKeyObservationEvidenceKind)
			if err != nil {
				tb.Fatal(err)
			}
			observation, err := protocol.DecodeClientKeyObservation(wrapper.Payload)
			if err != nil {
				tb.Fatal(err)
			}
			if err := observation.VerifyRegistration(registration, fixture.domain, request, fixture.root, fixture.root); err != nil {
				tb.Fatal(err)
			}
		}
	})
}

// Real malformed/forked authority reads fail before any SQL generation exists.
func TestStClientKeyHistoryRejectsActualRPCFaultsBeforeMutation(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		fixture, credential, _ := newStClientKeyHistoryControllerFixture(tb)
		for _, fault := range []string{"chain", "canonical", "policy", "root", "inactive", "trailing", "native-unavailable", "native-null", "native-wrong", "companion"} {
			fixture.fault = fault
			if err := SetClientKey(tb.Context(), *credential.ClientId, &connectprotocol.ClientKey{PublicKey: bytes.Repeat([]byte{9}, 32)}); err == nil {
				tb.Fatalf("actual %s RPC fault reached registration", fault)
			}
			server.Db(tb.Context(), func(conn server.PgConn) {
				var count int
				server.Raise(conn.QueryRow(tb.Context(), "SELECT COUNT(*) FROM st_client_key_history WHERE client_id=$1", *credential.ClientId).Scan(&count))
				if count != 0 {
					tb.Fatal("refused chain authority partly committed history")
				}
			})
		}
	})
}

// Cleanup cannot turn the still authentic last positive record into a fresh
// current observation. There is no fake signed tombstone on the reaping path.
func TestStClientKeyHistoryReapedClientCannotSignFreshObservation(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		fixture, credential, _ := newStClientKeyHistoryControllerFixture(tb)
		if err := SetClientKey(tb.Context(), *credential.ClientId, &connectprotocol.ClientKey{PublicKey: bytes.Repeat([]byte{9}, 32)}); err != nil {
			tb.Fatal(err)
		}
		server.Tx(tb.Context(), func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(tb.Context(), "DELETE FROM network_client WHERE client_id=$1", *credential.ClientId))
		})
		request := protocol.ClientKeyObservationRequest{ClientID: [16]byte(*credential.ClientId), ValidatorHotkey: [32]byte{6}, NativeBlock: 101, NativeHash: [32]byte{7}, NativeEpoch: 2, DecisionBoundary: fixture.boundary, Nonce: [32]byte{8}}
		encoded, err := json.Marshal(request)
		if err != nil {
			tb.Fatal(err)
		}
		clientSession := session.Testing_CreateClientSession(tb.Context(), credential)
		defer clientSession.Cancel()
		if result, err := SnClientKeyObservation(&SnClientKeyObservationArgs{ClientID: *credential.ClientId, Request: encoded}, clientSession); err == nil || result != nil {
			tb.Fatal("reaped client obtained a fresh positive observation")
		}
	})
}
