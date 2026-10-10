package strecovery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

type historicalCaptureRequest struct {
	Id     int               `json:"id"`
	Method string            `json:"method"`
	Params []json.RawMessage `json:"params"`
}

// Native roots/proof bytes come from independently generated pinned SDK
// vectors; the fixture's own signed native headers select their root roles.
type historicalCaptureRpc struct {
	t         testing.TB
	fixture   *historicalStorageFixture
	witness   *ReceiptHistoricalNativeStateWitness
	directory string
	calls     []historicalCaptureRequest
	intercept func(historicalCaptureRequest, any) (any, error)
}

func historicalCaptureInputs(t testing.TB, role string) (*historicalStorageFixture, ReceiptHistoricalNativeCaptureConfig, *historicalCaptureRpc) {
	t.Helper()
	fixture := historicalStorageTestFixture(t, 3, []int{-1, 0, 5, 10})
	witness := fixture.witness(role, 1)
	keys := []string{}
	for _, read := range witness.Reads {
		keys = append(keys, read.Key)
	}
	slices.Sort(keys)
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	config := ReceiptHistoricalNativeCaptureConfig{Schema: ReceiptHistoricalNativeCaptureConfigSchema, CollectionHash: witness.CollectionHash, CheckpointHash: witness.CheckpointHash, FinalityHash: witness.FinalityHash,
		Source: "synthetic-storage-archive", RpcUrl: "http://archive.example", EvmBlock: witness.EvmBlock, RootRole: role, Keys: keys, RetryWindowSeconds: 60}
	return fixture, config, &historicalCaptureRpc{t: t, fixture: fixture, witness: witness, directory: directory}
}

func (self *historicalCaptureRpc) configure(client *receiptCollectorRpc) {
	client.wait = func(context.Context, time.Duration) error { return nil }
	client.client.Transport = collectionTestRoundTripper(func(request *http.Request) (*http.Response, error) {
		var payload historicalCaptureRequest
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			return nil, err
		}
		// This transport barrier proves the actual request reservation was
		// synced before any node response, rather than asserted after the fact.
		if _, err := os.Stat(filepath.Join(self.directory, fmt.Sprintf("request-%05d.json", payload.Id))); err != nil {
			self.t.Fatal("storage request reached transport before durable reservation", err)
		}
		self.calls = append(self.calls, payload)
		var result any
		switch payload.Method {
		case "chain_getBlockHash":
			if len(payload.Params) != 1 || string(payload.Params[0]) != "0" {
				self.t.Fatal("capture used unapproved numbered state selector")
			}
			result = self.fixture.finality.checkpoint.Genesis
		case "state_getReadProof", "state_getStorage":
			var hash string
			if len(payload.Params) != 2 || json.Unmarshal(payload.Params[1], &hash) != nil || hash != self.witness.NativeStateBlock.Hash {
				self.t.Fatal("capture moved selected native historical state")
			}
			if payload.Method == "state_getReadProof" {
				var keys []string
				if json.Unmarshal(payload.Params[0], &keys) != nil || len(keys) != len(self.witness.Reads) {
					self.t.Fatal("storage key census changed")
				}
				for _, read := range self.witness.Reads {
					if !slices.Contains(keys, read.Key) {
						self.t.Fatal("capture dropped selected key")
					}
				}
				result = map[string]any{"at": hash, "proof": self.witness.Nodes}
			} else {
				var key string
				if json.Unmarshal(payload.Params[0], &key) != nil {
					self.t.Fatal("malformed storage key")
				}
				found := false
				for _, read := range self.witness.Reads {
					if read.Key == key {
						result, found = read.Value, true
					}
				}
				if !found {
					self.t.Fatal("capture requested an unselected key")
				}
			}
		default:
			self.t.Fatal("capture emitted a method outside its read profile", payload.Method)
		}
		if self.intercept != nil {
			var err error
			result, err = self.intercept(payload, result)
			if err != nil {
				return nil, err
			}
		}
		raw, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": payload.Id, "result": result})
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(raw))}, err
	})
}

