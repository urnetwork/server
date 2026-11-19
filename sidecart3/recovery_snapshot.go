package sidecart3

// T3's half of FI-12's snapshot (TEST-M3-01; sidecar's sidecartest/fault/fi12.go): what §19.10's postgres step brings
// back, read from server's Postgres through the oracle's read-only reader (store_view.go), and a recovery's incident
// list from the restore frame (server's sidecarrestore), each in FI-12's own terms, so a T3 row of FS-40's, FS-41's or
// FS-42's hands the oracle what came back from the real stores, before its fault and after its recovery, with
// fault.RecordRecoverySnapshot and fault.RecordRecovered. The heads and contents a row snapshots read through the
// oracle's store view on either tier (RecoverySnapshot.AddHeadsFrom), so only api's rows and the incident live here.
//
// The identities are a network's row (its name), its admin member's (the logins of network_user_auth_password and
// network_user_auth_sso, sorted and joined, which the restore's login-left-out names), and each of its clients'
// (active as network_client holds it): the rows the sidecar dump and the main dump each restore.

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/sidecartest/fault"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/sidecarrestore"
)

// One uuid column's value as an id's string, as server.Id prints it.
func recoveryRowId(value any) (string, error) {
	switch typed := value.(type) {
	case [16]byte:
		return server.Id(typed).String(), nil
	case []byte:
		if len(typed) == 16 {
			return server.Id([16]byte(typed)).String(), nil
		}
	case string:
		id, err := server.ParseId(typed)
		if err != nil {
			return "", err
		}
		return id.String(), nil
	}
	return "", fmt.Errorf("%T is no uuid", value)
}

// The ids' strings, as a query's uuid[] argument takes them.
func recoveryIdStrings(ids []server.Id) []string {
	idStrings := []string{}
	for _, id := range ids {
		idStrings = append(idStrings, id.String())
	}
	return idStrings
}

// Adds to snapshot each network's identity, its admin member's and its clients', as server's Postgres holds them now:
// a network or member whose row is gone is left out, which FI-12 reads as lost unless the incident lists it.
func AddRecoveryIdentities(ctx context.Context, reader sidecartest.PostgresReader, snapshot *fault.RecoverySnapshot, networkIds ...server.Id) error {
	if len(networkIds) == 0 {
		return nil
	}
	networkRows, err := reader.Rows(ctx, `SELECT network_id, network_name, admin_user_id FROM network WHERE network_id = ANY($1::uuid[])`, recoveryIdStrings(networkIds))
	if err != nil {
		return fmt.Errorf("the networks' rows: %w", err)
	}
	adminIds := []server.Id{}
	adminNetworks := map[string]string{}
	for _, row := range networkRows {
		networkId, err := recoveryRowId(row[0])
		if err != nil {
			return err
		}
		name, _ := row[1].(string)
		adminId, err := recoveryRowId(row[2])
		if err != nil {
			return err
		}
		snapshot.AddIdentity(&fault.RecoveryIdentity{Kind: fault.RecoveryKindNetwork, Id: networkId, Active: true, Fields: map[string]string{fault.RecoveryFieldName: name}})
		parsedAdminId, err := server.ParseId(adminId)
		if err != nil {
			return err
		}
		adminIds = append(adminIds, parsedAdminId)
		adminNetworks[adminId] = networkId
	}

	if 0 < len(adminIds) {
		userRows, err := reader.Rows(ctx, `SELECT user_id FROM network_user WHERE user_id = ANY($1::uuid[])`, recoveryIdStrings(adminIds))
		if err != nil {
			return fmt.Errorf("the members' rows: %w", err)
		}
		loginRows, err := reader.Rows(ctx, `
			SELECT user_id, user_auth FROM network_user_auth_password WHERE user_id = ANY($1::uuid[])
			UNION ALL
			SELECT user_id, user_auth FROM network_user_auth_sso WHERE user_id = ANY($1::uuid[])
		`, recoveryIdStrings(adminIds))
		if err != nil {
			return fmt.Errorf("the members' logins: %w", err)
		}
		userLogins := map[string][]string{}
		for _, row := range loginRows {
			userId, err := recoveryRowId(row[0])
			if err != nil {
				return err
			}
			if login, ok := row[1].(string); ok {
				userLogins[userId] = append(userLogins[userId], login)
			}
		}
		for _, row := range userRows {
			userId, err := recoveryRowId(row[0])
			if err != nil {
				return err
			}
			logins := userLogins[userId]
			slices.Sort(logins)
			snapshot.AddIdentity(&fault.RecoveryIdentity{Kind: fault.RecoveryKindMember, Id: userId, Active: true, Fields: map[string]string{
				fault.RecoveryFieldLogin:   strings.Join(logins, ","),
				fault.RecoveryFieldNetwork: adminNetworks[userId],
			}})
		}
	}

	clientRows, err := reader.Rows(ctx, `SELECT client_id, network_id, active FROM network_client WHERE network_id = ANY($1::uuid[])`, recoveryIdStrings(networkIds))
	if err != nil {
		return fmt.Errorf("the clients' rows: %w", err)
	}
	for _, row := range clientRows {
		clientId, err := recoveryRowId(row[0])
		if err != nil {
			return err
		}
		networkId, err := recoveryRowId(row[1])
		if err != nil {
			return err
		}
		active, _ := row[2].(bool)
		snapshot.AddIdentity(&fault.RecoveryIdentity{Kind: fault.RecoveryKindClient, Id: clientId, Active: active, Fields: map[string]string{fault.RecoveryFieldNetwork: networkId}})
	}
	return nil
}

// The incident's list as FI-12 reads it, every row in file order, and mainDump, the main dump's copy of each row the
// sidecar dump does not name, which the row took and hands over (nil for none).
func RecoveryIncidentOf(incident *sidecarrestore.Incident, mainDump map[string]*fault.RecoveryIdentity) *fault.RecoveryIncident {
	recovery := &fault.RecoveryIncident{Id: incident.IncidentId(), MainDump: mainDump}
	for _, row := range incident.Rows() {
		recovery.Lines = append(recovery.Lines, &fault.RecoveryIncidentLine{Kind: row.Kind, Table: row.Table, RowId: row.RowId, Detail: row.Detail})
	}
	return recovery
}
