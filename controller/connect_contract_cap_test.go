// pro.yml can cap the contract size granted against a payer's balance by
// plan tier. The cap is off until a tier sets it.
package controller

import (
	"context"
	"testing"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Only an escrowed contract has a payer, and a companion is paid by its
// destination, the same choice newContract makes for its funding path.
func TestContractPayerNetworkId(t *testing.T) {
	sourceNetworkId := server.NewId()
	destinationNetworkId := server.NewId()
	for _, test := range []struct {
		name        string
		provideMode model.ProvideMode
		companion   bool
		wantPayer   server.Id
		wantEscrow  bool
	}{
		{name: "public", provideMode: model.ProvideModePublic, wantPayer: sourceNetworkId, wantEscrow: true},
		{name: "public companion", provideMode: model.ProvideModePublic, companion: true, wantPayer: destinationNetworkId, wantEscrow: true},
		{name: "stream companion", provideMode: model.ProvideModeStream, companion: true, wantPayer: destinationNetworkId, wantEscrow: true},
		{name: "network", provideMode: model.ProvideModeNetwork},
		{name: "network companion", provideMode: model.ProvideModeNetwork, companion: true},
		{name: "friends and family", provideMode: model.ProvideModeFriendsAndFamily},
	} {
		t.Run(test.name, func(t *testing.T) {
			payer, escrowed := contractPayerNetworkId(test.provideMode, test.companion, sourceNetworkId, destinationNetworkId)
			if payer != test.wantPayer || escrowed != test.wantEscrow {
				t.Fatalf("payer = %s escrowed = %t, want %s %t", payer, escrowed, test.wantPayer, test.wantEscrow)
			}
		})
	}
}

// proLookup replaces the payer plan lookup and records each lookup.
type proLookup struct {
	pro     bool
	lookups []server.Id
}

func (self *proLookup) isProNetwork(ctx context.Context, networkId server.Id) bool {
	self.lookups = append(self.lookups, networkId)
	return self.pro
}

func withProLookup(lookup *proLookup) func() {
	previous := isProNetwork
	isProNetwork = lookup.isProNetwork
	return func() { isProNetwork = previous }
}

func TestPayerMaxContractTransferByteCount(t *testing.T) {
	for _, test := range []struct {
		name       string
		freeMax    model.ByteCount
		proMax     model.ByteCount
		pro        bool
		want       model.ByteCount
		wantLookup bool
	}{
		// unset or zero is no cap, and the plan is not looked up at all
		{name: "no cap", want: MaxContractTransferByteCount},
		{name: "no cap pro", pro: true, want: MaxContractTransferByteCount},
		{name: "negative cap", freeMax: -1, want: MaxContractTransferByteCount},
		{name: "free payer capped", freeMax: 64 * model.Mib, want: 64 * model.Mib, wantLookup: true},
		{name: "pro payer not capped", freeMax: 64 * model.Mib, pro: true, want: MaxContractTransferByteCount, wantLookup: true},
		// a cap below the smallest contract still grants one that fits a message
		{name: "cap below minimum", freeMax: 1024, want: MinContractTransferByteCount, wantLookup: true},
		{name: "cap above maximum", freeMax: 4 * model.Gib, want: MaxContractTransferByteCount, wantLookup: true},
		{name: "pro cap pro payer", proMax: 32 * model.Mib, pro: true, want: 32 * model.Mib, wantLookup: true},
		{name: "pro cap free payer", proMax: 32 * model.Mib, want: MaxContractTransferByteCount, wantLookup: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			defer model.Testing_SetMaxContractTransferByteCount(test.freeMax, test.proMax)()
			lookup := &proLookup{pro: test.pro}
			defer withProLookup(lookup)()
			payerNetworkId := server.NewId()

			if got := payerMaxContractTransferByteCount(context.Background(), payerNetworkId); got != test.want {
				t.Fatalf("max contract = %d, want %d", got, test.want)
			}
			if !test.wantLookup && len(lookup.lookups) != 0 {
				t.Fatalf("looked up the payer's plan %d times with no cap configured", len(lookup.lookups))
			}
			if test.wantLookup && (len(lookup.lookups) != 1 || lookup.lookups[0] != payerNetworkId) {
				t.Fatalf("plan lookups = %v, want only the payer %s", lookup.lookups, payerNetworkId)
			}
		})
	}
}

// Deterministic, database-free: newContract requests the escrow at the payer's
// cap and signs the granted size. A free payer's 128 MiB request is escrowed at
// the 64 MiB free cap; a Pro payer keeps the full request, and so does every
// payer while the cap is unset.
func TestNewContractCapsFreePayerEscrow(t *testing.T) {
	requested := 128 * model.Mib
	for _, test := range []struct {
		name    string
		freeMax model.ByteCount
		pro     bool
		want    model.ByteCount
	}{
		{name: "cap unset", want: requested},
		{name: "free payer", freeMax: 64 * model.Mib, want: 64 * model.Mib},
		{name: "pro payer", freeMax: 64 * model.Mib, pro: true, want: requested},
		{name: "request under the cap", freeMax: 256 * model.Mib, want: requested},
	} {
		t.Run(test.name, func(t *testing.T) {
			defer model.Testing_SetMaxContractTransferByteCount(test.freeMax, 0)()
			lookup := &proLookup{pro: test.pro}
			defer withProLookup(lookup)()

			sourceNetworkId := server.NewId()
			sourceId := server.NewId()
			destinationNetworkId := server.NewId()
			destinationId := server.NewId()
			grantedContractId := server.NewId()

			previousFind, previousCreate := findActiveClientPairNetworks, createTransferEscrow
			defer func() { findActiveClientPairNetworks, createTransferEscrow = previousFind, previousCreate }()
			findActiveClientPairNetworks = func(ctx context.Context, a server.Id, b server.Id) (*server.Id, *server.Id) {
				return &sourceNetworkId, &destinationNetworkId
			}
			var escrowRequest model.ByteCount
			createTransferEscrow = func(
				ctx context.Context,
				escrowSourceNetworkId server.Id,
				escrowSourceId server.Id,
				escrowDestinationNetworkId server.Id,
				escrowDestinationId server.Id,
				contractTransferByteCount model.ByteCount,
			) (*model.TransferEscrow, error) {
				escrowRequest = contractTransferByteCount
				return &model.TransferEscrow{
					ContractId:        grantedContractId,
					TransferByteCount: contractTransferByteCount,
					Priority:          model.UnpaidPriority,
				}, nil
			}

			contractId, count, _, _, err := newContract(
				context.Background(),
				sourceId,
				destinationId,
				nil,
				false,
				true,
				requested,
				model.ProvideModePublic,
				false,
				0,
				connect.DefaultContractManagerSettings(),
			)
			if err != nil {
				t.Fatal(err)
			}
			if escrowRequest != test.want {
				t.Fatalf("escrow requested %d, want %d", escrowRequest, test.want)
			}
			if contractId != grantedContractId || count != test.want {
				t.Fatalf("contract %s signs %d bytes, want %s %d", contractId, count, grantedContractId, test.want)
			}
			if 0 < test.freeMax && (len(lookup.lookups) != 1 || lookup.lookups[0] != sourceNetworkId) {
				t.Fatalf("plan lookups = %v, want only the paying source network %s", lookup.lookups, sourceNetworkId)
			}
			if test.freeMax == 0 && len(lookup.lookups) != 0 {
				t.Fatalf("looked up the payer's plan %d times with the cap unset", len(lookup.lookups))
			}
		})
	}
}
