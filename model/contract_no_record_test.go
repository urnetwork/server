package model

import (
	"context"
	"testing"

	"github.com/urnetwork/server"
)

// Each contract reads as what the answering client reported of it, and the
// read changes nothing.
func TestGetContractNoRecordStates(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := context.Background()
		network := server.NewId()
		origin, provider := contractPayoutTestId(1), contractPayoutTestId(2)
		addContractPayoutTestClients(ctx, map[server.Id]server.Id{origin: network, provider: network})
		newContract := func() server.Id {
			contractId, err := CreateContractNoEscrow(ctx, network, origin, network, provider, 100)
			if err != nil {
				t.Fatal(err)
			}
			return contractId
		}
		report := func(contractId server.Id, clientId server.Id, checkpoint bool) {
			if err := CloseContract(ctx, contractId, clientId, 50, checkpoint); err != nil {
				t.Fatal(err)
			}
		}
		silent := newContract()
		checkpointed := newContract()
		report(checkpointed, provider, true)
		final := newContract()
		report(final, provider, false)
		settled := newContract()
		report(settled, origin, false)
		report(settled, provider, false)
		missing := server.NewId()

		states := GetContractNoRecordStates(ctx, provider, []server.Id{silent, checkpointed, final, settled, missing})
		want := map[server.Id]ContractNoRecordState{
			silent:       {Party: ContractPartyDestination, Report: ContractNoRecordReportNone},
			checkpointed: {Party: ContractPartyDestination, Report: ContractNoRecordReportCheckpoint},
			final:        {Party: ContractPartyDestination, Report: ContractNoRecordReportFinal},
			settled:      {Party: ContractPartyDestination, Report: ContractNoRecordReportClosed},
			missing:      {Report: ContractNoRecordReportUnknown},
		}
		if len(states) != len(want) {
			t.Fatalf("states = %+v, want %+v", states, want)
		}
		for contractId, state := range want {
			if states[contractId] != state {
				t.Fatalf("state of %s = %+v, want %+v", contractId, states[contractId], state)
			}
		}
		if state := GetContractNoRecordStates(ctx, origin, []server.Id{silent})[silent]; state != (ContractNoRecordState{Party: ContractPartySource, Report: ContractNoRecordReportNone}) {
			t.Fatalf("sender state = %+v", state)
		}
		if state := GetContractNoRecordStates(ctx, server.NewId(), []server.Id{silent})[silent]; state != (ContractNoRecordState{Report: ContractNoRecordReportUnknown}) {
			t.Fatalf("outsider state = %+v", state)
		}
		var silentReports, checkpointReports int
		var checkpointStill bool
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT
				(SELECT count(*) FROM contract_close WHERE contract_id = $1),
				(SELECT count(*) FROM contract_close WHERE contract_id = $2),
				(SELECT checkpoint FROM contract_close WHERE contract_id = $2 AND party = 'destination')`,
				silent, checkpointed).Scan(&silentReports, &checkpointReports, &checkpointStill))
		})
		if silentReports != 0 || checkpointReports != 1 || !checkpointStill {
			t.Fatal("reading the states wrote a report")
		}
	})
}
