//go:build linux || darwin || freebsd

// Actual SDK custody, original consent and independently approved work reach
// publication before exact SQL bytes and immutable retries are inspected.
package controller

import (
	"bytes"
	"context"
	"encoding/hex"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/urfoundation/sn/v2026/payoutartifact"
	"github.com/urfoundation/sn/v2026/protocol"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/startifact"
)

// No fresh reader is available on the retry. A retained payout must return
// before either the chain or current wallet/configuration is consulted.
type stClosedWorkClient struct {
	StClient
	bindingCalls int
}

func (self *stClosedWorkClient) BindingsAt(ctx context.Context, clients [][16]byte, epoch, start, end uint64) ([]*StFleetBindingState, error) {
	self.bindingCalls++
	result := make([]*StFleetBindingState, len(clients))
	for index := range result {
		result[index] = &StFleetBindingState{}
	}
	return result, ctx.Err()
}

// Read the actual settled row's jsonb rather than predicting PostgreSQL's
// presentation. The common driver has already verified whole-work publication;
// this continuation additionally checks every original wallet and exact retry.
func TestStClosedWorkPublishedArtifactKeepsExactOriginalRowsAndRetry(t *testing.T) {
	providerWorkActualSdkStreamPublication(t, false, func(t testing.TB, ctx context.Context, f *providerWorkWindowFixture, artifact *payoutartifact.Artifact) {
		if artifact == nil || artifact.ClosedWork == nil || artifact.ClosedWork.WholeInventory == nil || len(artifact.ClosedWork.Records) != 1 {
			t.Fatal("actual SDK publication omitted its original complete census")
		}
		var original []byte
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT provider_usage FROM transfer_contract WHERE contract_id=$1`, server.Id(artifact.ClosedWork.Records[0].ContractId)).Scan(&original))
		})
		if !bytes.Equal(original, artifact.ClosedWork.Records[0].Original) || artifact.ClosedWork.WindowStart != f.epoch.StartTime.UTC().Format(time.RFC3339Nano) || artifact.ClosedWork.WindowEnd != f.epoch.EndTime.UTC().Format(time.RFC3339Nano) || artifact.ClosedWork.Start != artifact.Start || artifact.ClosedWork.End != artifact.End || artifact.ClosedWork.PolicyHash != artifact.PolicyHash {
			t.Fatal("issuance reconstructed database bytes or guessed epoch/policy")
		}
		approved, err := stLoadProviderWorkAuthority(ctx, f.cfg, f.epoch)
		if err != nil || approved == nil {
			t.Fatal("published original authority is unavailable", err)
		}
		component, err := payoutartifact.VerifyWholeWorkInventory(ctx, artifact, approved.Expectation)
		if err != nil || component == nil || !component.Complete || !component.AttributionComplete || component.Reports == nil || component.Reports.ClosedWork.Contracts != 1 || component.Reports.ClosedWork.Providers != 3 || component.Reports.ClosedWork.UsageBytes != 121 {
			t.Fatal("independent reconstruction rejected actual published source", component, err)
		}
		if len(artifact.Providers) != len(f.authority.ExpectedProviders) {
			t.Fatal("publication changed the independently approved provider universe")
		}
		for index, member := range f.authority.ExpectedProviders {
			head, err := hex.DecodeString(member.WalletHeadHash)
			if err != nil || len(head) != 32 {
				t.Fatal("published provider has no independently approved wallet head", err)
			}
			originals, err := model.ReadWalletMappingHistory(ctx, f.authority.Domain, server.Id(member.ClientId), member.WalletGeneration, [32]byte(head))
			if err != nil {
				t.Fatal(err)
			}
			mapping, err := protocol.VerifyWalletMappingHistory(ctx, originals, protocol.WalletMappingHistoryExpectation{Domain: f.authority.Domain, ClientId: member.ClientId, HeadHash: [32]byte(head), Generation: member.WalletGeneration, Epoch: f.epoch.Epoch})
			if err != nil {
				t.Fatal(err)
			}
			if err := protocol.VerifyProspectiveWalletMapping(ctx, mapping, crypto.PubkeyToAddress(f.cfg.RootKey.PublicKey), f.epoch.Start.Block, f.epoch.StartTime.Unix()); err != nil {
				t.Fatal(err)
			}
			provider := artifact.Providers[index]
			if provider.ClientID != member.ClientId || provider.NetworkID != member.NetworkId || mapping.Statement.NetworkId != member.NetworkId || provider.Coldkey != mapping.Statement.Coldkey {
				t.Fatal("signed payout changed an original epoch wallet", index, provider)
			}
		}
		record := model.GetStPayoutArtifact(ctx, f.cfg.DeploymentKey(), f.epoch.Epoch, f.cfg.NoId)
		store, ok := server.LoadBlobStore()
		if record == nil || !ok {
			t.Fatal("original artifact publication is absent")
		}
		_, raw, err := startifact.Read(ctx, store, record.ContentHash)
		if err != nil {
			t.Fatal(err)
		}
		retryCfg := *f.cfg
		retryCfg.RootKey, retryCfg.ArtifactKey = nil, nil
		client := &stClosedWorkClient{}
		root, leaves, err := stComputeReleasePayout(ctx, &retryCfg, client, f.epoch.Epoch, time.Time{}, time.Time{}, 0, 0, nil)
		if err != nil || root != artifact.PayoutRoot || leaves != len(artifact.Leaves) || client.bindingCalls != 0 {
			t.Fatal("immutable retry asked for newer source authority", root, leaves, err)
		}
		_, again, err := startifact.Read(ctx, store, record.ContentHash)
		if err != nil || !bytes.Equal(raw, again) {
			t.Fatal("restart changed exact signed original census", err)
		}
	})
}