func (self *historicalCaptureRpc) capture(ctx context.Context, config ReceiptHistoricalNativeCaptureConfig) (*ReceiptHistoricalNativeCaptureResult, error) {
	f := self.fixture.finality
	return captureReceiptHistoricalNativeState(ctx, f.receipts.archive, f.collection, f.checkpoint, f.proof, config, self.directory, self.configure)
}

func TestReceiptHistoricalNativeCapturePublishesBothRolesAndReplaysOffline(t *testing.T) {
	for _, role := range []string{NativeStorageRootParentExecution, NativeStorageRootChildPostState} {
		t.Run(role, func(t *testing.T) {
			fixture, config, rpc := historicalCaptureInputs(t, role)
			f := fixture.finality
			before := []string{objectDigest(f.receipts.archive), objectDigest(f.collection), objectDigest(f.checkpoint), objectDigest(f.proof)}
			result, err := rpc.capture(t.Context(), config)
			if err != nil {
				t.Fatal(err)
			}
			witness, err := LoadReceiptHistoricalNativeStateWitness(t.Context(), result.Witness)
			if err != nil || witness.NativeStateBlock != rpc.witness.NativeStateBlock || witness.NativeReceiptBlock != rpc.witness.NativeReceiptBlock || len(witness.Reads) != len(config.Keys) {
				t.Fatal("published witness changed historical selection", err)
			}
			replay, err := fixture.verify(t.Context(), witness)
			if err != nil || objectDigest(replay) != objectDigest(result.Reconciliation) || !replay.NativeHeaderStorageVerified || !replay.HistoricalContextVerified {
				t.Fatal("offline witness replay differs", err)
			}
			if replay.AuthorityCheckpointAuthenticated || replay.GenesisAuthenticated || replay.RuntimeSourceAuthenticated || replay.FeeAttributionAuthenticated || replay.PayerBindingAuthenticated || replay.FinalityAuthenticated || replay.ActualFeesReconciled || replay.SpendingAuthorized {
				t.Fatal("storage capture manufactured authority")
			}
			for _, transaction := range replay.FeeContexts.Transactions {
				if transaction.ActualGasDebitRao != nil || transaction.ActualWithdrawalRao != nil || transaction.ActualRefundRao != nil || transaction.Receipt != nil && transaction.Receipt.ActualGasFee != nil {
					t.Fatal("raw storage became an actual fee")
				}
			}
			for _, expected := range rpc.witness.Reads {
				found := false
				for _, actual := range replay.Reads {
					if actual.Key == expected.Key && objectDigest(actual) == objectDigest(expected) {
						found = true
					}
				}
				if !found {
					t.Fatal("capture confused raw value, empty value or authenticated absence")
				}
			}
			info, err := os.Stat(result.Witness.Path)
			if err != nil || info.Mode().Perm() != 0400 {
				t.Fatal("witness was not sealed private", err)
			}
			again, err := captureReceiptHistoricalNativeState(t.Context(), f.receipts.archive, f.collection, f.checkpoint, f.proof, config, rpc.directory, func(*receiptCollectorRpc) { t.Fatal("complete restart opened RPC") })
			if err != nil || objectDigest(result) != objectDigest(again) || !slices.Equal(before, []string{objectDigest(f.receipts.archive), objectDigest(f.collection), objectDigest(f.checkpoint), objectDigest(f.proof)}) {
				t.Fatal("completed restart changed evidence or original custody", err)
			}
			after, _ := os.Stat(result.Witness.Path)
			if !os.SameFile(info, after) {
				t.Fatal("complete replay replaced retained witness")
			}
		})
	}
}

