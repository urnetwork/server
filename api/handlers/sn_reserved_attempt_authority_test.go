// Signed activation ABI and native SCALE responses feed the actual production
// admission readers over HTTP. No fixture can return an eligibility verdict.
package handlers

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/centrifuge/go-substrate-rpc-client/v4/types"
	"github.com/centrifuge/go-substrate-rpc-client/v4/types/codec"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	gethrpc "github.com/ethereum/go-ethereum/rpc"

	"github.com/urfoundation/sn/crv4"
	"github.com/urfoundation/sn/protocol"
	"github.com/urfoundation/sn/stabi"
	"github.com/urfoundation/sn/validator"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// All values are frozen before either admission lifecycle starts. The two
// destination caches own separate native/EVM clients and real refresh loops.
type snReservedAttemptAuthority struct {
	record     protocol.ValidatorEvidenceActivation
	key        ed25519.PrivateKey
	digest     [32]byte
	deployment validator.ValidatorUploadDeployment
	endpoint   *httptest.Server
	metadata   string
	storage    map[string]any
	metagraph  string
	now        time.Time
}

// This exact runtime metadata is independently hashed by CRV4 at both native
// snapshots. Permit and stake use the actual v454 layout and storage hashers.
func newSnReservedAttemptAuthority(t testing.TB) *snReservedAttemptAuthority {
	t.Helper()
	hotkey, err := crv4.KeypairFromSeed([32]byte{0x31})
	if err != nil {
		t.Fatal(err)
	}
	key := ed25519.NewKeyFromSeed(append([]byte{0x41}, make([]byte, 31)...))
	self := &snReservedAttemptAuthority{key: key, storage: make(map[string]any), now: time.Now().Truncate(time.Second)}
	self.record = protocol.ValidatorEvidenceActivation{Domain: protocol.ValidatorEvidenceActivationDomain{ChainID: 945, GenesisHash: [32]byte{1}, Netuid: 521,
		Coordinator: [20]byte{0x12}, SettlementVault: [20]byte{0x13}, DeploymentIDHash: sha256.Sum256([]byte("reserved-http-fixture")), PolicyHash: [32]byte{0x15}, Epoch: 7},
		Hotkey: hotkey.PublicKey(), NoID: 1, FirstSequence: 1, NativeBlock: 100, NativeHash: [32]byte{2}, EVMBlock: 1000, EVMHash: [32]byte(common.BigToHash(big.NewInt(1000)))}
	copy(self.record.VPK[:], key[ed25519.SeedSize:])
	self.digest, err = self.record.Digest()
	if err != nil {
		t.Fatal(err)
	}
	vpkSignature, err := self.record.SignVPK(key)
	if err != nil {
		t.Fatal(err)
	}
	hotkeySignature, err := hotkey.Sign(self.digest[:])
	if err != nil {
		t.Fatal(err)
	}
	if err := self.record.Verify(self.record, vpkSignature, hotkeySignature); err != nil {
		t.Fatal(err)
	}
	metadata := types.NewMetadataV14()
	metadata.MagicNumber = types.MagicNumber
	metadata.AsMetadataV14.Pallets = []types.PalletMetadataV14{{Name: "SubtensorModule", HasStorage: true, Storage: types.StorageMetadataV14{Prefix: "SubtensorModule"}}}
	identity := types.StorageHasherV10{IsIdentity: true}
	account := types.StorageHasherV10{IsBlake2_128Concat: true}
	netuid, uid := binary.LittleEndian.AppendUint16(nil, 521), binary.LittleEndian.AppendUint16(nil, 1)
	for _, field := range []struct {
		name     string
		optional bool
		hashers  []types.StorageHasherV10
		args     [][]byte
		value    any
	}{
		{name: "SubnetworkN", hashers: []types.StorageHasherV10{identity}, args: [][]byte{netuid}, value: []byte{3, 0}},
		{name: "Keys", hashers: []types.StorageHasherV10{identity, identity}, args: [][]byte{netuid, uid}, value: self.record.Hotkey[:]},
		{name: "Uids", optional: true, hashers: []types.StorageHasherV10{identity, account}, args: [][]byte{netuid, self.record.Hotkey[:]}, value: uid},
		{name: "Owner", hashers: []types.StorageHasherV10{account}, args: [][]byte{self.record.Hotkey[:]}, value: append([]byte{12}, make([]byte, 31)...)},
		{name: "TotalHotkeyAlpha", hashers: []types.StorageHasherV10{account, identity}, args: [][]byte{self.record.Hotkey[:], netuid}, value: binary.LittleEndian.AppendUint64(nil, 9000000000000000)},
		{name: "ValidatorPermit", hashers: []types.StorageHasherV10{identity}, args: [][]byte{netuid}, value: []byte{12, 0, 1, 0}},
		{name: "SubnetOwnerHotkey", hashers: []types.StorageHasherV10{identity}, args: [][]byte{netuid}},
		{name: "StakeThreshold", value: binary.LittleEndian.AppendUint64(nil, 100)},
		{name: "SubnetEpochIndex", hashers: []types.StorageHasherV10{identity}, args: [][]byte{netuid}, value: binary.LittleEndian.AppendUint64(nil, 77)},
	} {
		entry := types.StorageEntryMetadataV14{Name: types.Text(field.name), Modifier: types.StorageFunctionModifierV0{IsOptional: field.optional, IsDefault: !field.optional}}
		if len(field.hashers) == 0 {
			entry.Type = types.StorageEntryTypeV14{IsPlainType: true}
		} else {
			entry.Type = types.StorageEntryTypeV14{IsMap: true, AsMap: types.MapTypeV14{Hashers: field.hashers}}
		}
		metadata.AsMetadataV14.Pallets[0].Storage.Items = append(metadata.AsMetadataV14.Pallets[0].Storage.Items, entry)
		storageKey, err := types.CreateStorageKey(metadata, "SubtensorModule", field.name, field.args...)
		if err != nil {
			t.Fatal(err)
		}
		if value, ok := field.value.([]byte); ok {
			self.storage[storageKey.Hex()] = "0x" + hex.EncodeToString(value)
		} else {
			self.storage[storageKey.Hex()] = nil
		}
	}
	metadata.AsMetadataV14.Pallets = append(metadata.AsMetadataV14.Pallets, types.PalletMetadataV14{Name: "Timestamp", HasStorage: true, Storage: types.StorageMetadataV14{Prefix: "Timestamp", Items: []types.StorageEntryMetadataV14{{Name: "Now", Modifier: types.StorageFunctionModifierV0{IsDefault: true}, Type: types.StorageEntryTypeV14{IsPlainType: true}}}}})
	timeKey, err := types.CreateStorageKey(metadata, "Timestamp", "Now")
	if err != nil {
		t.Fatal(err)
	}
	self.storage[timeKey.Hex()] = "clock"
	self.metadata, err = codec.EncodeToHex(metadata)
	if err != nil {
		t.Fatal(err)
	}
	_, metadataHash, err := crv4.DecodeRuntimeMetadata(self.metadata)
	if err != nil {
		t.Fatal(err)
	}
	self.deployment = validator.ValidatorUploadDeployment{ChainID: 945, GenesisHash: self.record.Domain.GenesisHash, Netuid: 521, Coordinator: self.record.Domain.Coordinator, SettlementVault: self.record.Domain.SettlementVault,
		DeploymentIDHash: self.record.Domain.DeploymentIDHash, Journal: [20]byte{0x33}, RuntimeHash: [32]byte(crypto.Keccak256Hash([]byte{0x60, 0, 0})), DeploymentBlock: 1001, MaximumSubnetUIDs: 3,
		NativeRuntime: crv4.RuntimeArtifactIdentity{Version: crv4.RuntimeVersionIdentity{SpecName: "node-subtensor", SpecVersion: 454, TransactionVersion: 1, StateVersion: 1}, CodeHash: types.Hash{4}.Hex(), MetadataHash: metadataHash}}
	compact := func(value uint64) []byte {
		data, err := codec.Encode(types.NewUCompactFromUInt(value))
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	data := append([]byte{1}, compact(521)...)
	for index := 1; index <= 76; index++ {
		if index != 30 && index != 52 && index != 57 && index != 69 {
			data = append(data, 0)
			continue
		}
		data = append(data, 1, 12)
		switch index {
		case 52:
			for _, key := range [][32]byte{{31}, self.record.Hotkey, {33}} {
				data = append(data, key[:]...)
			}
		case 57:
			data = append(data, 0, 1, 0)
		case 69:
			for _, stake := range []uint64{0, 150, 0} {
				data = append(data, compact(stake)...)
			}
		}
	}
	self.metagraph = "0x" + hex.EncodeToString(data)
	self.endpoint = httptest.NewServer(self)
	t.Cleanup(self.endpoint.Close)
	return self
}

// Each API cache is initialized through its actual production constructor and
// WaitReady observes the real historical/current refresh, not cache allocation.
func (self *snReservedAttemptAuthority) start(t testing.TB, replicaNoID uint64) *controller.StReservedAttemptUpload {
	t.Helper()
	config := &controller.StConfig{Enabled: true, Profile: "testnet", DeploymentId: "reserved-http-fixture", ChainId: 945, GenesisHash: self.record.Domain.GenesisHash, Netuid: 521, NoId: replicaNoID,
		ContractAddress: common.Address(self.record.Domain.Coordinator), SettlementVault: common.Address(self.record.Domain.SettlementVault), RpcUrls: []string{self.endpoint.URL}}
	config.ReservedAttemptUpload = &controller.StReservedAttemptUploadConfig{NativeRPCURLs: []string{self.endpoint.URL}, Budget: model.StReservedAttemptUploadBudget{RetryRequestsPerHour: 2, ObjectsPerHour: 3, BytesPerHour: 256},
		Admission: validator.ValidatorUploadAdmissionConfig{Deployment: self.deployment, ReplicaNoID: replicaNoID, MaximumContextBytes: 65536, MaximumOwners: 2, BlocksPerRange: 200, MaximumRanges: 1, MaximumEventsPerRange: 4,
			RefreshSeconds: 10, MaximumRefreshSeconds: 20, MaximumHeadAgeSeconds: 60, MaximumIntentSeconds: 25, FreshActivePerOwner: 1, RetryActivePerOwner: 1}}
	owner, err := controller.NewStReservedAttemptUploadWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(owner.Close)
	if err := owner.WaitReady(t.Context()); err != nil {
		t.Fatalf("real protected API authority prerequisite: %v", err)
	}
	return owner
}

// Exact JSON-RPC framing supports the actual EVM batch transport and native
// serial reader. Unknown methods and unpinned arguments produce real errors.
func (self *snReservedAttemptAuthority) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	var wire json.RawMessage
	if json.NewDecoder(r.Body).Decode(&wire) != nil {
		http.Error(w, "invalid RPC", 400)
		return
	}
	type call struct {
		ID     json.RawMessage   `json:"id"`
		Method string            `json:"method"`
		Params []json.RawMessage `json:"params"`
	}
	reply := func(item call) map[string]any {
		value, err := self.result(item.Method, item.Params)
		if err != nil {
			return map[string]any{"jsonrpc": "2.0", "id": item.ID, "error": map[string]any{"code": -32000, "message": err.Error()}}
		}
		return map[string]any{"jsonrpc": "2.0", "id": item.ID, "result": value}
	}
	w.Header().Set("Content-Type", "application/json")
	if len(wire) > 0 && wire[0] == '[' {
		var calls []call
		if json.Unmarshal(wire, &calls) != nil {
			http.Error(w, "invalid batch", 400)
			return
		}
		responses := make([]map[string]any, 0, len(calls))
		for _, item := range calls {
			responses = append(responses, reply(item))
		}
		_ = json.NewEncoder(w).Encode(responses)
	} else {
		var item call
		if json.Unmarshal(wire, &item) != nil {
			http.Error(w, "invalid call", 400)
			return
		}
		_ = json.NewEncoder(w).Encode(reply(item))
	}
}

