package perfvar

import (
	"context"
	"encoding/json"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
)

// This restores only the three byte-credit knobs overridden by the bulk
// fixture. It preserves its owner budgets, contract prefetch, retry spacing,
// write/read deadlines, selectors, H1 policy, and all packet-custody callbacks.
func privateSdkApplyProductionContractCredits(settings *clientconnect.ClientSettings) {
	defaults := clientconnect.DefaultContractManagerSettings()
	settings.ContractManagerSettings.InitialContractTransferByteCount = defaults.InitialContractTransferByteCount
	settings.ContractManagerSettings.InitialNetworkPeerContractTransferByteCount = defaults.InitialNetworkPeerContractTransferByteCount
	settings.ContractManagerSettings.StandardContractTransferByteCount = defaults.StandardContractTransferByteCount
}

type privateSdkDefaultContractRow struct {
	Stage                string `json:"stage"`
	Provider             int    `json:"provider"`
	Peer                 int    `json:"peer"`
	StatsSequence        uint64 `json:"stats_sequence,omitempty"`
	SignaturePresent     bool   `json:"signature_present"`
	Page                 int    `json:"page"`
	AtNs                 int64  `json:"at_ns"`
	Sequence             int    `json:"sequence,omitempty"`
	Contract             int    `json:"contract,omitempty"`
	Request              int    `json:"request,omitempty"`
	Lane                 uint32 `json:"lane,omitempty"`
	Companion            bool   `json:"companion"`
	ForceStream          bool   `json:"force_stream"`
	MetadataGeneration   uint64 `json:"metadata_generation,omitempty"`
	ContractRequestIndex uint64 `json:"contract_request_index,omitempty"`
	Role                 uint8  `json:"role,omitempty"`
	Requested            int64  `json:"requested_bytes,omitempty"`
	Granted              int64  `json:"granted_bytes,omitempty"`
	Effective            int64  `json:"effective_bytes,omitempty"`
	Acked                int64  `json:"acked_bytes,omitempty"`
	Used                 int64  `json:"used_bytes,omitempty"`
	Pending              int64  `json:"pending_bytes,omitempty"`
	Message              int64  `json:"message_bytes,omitempty"`
	CanUpdate            bool   `json:"can_update"`
	Expired              bool   `json:"expired"`
	MinUpdate            int64  `json:"min_update_bytes,omitempty"`
	NoAckPublished       bool   `json:"no_ack_budget_published"`
	NoAckRemaining       int64  `json:"no_ack_remaining_bytes,omitempty"`
	CreditInsufficient   bool   `json:"credit_insufficient"`
	BudgetUsed           int64  `json:"resend_budget_used,omitempty"`
	BudgetCapacity       int64  `json:"resend_budget_capacity,omitempty"`
	CallbackError        bool   `json:"callback_error"`
}

type privateSdkDefaultContractMode struct {
	page      atomic.Int64
	stateLock sync.Mutex
	rows      []privateSdkDefaultContractRow
	contracts map[clientconnect.Id]int
	peers     map[clientconnect.Id]int
	usageRows map[clientconnect.Id]int
	requests  int
	removals  []func()
	dropped   int
	overflows int
}

func newPrivateSdkDefaultContractMode() *privateSdkDefaultContractMode {
	mode := &privateSdkDefaultContractMode{contracts: map[clientconnect.Id]int{}, peers: map[clientconnect.Id]int{}, usageRows: map[clientconnect.Id]int{}}
	mode.page.Store(-1)
	return mode
}

// Only bounded local synthetic identities are retained; reports use ordinals.
func (self *privateSdkDefaultContractMode) peerOrdinal(peer clientconnect.Id) int {
	if peer == (clientconnect.Id{}) {
		return 0
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if ordinal := self.peers[peer]; ordinal != 0 {
		return ordinal
	}
	if len(self.peers) >= 128 {
		self.dropped++
		return 0
	}
	ordinal := len(self.peers) + 1
	self.peers[peer] = ordinal
	return ordinal
}

// Public usage rows are latest cumulative snapshots, not a per-epoch history.
// StatsSequence is the public per-contract emit order; old deliveries cannot regress it.
func (self *privateSdkDefaultContractMode) record(row privateSdkDefaultContractRow, contract clientconnect.Id) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if row.Stage == "sequence_overflow" || row.Stage == "contract_overflow" {
		self.overflows++
	}
	if contract != (clientconnect.Id{}) {
		if self.contracts[contract] == 0 {
			if len(self.contracts) >= 128 {
				self.dropped++
				return
			}
			self.contracts[contract] = len(self.contracts) + 1
		}
		row.Contract = self.contracts[contract]
	}
	row.Page = int(self.page.Load())
	if row.AtNs == 0 {
		row.AtNs = time.Now().UnixNano()
	}
	usage := row.Stage == "public_contract_usage" || row.Stage == "final_contract_usage"
	if usage {
		if index, ok := self.usageRows[contract]; ok {
			prior := self.rows[index]
			if row.StatsSequence < prior.StatsSequence {
				return
			}
			if row.StatsSequence == prior.StatsSequence {
				if row.Used != prior.Used || row.Granted != prior.Granted || row.Peer != prior.Peer || row.Provider != prior.Provider || row.Companion != prior.Companion || row.ForceStream != prior.ForceStream {
					self.overflows++
				}
				return
			}
			self.rows[index] = row
			return
		}
	}
	if len(self.rows) >= 512 {
		self.dropped++
		return
	}
	if usage {
		self.usageRows[contract] = len(self.rows)
	}
	self.rows = append(self.rows, row)
}