func TestReceiptHistoricalNativeCaptureResumesExactPartialReadsAndBudget(t *testing.T) {
	_, config, rpc := historicalCaptureInputs(t, NativeStorageRootChildPostState)
	if len(config.Keys) < 2 {
		t.Fatal("partial fixture needs multiple keys")
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	rpc.intercept = func(request historicalCaptureRequest, result any) (any, error) {
		if request.Method == "state_getStorage" && string(request.Params[0]) == fmt.Sprintf("%q", config.Keys[1]) {
			cancel()
			return nil, context.Canceled
		}
		return result, nil
	}
	if result, err := rpc.capture(ctx, config); result != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("canceled partial capture succeeded", err)
	}
	retained, err := os.ReadFile(filepath.Join(rpc.directory, "storage-00.json"))
	if err != nil {
		t.Fatal("completed storage value was not retained", err)
	}
	lastId := rpc.calls[len(rpc.calls)-1].Id
	rpc.calls, rpc.intercept = nil, nil
	result, err := rpc.capture(t.Context(), config)
	if err != nil || result == nil || rpc.calls[0].Id != lastId+1 {
		t.Fatal("restart lost spent requests", err)
	}
	for _, call := range rpc.calls {
		if call.Method == "state_getReadProof" || call.Method == "state_getStorage" && string(call.Params[0]) == fmt.Sprintf("%q", config.Keys[0]) {
			t.Fatal("restart refetched completed historical evidence")
		}
	}
	after, _ := os.ReadFile(filepath.Join(rpc.directory, "storage-00.json"))
	if !bytes.Equal(retained, after) {
		t.Fatal("restart changed original raw value")
	}
}

func TestReceiptHistoricalNativeCaptureRetriesSameHashWithDurableDebits(t *testing.T) {
	_, config, rpc := historicalCaptureInputs(t, NativeStorageRootParentExecution)
	failures := 0
	rpc.intercept = func(request historicalCaptureRequest, result any) (any, error) {
		if request.Method == "state_getReadProof" && failures < 5 {
			failures++
			return nil, collectionTestTimeout{}
		}
		return result, nil
	}
	if result, err := rpc.capture(t.Context(), config); err != nil || result == nil || failures != 5 {
		t.Fatal("bounded transient reads failed", err)
	}
	for _, call := range rpc.calls {
		if _, err := os.Stat(filepath.Join(rpc.directory, fmt.Sprintf("read-%05d.json", call.Id))); err != nil {
			t.Fatal("read completion did not preserve response debit", err)
		}
	}
}

func TestReceiptHistoricalNativeCaptureRejectsWrongProofBlockAndRetainsRefusal(t *testing.T) {
	_, config, rpc := historicalCaptureInputs(t, NativeStorageRootParentExecution)
	rpc.intercept = func(request historicalCaptureRequest, result any) (any, error) {
		if request.Method == "state_getReadProof" {
			// Equal proof bytes still cannot replace the explicit exact block.
			result.(map[string]any)["at"] = rpc.witness.NativeReceiptBlock.Hash
		}
		return result, nil
	}
	for range 2 {
		if result, err := rpc.capture(t.Context(), config); err == nil || result != nil || !strings.Contains(err.Error(), "selector") {
			t.Fatal("wrong block proof was accepted", err)
		}
		rpc.intercept = nil
	}
	proofCalls := 0
	for _, call := range rpc.calls {
		if call.Method == "state_getReadProof" {
			proofCalls++
		}
		if call.Method == "state_getStorage" {
			t.Fatal("wrong proof identity admitted storage reads")
		}
	}
	if proofCalls != 1 {
		t.Fatal("restart overwrote rejected proof evidence")
	}
}

func TestReceiptHistoricalNativeCaptureRejectsConflictingValueBeforePublication(t *testing.T) {
	for _, fault := range []string{"value", "missing-node", "malformed-proof", "null-proof"} {
		t.Run(fault, func(t *testing.T) {
			_, config, rpc := historicalCaptureInputs(t, NativeStorageRootChildPostState)
			rpc.intercept = func(request historicalCaptureRequest, result any) (any, error) {
				if request.Method == "state_getStorage" && fault == "value" {
					return "0xffff", nil
				}
				if request.Method == "state_getReadProof" {
					switch fault {
					case "missing-node":
						result.(map[string]any)["proof"] = []string{}
					case "malformed-proof":
						return map[string]any{"at": rpc.witness.NativeStateBlock.Hash, "proof": []string{"0xGG"}}, nil
					case "null-proof":
						result.(map[string]any)["proof"] = nil
					}
				}
				return result, nil
			}
			if result, err := rpc.capture(t.Context(), config); err == nil || result != nil {
				t.Fatal("unproven storage was published", fault)
			}
			if _, err := os.Stat(filepath.Join(rpc.directory, "witness.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failed proof left a published witness")
			}
			if _, err := os.Stat(filepath.Join(rpc.directory, "storage-proof.json")); err != nil {
				t.Fatal("refusal discarded original proof response", err)
			}
		})
	}
}

