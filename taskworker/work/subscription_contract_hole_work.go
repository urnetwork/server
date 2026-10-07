package work

// Retained task names drain queued refresh work after periodic renewal retires.
// New contract creation owns the live key TTL; these handlers touch no hole state.

import (
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// Keep the old JSON shape so pending work and already-recorded post retries decode.
type RefreshContractHolesArgs struct {
	Cursor           *model.ContractHoleCursor  `json:"cursor,omitempty"`
	PassStarted      time.Time                  `json:"pass_started"`
	FailedPairs      int                        `json:"failed_pairs,omitempty"`
	PairVisits       int                        `json:"pair_visits,omitempty"`
	SuccessfulPairs  int                        `json:"successful_pairs,omitempty"`
	PositivePairs    int                        `json:"positive_pairs,omitempty"`
	Pages            int                        `json:"pages,omitempty"`
	EarliestPositive *model.ContractHoleWitness `json:"earliest_positive,omitempty"`
}

// Retain old post-result decoding; the retirement hook ignores these values.
type RefreshContractHolesResult struct {
	Cursor           *model.ContractHoleCursor  `json:"cursor,omitempty"`
	Pairs            int                        `json:"pairs"`
	FailedPairs      int                        `json:"failed_pairs"`
	PairVisits       int                        `json:"pair_visits"`
	SuccessfulPairs  int                        `json:"successful_pairs"`
	PositivePairs    int                        `json:"positive_pairs"`
	Pages            int                        `json:"pages"`
	EarliestPositive *model.ContractHoleWitness `json:"earliest_positive,omitempty"`
	WarmReady        bool                       `json:"warm_ready"`
}

// An old queued task completes successfully without querying PostgreSQL or Redis.
func RefreshContractHoles(args *RefreshContractHolesArgs, clientSession *session.ClientSession) (*RefreshContractHolesResult, error) {
	return &RefreshContractHolesResult{}, nil
}

// Already-committed post retries also drain without scheduling another task.
func RefreshContractHolesPost(args *RefreshContractHolesArgs, result *RefreshContractHolesResult, clientSession *session.ClientSession, tx server.PgTx) error {
	return nil
}