// This is the existing public listener installed before platform/application publication.
func (self *privateSdkDefaultContractMode) observeProvider(index int, client *clientconnect.Client) {
	unsubscribe := client.ContractManager().AddContractStatsCallback(func(events []*clientconnect.ContractStatsEvent) {
		for _, event := range events {
			if event.Receive {
				continue
			}
			stage := "public_contract_usage"
			if !event.Open {
				stage = "final_contract_usage"
			}
			self.record(privateSdkDefaultContractRow{Stage: stage, Provider: index, Peer: self.peerOrdinal(event.Path.DestinationId),
				Companion: event.Companion, ForceStream: event.Stream, Granted: int64(event.TransferByteCount), Used: int64(event.UsedByteCount), StatsSequence: event.Sequence}, event.ContractId)
		}
	})
	self.record(privateSdkDefaultContractRow{Stage: "observer_attached", Provider: index}, clientconnect.Id{})
	self.stateLock.Lock()
	self.removals = append(self.removals, unsubscribe)
	self.stateLock.Unlock()
}

func (self *privateSdkDefaultContractMode) close() {
	self.stateLock.Lock()
	removals := self.removals
	self.removals = nil
	self.stateLock.Unlock()
	for _, remove := range removals {
		remove()
	}
}

// Embedding preserves the real API control's Close/CloseAndWait/SetByJwt
// interfaces. Both send methods delegate with the exact original context.
type privateSdkDefaultContractControl struct {
	*clientconnect.ApiOutOfBandControl
	mode     *privateSdkDefaultContractMode
	provider int
}

func (self *privateSdkDefaultContractMode) wrapControl(index int, api *clientconnect.ApiOutOfBandControl) clientconnect.OutOfBandControl {
	return &privateSdkDefaultContractControl{ApiOutOfBandControl: api, mode: self, provider: index}
}

func (self *privateSdkDefaultContractControl) responseObserver(frames []*protocol.Frame, callback clientconnect.OobResultFunction) clientconnect.OobResultFunction {
	type requested struct {
		ordinal           int
		bytes             uint64
		companion, stream bool
	}
	var requests []requested
	for _, frame := range frames {
		message, err := clientconnect.FromFrame(frame)
		if err != nil {
			continue
		}
		request, ok := message.(*protocol.CreateContract)
		if !ok {
			continue
		}
		self.mode.stateLock.Lock()
		self.mode.requests++
		ordinal := self.mode.requests
		self.mode.stateLock.Unlock()
		requests = append(requests, requested{ordinal: ordinal, bytes: request.TransferByteCount, companion: request.Companion, stream: request.GetForceStream()})
		self.mode.record(privateSdkDefaultContractRow{Stage: "oob_request", Provider: self.provider, Request: ordinal,
			Requested: int64(request.TransferByteCount), Companion: request.Companion, ForceStream: request.GetForceStream()}, clientconnect.Id{})
	}
	return func(result []*protocol.Frame, err error) {
		for _, request := range requests {
			self.mode.record(privateSdkDefaultContractRow{Stage: "oob_callback", Provider: self.provider, Request: request.ordinal,
				Requested: int64(request.bytes), Companion: request.companion, ForceStream: request.stream, CallbackError: err != nil}, clientconnect.Id{})
		}
		for _, frame := range result {
			message, decodeErr := clientconnect.FromFrame(frame)
			if decodeErr != nil {
				continue
			}
			grant, ok := message.(*protocol.CreateContractResult)
			if !ok || grant.Contract == nil {
				continue
			}
			var stored protocol.StoredContract
			if clientconnect.ProtoUnmarshal(grant.Contract.StoredContractBytes, &stored) != nil {
				continue
			}
			id, parseErr := clientconnect.IdFromBytes(stored.ContractId)
			if parseErr != nil {
				continue
			}
			// A successful processed response can contain companion/stream
			// caps or a reused origin amount; never substitute requested bytes.
			peer, peerErr := clientconnect.IdFromBytes(stored.DestinationId)
			if peerErr != nil {
				continue
			}
			row := privateSdkDefaultContractRow{Stage: "signed_grant", Provider: self.provider, Peer: self.mode.peerOrdinal(peer), Granted: int64(stored.TransferByteCount), ForceStream: len(stored.StreamId) != 0, SignaturePresent: len(grant.Contract.StoredContractHmac) != 0, CallbackError: err != nil}
			if len(requests) == 1 {
				row.Request, row.Requested, row.Companion = requests[0].ordinal, int64(requests[0].bytes), requests[0].companion
			}
			self.mode.record(row, id)
		}
		if callback != nil {
			callback(result, err)
		}
	}
}