// Historical and current snapshots use distinct native identities. The public
// runtime descriptor is independently checked against these exact fixtures.
func (self *snReservedAttemptAuthority) result(method string, args []json.RawMessage) (any, error) {
	argument := func(index int) string {
		if index >= len(args) {
			return ""
		}
		var value string
		_ = json.Unmarshal(args[index], &value)
		return value
	}
	knownNative := func(hash string) bool { return hash == types.Hash{2}.Hex() || hash == types.Hash{3}.Hex() }
	block := func(number uint64) map[string]any {
		return map[string]any{"number": hexutil.EncodeUint64(number), "hash": common.BigToHash(new(big.Int).SetUint64(number)), "timestamp": hexutil.EncodeUint64(uint64(self.now.Unix()))}
	}
	switch method {
	case "eth_chainId":
		return "0x3b1", nil
	case "eth_getBlockByNumber":
		if len(args) != 2 || string(args[1]) != "false" {
			break
		}
		if argument(0) == "finalized" {
			return block(1200), nil
		}
		number, err := hexutil.DecodeUint64(argument(0))
		if err != nil || (number != 1000 && number != 1001 && number != 1200) {
			break
		}
		return block(number), nil
	case "eth_getBlockByHash":
		if len(args) != 2 || string(args[1]) != "false" {
			break
		}
		for _, number := range []uint64{1000, 1001, 1200} {
			if argument(0) == common.BigToHash(new(big.Int).SetUint64(number)).Hex() {
				return block(number), nil
			}
		}
	case "eth_getCode":
		var selector gethrpc.BlockNumberOrHash
		if len(args) != 2 || json.Unmarshal(args[1], &selector) != nil || argument(0) != common.Address(self.deployment.Journal).Hex() || selector.BlockHash == nil || *selector.BlockHash != common.BigToHash(big.NewInt(1200)) || !selector.RequireCanonical {
			break
		}
		return hexutil.Bytes{0x60, 0, 0}, nil
	case "eth_getLogs":
		var filter struct {
			Address common.Address `json:"address"`
			From    string         `json:"fromBlock"`
			To      string         `json:"toBlock"`
			Topics  []common.Hash  `json:"topics"`
		}
		if len(args) != 1 || json.Unmarshal(args[0], &filter) != nil || filter.Address != common.Address(self.deployment.Journal) || filter.From != "0x3e9" || filter.To != "0x4b0" || len(filter.Topics) != 1 || filter.Topics[0] != crypto.Keccak256Hash([]byte("ActivationPublished(bytes32,bytes32,uint64,uint64)")) {
			break
		}
		epoch := common.BigToHash(big.NewInt(7))
		return []map[string]any{{"address": filter.Address, "topics": []common.Hash{filter.Topics[0], common.Hash(self.digest), common.Hash(self.record.Hotkey), common.BigToHash(big.NewInt(1))},
			"data": hexutil.Bytes(epoch[:]), "blockNumber": "0x3e9", "blockHash": common.BigToHash(big.NewInt(1001)), "logIndex": "0x0", "removed": false}}, nil
	case "eth_call":
		return self.evmCall(args)
	case "chain_getFinalizedHead":
		return types.Hash{3}.Hex(), nil
	case "chain_getBlockHash":
		var number uint64
		if len(args) != 1 || json.Unmarshal(args[0], &number) != nil {
			break
		}
		switch number {
		case 0:
			return types.Hash{1}.Hex(), nil
		case 100:
			return types.Hash{2}.Hex(), nil
		case 101:
			return types.Hash{3}.Hex(), nil
		}
	case "chain_getHeader":
		if len(args) != 1 || !knownNative(argument(0)) {
			break
		}
		number := "0x64"
		if argument(0) == (types.Hash{3}).Hex() {
			number = "0x65"
		}
		return map[string]any{"parentHash": types.Hash{1}.Hex(), "number": number, "stateRoot": types.Hash{4}.Hex(), "extrinsicsRoot": types.Hash{5}.Hex(), "digest": map[string]any{"logs": []string{}}}, nil
	case "state_getRuntimeVersion":
		if len(args) == 0 || len(args) == 1 && knownNative(argument(0)) {
			return self.deployment.NativeRuntime.Version, nil
		}
	case "state_getMetadata":
		if len(args) == 0 || len(args) == 1 && knownNative(argument(0)) {
			return self.metadata, nil
		}
	case "state_getStorageHash":
		if len(args) == 2 && argument(0) == "0x3a636f6465" && knownNative(argument(1)) {
			return self.deployment.NativeRuntime.CodeHash, nil
		}
	case "state_getStorage":
		if len(args) != 2 || !knownNative(argument(1)) {
			break
		}
		value, exists := self.storage[argument(0)]
		if !exists {
			break
		}
		if value == "clock" {
			if argument(1) != (types.Hash{3}).Hex() {
				break
			}
			return "0x" + hex.EncodeToString(binary.LittleEndian.AppendUint64(nil, uint64(self.now.UnixMilli()))), nil
		}
		return value, nil
	case "state_call":
		if len(args) == 3 && argument(0) == "SubnetInfoRuntimeApi_get_selective_metagraph" && argument(1) == "0x09021400001e00340039004500" && knownNative(argument(2)) {
			return self.metagraph, nil
		}
	}
	return nil, fmt.Errorf("unrecognized exact fixture RPC %s", method)
}

