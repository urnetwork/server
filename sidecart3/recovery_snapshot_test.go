package sidecart3

// FI-12's T3 half (TEST-M3-01): the incident kinds FI-12 reads are the restore frame's own, and server's rows read
// back as identities, so a renamed network or a deactivated client fails FI-12 until the incident lists it.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/sidecar/sidecartest/fault"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/sidecarrestore"
)

// Where xops's reconcile writes the postgres step's list rows, from this package's directory.
const recoveryReconcilePath = "../../xops/main/ansible/host_files/by-us-fmt-5-edge-6/db-archive/sidecar-restore-reconcile.sql"

// FI-12's kinds are the ones the frame and xops's reconcile write: the frame's by its own constants, the reconcile's
// by the rows its SQL inserts.
func TestRecoveryLineKindsAreTheFramesOwn(t *testing.T) {
	for _, pair := range [][2]string{
		{fault.RecoveryLineOrphanLog, sidecarrestore.ListKindOrphanLog},
		{fault.RecoveryLineOrphanRepo, sidecarrestore.ListKindOrphanRepo},
		{fault.RecoveryLineCloseNotReseeded, sidecarrestore.ListKindCloseNotReseeded},
		{fault.RecoveryLineCloseUnderReseed, sidecarrestore.ListKindCloseUnderReseed},
	} {
		if pair[0] != pair[1] {
			t.Errorf("FI-12 reads %q where the frame writes %q", pair[0], pair[1])
		}
	}
	reconcile, err := os.ReadFile(recoveryReconcilePath)
	if err != nil {
		t.Skipf("not run: no xops checkout beside the server (%v)", err)
	}
	for _, kind := range []string{fault.RecoveryLineNetworkRenamed, fault.RecoveryLineLoginLeftOut, fault.RecoveryLineClientDeactivated} {
		if !strings.Contains(string(reconcile), "'"+kind+"'") {
			t.Errorf("xops's reconcile writes no %q row", kind)
		}
	}
}

// A network as api creates one, its admin with a password login, and one client of the admin's.
func recoverySnapshotNetwork(t testing.TB, ctx context.Context) (networkId server.Id, userId server.Id, login string, clientId server.Id) {
	t.Helper()
	networkId, userId = server.NewId(), server.NewId()
	name := fmt.Sprintf("fi12-%s", strings.ToLower(networkId.String()[:8]))
	login = model.Testing_CreateNetwork(ctx, networkId, name, userId)
	clientSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: networkId, UserId: userId, NetworkName: name})
	result, err := model.AuthNetworkClient(&model.AuthNetworkClientArgs{Description: "synthetic", DeviceSpec: "synthetic"}, clientSession)
	if err != nil || result.Error != nil || result.ByClientJwt == nil {
		t.Fatalf("client: %v %+v", err, result)
	}
	byJwt, err := session.ParseByJwt(ctx, *result.ByClientJwt)
	if err != nil {
		t.Fatal(err)
	}
	return networkId, userId, login, *byJwt.ClientId
}

// Server's rows read back as FI-12's identities, through the oracle's read-only reader; a network renamed and a client
// deactivated behind the snapshot's back fail FI-12, and pass once the incident's list, read through the frame, names
// each.
func TestRecoveryIdentitiesReadServersRowsOnT3(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true}).Run(t, func(tb testing.TB) {
		ctx := tb.Context()
		networkId, userId, login, clientId := recoverySnapshotNetwork(tb, ctx)
		before := fault.NewRecoverySnapshot()
		if err := AddRecoveryIdentities(ctx, PostgresReader(), before, networkId); err != nil {
			tb.Fatal(err)
		}
		network := before.Identities[fault.RecoveryIdentityKey(fault.RecoveryKindNetwork, networkId.String())]
		member := before.Identities[fault.RecoveryIdentityKey(fault.RecoveryKindMember, userId.String())]
		client := before.Identities[fault.RecoveryIdentityKey(fault.RecoveryKindClient, clientId.String())]
		if network == nil || !strings.HasPrefix(network.Fields[fault.RecoveryFieldName], "fi12-") || !network.Active {
			tb.Fatalf("the network reads %+v", network)
		}
		if member == nil || member.Fields[fault.RecoveryFieldLogin] != login || member.Fields[fault.RecoveryFieldNetwork] != networkId.String() {
			tb.Fatalf("the member reads %+v", member)
		}
		if client == nil || !client.Active || client.Fields[fault.RecoveryFieldNetwork] != networkId.String() {
			tb.Fatalf("the client reads %+v", client)
		}
		again := fault.NewRecoverySnapshot()
		if err := AddRecoveryIdentities(ctx, PostgresReader(), again, networkId); err != nil {
			tb.Fatal(err)
		}
		if err := fault.CheckRecovery(before, again, nil); err != nil {
			tb.Fatalf("the rows read twice: %v", err)
		}

		// behind the snapshot's back: a suffixed name and a deactivated client, as a restore that listed nothing
		suffixed := network.Fields[fault.RecoveryFieldName] + "-3f9a"
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network SET network_name = $2 WHERE network_id = $1`, networkId, suffixed))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET active = false WHERE client_id = $1`, clientId))
		})
		after := fault.NewRecoverySnapshot()
		if err := AddRecoveryIdentities(ctx, PostgresReader(), after, networkId); err != nil {
			tb.Fatal(err)
		}
		err := fault.CheckRecovery(before, after, nil)
		if err == nil || !strings.Contains(err.Error(), "came back with name") || !strings.Contains(err.Error(), "came back inactive") {
			tb.Fatalf("a rename and a deactivation no line lists: %v", err)
		}

		// the same, listed in the incident the frame reads
		incidentId := server.NewId()
		listPath := filepath.Join(tb.TempDir(), "incident-"+incidentId.String()+".tsv")
		snapshotTime := time.Now().UTC().Format(time.RFC3339Nano)
		head := fmt.Sprintf("snapshot-time\tsidecar dump\t%s\t%s\nsnapshot-time\tmain dump\t%s\t%s\n", incidentId, snapshotTime, incidentId, snapshotTime)
		if err := os.WriteFile(listPath, []byte(head), 0o600); err != nil {
			tb.Fatal(err)
		}
		incident, err := sidecarrestore.OpenIncident(incidentId, listPath, nil)
		if err != nil {
			tb.Fatal(err)
		}
		for _, row := range []*sidecarrestore.ListRow{
			{Kind: fault.RecoveryLineNetworkRenamed, Table: "network", RowId: networkId.String(), Detail: suffixed},
			{Kind: fault.RecoveryLineClientDeactivated, Table: "network_client", RowId: clientId.String(), Detail: "created after the sidecar dump; re-enrolls"},
		} {
			if err := incident.Append(row); err != nil {
				tb.Fatal(err)
			}
		}
		recovery := RecoveryIncidentOf(incident, nil)
		if recovery.Id != incidentId || len(recovery.Lines) != 4 {
			tb.Fatalf("the incident reads %+v", recovery)
		}
		if err := fault.CheckRecovery(before, after, recovery); err != nil {
			tb.Fatalf("the rename and the deactivation, listed: %v", err)
		}
	})
}