func (self *privateSdkDefaultContractControl) SendControl(frames []*protocol.Frame, callback clientconnect.OobResultFunction) {
	self.ApiOutOfBandControl.SendControl(frames, self.responseObserver(frames, callback))
}

func (self *privateSdkDefaultContractControl) SendControlWithCtx(ctx context.Context, frames []*protocol.Frame, callback clientconnect.OobResultFunction) {
	self.ApiOutOfBandControl.SendControlWithCtx(ctx, frames, self.responseObserver(frames, callback))
}

func (self *privateSdkDefaultContractMode) finish(t testing.TB, fixture *privateSdkBrowsingFixture, observation *nativeBrowsingObservation) {
	t.Helper()
	self.close()
	self.stateLock.Lock()
	rows, dropped, overflows := append([]privateSdkDefaultContractRow(nil), self.rows...), self.dropped, self.overflows
	self.stateLock.Unlock()
	witness := privateSdkDefaultContractWitness(rows, dropped, overflows)
	exercised := witness.Owners
	report := struct {
		Version           string                         `json:"version"`
		Status            string                         `json:"status"`
		PublicOpeningKeys int                            `json:"public_opening_keys"`
		Dropped           int                            `json:"dropped"`
		CoreOverflows     int                            `json:"core_overflows"`
		Pages             int                            `json:"completed_pages"`
		Rows              []privateSdkDefaultContractRow `json:"rows"`
	}{Version: "canonical-public-production-contract-opening-v1", Status: witness.Status, PublicOpeningKeys: exercised, Dropped: dropped, CoreOverflows: witness.Overflows, Pages: observation.CompletedNavigations, Rows: rows}
	encoded, err := json.Marshal(report)
	if err != nil || len(encoded) > 256*1024 {
		t.Error("default-contract bounded report failed")
		return
	}
	t.Logf("SDK_DEFAULT_CONTRACT_OPENING_V2 %s", encoded)
	if witness.Status != "EXERCISED_PUBLIC_PRODUCTION_OPENING_AND_USAGE" {
		t.Errorf("%s: rows must prove a public1MiB opening grant was used and a distinct signed grant was used on the same public key with aggregate usage beyond1MiB; exact sequence/debt/expiry causality is unavailable", witness.Status)
	}
}

func TestPrivateSdkProductionContractCreditChangeHasExactScope(t *testing.T) {
	settings := clientconnect.DefaultClientSettings()
	settings.ContractManagerSettings.InitialContractTransferByteCount = 256 * 1024 * 1024
	settings.ContractManagerSettings.InitialNetworkPeerContractTransferByteCount = 256 * 1024 * 1024
	settings.ContractManagerSettings.StandardContractTransferByteCount = 256 * 1024 * 1024
	expected := *settings.ContractManagerSettings
	defaults := clientconnect.DefaultContractManagerSettings()
	expected.InitialContractTransferByteCount, expected.InitialNetworkPeerContractTransferByteCount, expected.StandardContractTransferByteCount = defaults.InitialContractTransferByteCount, defaults.InitialNetworkPeerContractTransferByteCount, defaults.StandardContractTransferByteCount
	send, receive := settings.SendBufferSettings, settings.ReceiveBufferSettings
	privateSdkApplyProductionContractCredits(settings)
	if !reflect.DeepEqual(&expected, settings.ContractManagerSettings) || send != settings.SendBufferSettings || receive != settings.ReceiveBufferSettings {
		t.Fatal("restoring contract byte defaults changed adjacent policy")
	}
}
