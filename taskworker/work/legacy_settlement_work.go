// Legacy settlement intents have independent bounded recovery owners.
package work

import (
	"fmt"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

type FlushLegacySettlementsArgs struct {
	Shard  int                           `json:"shard"`
	Cursor *model.LegacySettlementCursor `json:"cursor,omitempty"`
}
type FlushLegacySettlementsResult struct {
	model.LegacySettlementFlushResult
}

func scheduleFlushLegacySettlements(clientSession *session.ClientSession, tx server.PgTx, shard int, after *model.LegacySettlementCursor, more bool) {
	next := server.NowUtc().Add(2 * time.Second)
	if more {
		next = server.NowUtc()
	}
	task.ScheduleTaskInTx(tx, FlushLegacySettlements, &FlushLegacySettlementsArgs{Shard: shard, Cursor: after}, clientSession,
		task.RunOnce(fmt.Sprintf("flush_legacy_settlements_%d", shard)), task.RunAt(next), task.MaxTime(30*time.Second))
}
func ScheduleFlushLegacySettlements(clientSession *session.ClientSession, tx server.PgTx) {
	for shard := range model.LegacySettlementShardCount {
		scheduleFlushLegacySettlements(clientSession, tx, shard, nil, false)
	}
}
func FlushLegacySettlements(args *FlushLegacySettlementsArgs, clientSession *session.ClientSession) (*FlushLegacySettlementsResult, error) {
	result, err := model.FlushLegacySettlements(clientSession.Ctx, args.Shard, args.Cursor, model.LegacySettlementPageLimit)
	return &FlushLegacySettlementsResult{LegacySettlementFlushResult: result}, err
}
func FlushLegacySettlementsPost(args *FlushLegacySettlementsArgs, result *FlushLegacySettlementsResult, clientSession *session.ClientSession, tx server.PgTx) error {
	scheduleFlushLegacySettlements(clientSession, tx, args.Shard, result.Cursor, result.More && result.Failed == 0 && result.Completed > 0)
	return nil
}