func TestReceiptHistoricalNativeCaptureRequiresExactContextBeforeOwnership(t *testing.T) {
	for _, fault := range []string{"collection", "checkpoint", "finality", "receipt", "role", "keys", "retry", "route", "missing-parent", "ambiguous"} {
		t.Run(fault, func(t *testing.T) {
			f, config, rpc := historicalCaptureInputs(t, NativeStorageRootParentExecution)
			switch fault {
			case "collection":
				config.CollectionHash = "sha256:" + strings.Repeat("a", 64)
			case "checkpoint":
				config.CheckpointHash = "sha256:" + strings.Repeat("a", 64)
			case "finality":
				config.FinalityHash = "sha256:" + strings.Repeat("a", 64)
			case "receipt":
				config.EvmBlock.Number++
			case "role":
				config.RootRole = "finalized"
			case "keys":
				config.Keys = append(config.Keys, config.Keys[0])
			case "retry":
				config.RetryWindowSeconds = 901
			case "route":
				config.RpcUrl = "https://user:secret@archive.example"
			case "missing-parent", "ambiguous":
				mappings := []int{0, 5, -1, 10}
				if fault == "ambiguous" {
					mappings = []int{-1, 0, 0, 10}
				}
				f = historicalStorageTestFixture(t, 3, mappings)
				rpc.fixture = f
				config.CollectionHash, config.CheckpointHash, config.FinalityHash = f.finality.collection.ContentHash, f.finality.checkpoint.Hash(), objectDigest(f.finality.proof)
			}
			if result, err := rpc.capture(t.Context(), config); err == nil || result != nil {
				t.Fatal("invalid context admitted", fault)
			}
			if len(rpc.calls) != 0 {
				t.Fatal("invalid context reached RPC")
			}
			entries, err := os.ReadDir(rpc.directory)
			if err != nil || len(entries) != 0 {
				t.Fatal("invalid context acquired journal ownership", err)
			}
		})
	}
}

func TestReceiptHistoricalNativeCaptureRejectsForeignGenesis(t *testing.T) {
	_, config, rpc := historicalCaptureInputs(t, NativeStorageRootChildPostState)
	rpc.intercept = func(request historicalCaptureRequest, result any) (any, error) {
		if request.Method == "chain_getBlockHash" {
			foreign := "0x" + strings.Repeat("b", 64)
			if foreign == rpc.fixture.finality.checkpoint.Genesis {
				t.Fatal("foreign genesis fixture must differ")
			}
			return foreign, nil
		}
		return result, nil
	}
	if result, err := rpc.capture(t.Context(), config); err == nil || result != nil || !strings.Contains(err.Error(), "genesis") || len(rpc.calls) != 1 {
		t.Fatal("wrong genesis reached state reads", err)
	}
}

