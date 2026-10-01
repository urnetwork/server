// The operator must stop before chain reads or artifact construction when
// retained terminal work has no provable epoch ownership.
package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

type missingUsageTimeClient struct {
	StClient
	bindingCalls int
}

func (self *missingUsageTimeClient) BindingsAt(context.Context, [][16]byte, uint64, uint64, uint64) ([]*StFleetBindingState, error) {
	self.bindingCalls++
	return nil, errors.New("unexpected chain read after incomplete custody")
}

func TestStReleasePayoutMissingCloseTimeStopsBeforeChain(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.ApplyDbMigrations = false
	testEnv.Run(t, func(t testing.TB) {
		ctx := t.Context()
		server.ApplyDbMigrationsUpTo(ctx, 744)
		id := server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
				(contract_id,source_id,source_network_id,destination_id,destination_network_id,transfer_byte_count,outcome)
				VALUES($1,$2,$3,$4,$5,1000,'settled')`, id, server.NewId(), server.NewId(), server.NewId(), server.NewId()))
		})
		server.ApplyDbMigrations(ctx)
		cfg := &StConfig{ChainId: 964, ContractAddress: common.Address{19: 1}, NoId: 7}
		start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
		client := &missingUsageTimeClient{}
		for _, archive := range []bool{false, true} {
			if archive {
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_contract WHERE contract_id=$1`, id))
				})
			}
			root, leaves, err := stComputeReleasePayout(ctx, cfg, client, 906, start, start.Add(time.Hour), 101, 401, nil)
			if err == nil || !strings.Contains(err.Error(), id.String()) || root != ([32]byte{}) || leaves != 0 || client.bindingCalls != 0 {
				t.Fatalf("archive=%t: incomplete custody reached payout: root=%x leaves=%d calls=%d error=%v", archive, root, leaves, client.bindingCalls, err)
			}
			if artifact := model.GetStPayoutArtifact(ctx, cfg.DeploymentKey(), 906, cfg.NoId); artifact != nil {
				t.Fatal("incomplete custody persisted a payout artifact")
			}
		}
	})
}