// All ABI output bytes are generated from the independent deployment and
// genuinely signed record. The canonical hash selector remains mandatory.
func (self *snReservedAttemptAuthority) evmCall(args []json.RawMessage) (any, error) {
	var call map[string]hexutil.Bytes
	var selector gethrpc.BlockNumberOrHash
	if len(args) != 2 || json.Unmarshal(args[0], &call) != nil || json.Unmarshal(args[1], &selector) != nil || selector.BlockHash == nil || selector.BlockNumber != nil || !selector.RequireCanonical {
		return nil, errors.New("fixture view is not hash pinned")
	}
	coordinator, evidence := stabi.NewSTCoordinator(), stabi.NewSTValidatorEvidence()
	coordinatorABI, err := stabi.STCoordinatorMetaData.ParseABI()
	if err != nil {
		return nil, err
	}
	evidenceABI, err := stabi.STValidatorEvidenceMetaData.ParseABI()
	if err != nil {
		return nil, err
	}
	input, target := call["input"], common.BytesToAddress(call["to"])
	if len(call["to"]) != 20 {
		return nil, errors.New("fixture view target differs")
	}
	if *selector.BlockHash == common.BigToHash(big.NewInt(1000)) && target == common.Address(self.record.Domain.Coordinator) {
		for _, view := range []struct {
			input  []byte
			method string
			value  any
		}{
			{input: coordinator.PackCurrentEpoch(), method: "currentEpoch", value: big.NewInt(6)},
			{input: coordinator.PackPolicyAt(big.NewInt(7)), method: "policyAt", value: stabi.STCoordinatorPolicySnapshot{PolicyHash: self.record.Domain.PolicyHash, EffectiveEpoch: 7, EffectiveBlock: 1010, EpochBlocks: 100, EpochDepositCapRao: big.NewInt(1000), CampaignDepositCapRao: big.NewInt(5000)}},
			{input: coordinator.PackOperatorAt(big.NewInt(1), big.NewInt(7)), method: "operatorAt", value: stabi.STCoordinatorOperatorVersion{Active: true, EffectiveEpoch: 7}},
		} {
			if bytes.Equal(input, view.input) {
				value, err := coordinatorABI.Methods[view.method].Outputs.Pack(view.value)
				return hexutil.Bytes(value), err
			}
		}
	}
	if *selector.BlockHash == common.BigToHash(big.NewInt(1200)) {
		if target == common.Address(self.record.Domain.Coordinator) && bytes.Equal(input, coordinator.PackValidatorEvidence()) {
			value, err := coordinatorABI.Methods["validatorEvidence"].Outputs.Pack(common.Address(self.deployment.Journal))
			return hexutil.Bytes(value), err
		}
		if target == common.Address(self.deployment.Journal) {
			for _, view := range []struct {
				input  []byte
				method string
				value  any
			}{
				{input: evidence.PackCoordinator(), method: "coordinator", value: common.Address(self.record.Domain.Coordinator)},
				{input: evidence.PackSettlementVault(), method: "settlementVault", value: common.Address(self.record.Domain.SettlementVault)},
				{input: evidence.PackChainId(), method: "chainId", value: uint64(945)},
				{input: evidence.PackNetuid(), method: "netuid", value: uint16(521)},
				{input: evidence.PackGenesisHash(), method: "genesisHash", value: self.record.Domain.GenesisHash},
				{input: evidence.PackDeploymentIdHash(), method: "deploymentIdHash", value: self.record.Domain.DeploymentIDHash},
				{input: evidence.PackActivation(self.digest), method: "activation", value: stabi.STValidatorEvidenceActivation{Record: stabi.ValidatorEvidenceActivationRecordFromProtocol(self.record), PublishedBlock: 1001}},
			} {
				if bytes.Equal(input, view.input) {
					value, err := evidenceABI.Methods[view.method].Outputs.Pack(view.value)
					return hexutil.Bytes(value), err
				}
			}
		}
	}
	return nil, errors.New("unrecognized exact fixture ABI view")
}