func TestReceiptHistoricalNativeCaptureRefusesScopeChangesOnRestart(t *testing.T) {
	_, config, rpc := historicalCaptureInputs(t, NativeStorageRootParentExecution)
	result, err := rpc.capture(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(result.Witness.Path)
	for _, fault := range []string{"route", "keys", "role"} {
		changed := config
		switch fault {
		case "route":
			changed.RpcUrl = "http://other.example"
		case "keys":
			changed.Keys = changed.Keys[:1]
		case "role":
			changed.RootRole = NativeStorageRootChildPostState
		}
		calls := len(rpc.calls)
		if result, err := rpc.capture(t.Context(), changed); err == nil || result != nil || len(rpc.calls) != calls {
			t.Fatal("restart retargeted admitted capture", fault, err)
		}
	}
	after, _ := os.ReadFile(result.Witness.Path)
	if !bytes.Equal(before, after) {
		t.Fatal("scope refusal rewrote witness")
	}
}

func TestReceiptHistoricalNativeCaptureExhaustedCrashBudgetPreventsNetwork(t *testing.T) {
	_, config, rpc := historicalCaptureInputs(t, NativeStorageRootParentExecution)
	// Eight unanswered maximum-size reservations exceed the 128 MiB lifetime
	// allowance. This state models crashes before durable response completion.
	for index := 1; index <= 8; index++ {
		raw, _ := json.Marshal(finalityCaptureAttempt{RequestHash: digest([]byte(fmt.Sprint(index)))})
		if err := os.WriteFile(filepath.Join(rpc.directory, fmt.Sprintf("request-%05d.json", index)), raw, 0400); err != nil {
			t.Fatal(err)
		}
	}
	if result, err := rpc.capture(t.Context(), config); err == nil || result != nil || !strings.Contains(err.Error(), "budget") || len(rpc.calls) != 0 {
		t.Fatal("restart reset exhausted response budget", err)
	}
}

func TestReceiptHistoricalNativeCaptureRejectsRetainedValidSubset(t *testing.T) {
	_, config, rpc := historicalCaptureInputs(t, NativeStorageRootParentExecution)
	result, err := rpc.capture(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	witness, err := LoadReceiptHistoricalNativeStateWitness(t.Context(), result.Witness)
	if err != nil || len(witness.Reads) < 2 {
		t.Fatal("subset fixture incomplete", err)
	}
	witness.Reads = witness.Reads[:1]
	if _, err := rpc.fixture.verify(t.Context(), witness); err != nil {
		t.Fatal("subset is not independently valid", err)
	}
	raw, _ := json.Marshal(witness)
	if err := os.Chmod(result.Witness.Path, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(result.Witness.Path, raw, 0400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(result.Witness.Path, 0400); err != nil {
		t.Fatal(err)
	}
	if result, err := rpc.capture(t.Context(), config); err == nil || result != nil || !strings.Contains(err.Error(), "scope") {
		t.Fatal("valid subset replaced complete selected witness", err)
	}
}

func TestReceiptHistoricalNativeCaptureReadProfileExcludesOtherMethods(t *testing.T) {
	for _, method := range []string{"author_submitExtrinsic", "eth_sendRawTransaction", "state_call", "state_getKeysPaged", "chain_getHeader", "eth_getBalance"} {
		client := newReceiptCollectorRpc("http://archive.example")
		client.storage = true
		client.client.Transport = collectionTestRoundTripper(func(*http.Request) (*http.Response, error) {
			t.Fatal("unadmitted method reached transport")
			return nil, nil
		})
		if _, err := client.call(t.Context(), method, nil); err == nil || client.requests != 0 {
			t.Fatal("method admitted", method)
		}
		client.client.CloseIdleConnections()
	}
}

func TestReceiptHistoricalNativeCaptureConfigPinsAndStrictPrivateInputs(t *testing.T) {
	_, config, rpc := historicalCaptureInputs(t, NativeStorageRootParentExecution)
	raw, _ := json.Marshal(config)
	path := filepath.Join(rpc.directory, "config.json")
	for _, fault := range []string{"valid", "pin", "public", "unknown", "duplicate"} {
		encoded, mode := slices.Clone(raw), os.FileMode(0600)
		switch fault {
		case "public":
			mode = 0644
		case "unknown":
			encoded = append([]byte(`{"native_state_root":"0x1234",`), raw[1:]...)
		case "duplicate":
			encoded = append([]byte(`{"schema":"substitution",`), raw[1:]...)
		}
		if err := os.WriteFile(path, encoded, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		reference := FileReference{Path: path, Sha256: digest(encoded)}
		if fault == "pin" {
			reference.Sha256 = "sha256:" + strings.Repeat("1", 64)
		}
		loaded, err := LoadReceiptHistoricalNativeCaptureConfig(t.Context(), reference)
		if fault == "valid" {
			if err != nil || objectDigest(loaded) != objectDigest(config) {
				t.Fatal("private config failed roundtrip", err)
			}
		} else if err == nil || loaded != nil {
			t.Fatal("bad config admitted", fault)
		}
	}
}
