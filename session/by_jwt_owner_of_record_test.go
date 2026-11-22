//go:build sidecar_pending

package session

// QD-1194's exception to D-TEAM-2's owner of record, on Postgres with MG6: a
// team's tagged client is the network's (owner decision 83), so its mint signs
// its credential, and its window clients', for the team's owner of record with
// no member_id, and the state check takes such a credential only as a
// sidecar-held client credential whose root claim names an active tagged
// client of that team. Every other credential naming the owner of record stays
// refused by its credential_change_time, which lies past every token, and a
// personal network's credentials rotate as before.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// model's teamOwnerOfRecordCredentialChangeTime: past every token's creation.
var testOwnerOfRecordChangeTime = time.Date(9999, time.January, 1, 0, 0, 0, 0, time.UTC)

// A team as team-create leaves it: its owner of record, whose change time
// refuses every token naming it, network_team, and a device.
type testOwnerOfRecordTeam struct {
	networkId       server.Id
	ownerOfRecordId server.Id
	deviceId        server.Id
}

func newTestOwnerOfRecordTeam(t testing.TB, ctx context.Context) *testOwnerOfRecordTeam {
	t.Helper()
	team := &testOwnerOfRecordTeam{networkId: server.NewId(), ownerOfRecordId: server.NewId(), deviceId: server.NewId()}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_user(user_id,user_name,auth_type,credential_change_time) VALUES($1,'owner-of-record','owner_of_record',$2)`, team.ownerOfRecordId, testOwnerOfRecordChangeTime))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network(network_id,network_name,admin_user_id) VALUES($1,$2,$3)`, team.networkId, "team-"+team.networkId.String(), team.ownerOfRecordId))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_team(network_id) VALUES($1)`, team.networkId))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO device(device_id,network_id,device_name,device_spec) VALUES($1,$2,'synthetic','synthetic')`, team.deviceId, team.networkId))
	})
	return team
}

// Adds an active client under parent (top level when nil) and its owner row,
// and returns the credential the mint signs for it: the owner of record's,
// sidecar-held and admin_refused, with no member_id, its root claim its actual
// root. A tagged top-level client is one a tagged key enrolled, sponsored by
// a member; an untagged child is a window client, owned by the owner of record
// as the mint writes it.
func (self *testOwnerOfRecordTeam) client(t testing.TB, ctx context.Context, parent *ByJwt, tagged bool) *ByJwt {
	t.Helper()
	clientId := server.NewId()
	issuer := NewByJwt(self.networkId, self.ownerOfRecordId, "team", false, false)
	issuer.SetStrictExp(true)
	var parentClientId *server.Id
	if parent != nil {
		issuer = parent
		parentClientId = parent.ClientId
	}
	var ownerUserId *server.Id
	var sponsorUserId *server.Id
	if tagged {
		sponsor := server.NewId()
		sponsorUserId = &sponsor
	} else {
		ownerUserId = &self.ownerOfRecordId
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client(client_id,network_id,device_id,description,active,source_client_id) VALUES($1,$2,$3,'synthetic',true,$4)`, clientId, self.networkId, self.deviceId, parentClientId))
		server.RaisePgResult(tx.Exec(
			ctx,
			`INSERT INTO network_client_owner(client_id,owner_user_id,member_id,sponsor_user_id,tagged,admin_refused,strict_exp) VALUES($1,$2,$2,$3,$4,true,true)`,
			clientId, ownerUserId, sponsorUserId, tagged,
		))
	})
	credential := issuer.Client(self.deviceId, clientId)
	credential.AdminRefused = true
	return credential
}

// Deactivates clients as production does, their records refused first.
func (self *testOwnerOfRecordTeam) deactivate(ctx context.Context, clients ...*ByJwt) {
	clientIds := []server.Id{}
	for _, client := range clients {
		clientIds = append(clientIds, *client.ClientId)
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `SELECT client_id FROM network_client WHERE client_id = ANY($1) FOR UPDATE`, clientIds))
		server.Raise(RevokeClientRecordsInTx(ctx, tx, self.networkId, clientIds))
		server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET active=false WHERE client_id = ANY($1)`, clientIds))
	})
}

// A copy of credential with its claims changed by edit.
func testOwnerOfRecordVariant(credential *ByJwt, edit func(variant *ByJwt)) *ByJwt {
	variant := credential.Renew()
	edit(variant)
	return variant
}

// The tagged client's credential and its window client's validate on the owner
// of record, alone, with their parent, in a transaction, on a session branch
// and as renewed, and the renewed token validates too: the paths a tagged
// agent's refresh, auth-client, bind and connect transports take.
func TestTeamTaggedLineageCredentialsPassOnTheOwnerOfRecord(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := WithStateQuerySource(t.Context(), StateQueryConnectH1)
		team := newTestOwnerOfRecordTeam(t, ctx)
		tagged := team.client(t, ctx, nil, true)
		window := team.client(t, ctx, tagged, false)
		if tagged.MemberId != nil || window.MemberId != nil || *window.RootClientId != *tagged.ClientId {
			t.Fatalf("the lineage's claims: member %v %v, window root %v", tagged.MemberId, window.MemberId, window.RootClientId)
		}

		validBefore := stateQueryCount(StateQueryConnectH1, true, stateQueryValid)
		if err := ValidateByJwtState(ctx, tagged, true); err != nil {
			t.Fatalf("the tagged client's credential: %v", err)
		}
		if err := ValidateByJwtState(ctx, window, true); err != nil {
			t.Fatalf("its window client's credential: %v", err)
		}
		if err := ValidateByJwtStateForParent(ctx, window, *tagged.ClientId); err != nil {
			t.Fatalf("its window client's credential with its parent: %v", err)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			if err := ValidateByJwtStateInTx(ctx, tx, tagged, true); err != nil {
				t.Fatalf("the tagged client's credential in a transaction: %v", err)
			}
		})
		if valid := stateQueryCount(StateQueryConnectH1, true, stateQueryValid) - validBefore; valid != 4 {
			t.Errorf("4 checks counted %.0f valid state queries", valid)
		}
		sessionId := server.NewId()
		onSession := testOwnerOfRecordVariant(window, func(variant *ByJwt) { variant.SessionId = &sessionId })
		if err := ValidateByJwtState(ctx, onSession, true); err != nil {
			t.Fatalf("its window client's credential on a session branch: %v", err)
		}

		// the refresh (GET /auth/refresh's renewal) checks the credential in its
		// own transaction and signs one that names the owner of record again
		clientSession := NewLocalClientSession(ctx, "192.0.2.9:1", tagged)
		defer clientSession.Cancel()
		signed, err := RenewSessionCredential(clientSession, "team", false)
		if err != nil {
			t.Fatalf("the tagged client's refresh: %v", err)
		}
		renewed, err := ParseByJwt(ctx, signed)
		if err != nil {
			t.Fatal(err)
		}
		if renewed.UserId != team.ownerOfRecordId || renewed.MemberId != nil || !renewed.StrictExp || renewed.ClientId == nil || *renewed.ClientId != *tagged.ClientId {
			t.Fatalf("the renewed credential: user %s member %v strict_exp %t client %v", renewed.UserId, renewed.MemberId, renewed.StrictExp, renewed.ClientId)
		}
		if err := ValidateByJwtState(ctx, renewed, true); err != nil {
			t.Fatalf("the renewed credential: %v", err)
		}
	})
}

// Every other credential naming the owner of record stays refused, as rotated,
// which is never remembered: the owner of record's own network credential, an
// untagged client's and its child's, the tagged client's without strict_exp,
// with an API key's claim, with a member claim, and a root claim naming a
// tagged client of another team.
func TestOwnerOfRecordCredentialsOutsideATaggedLineageAreRefused(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := WithStateQuerySource(t.Context(), StateQueryConnectH1)
		team := newTestOwnerOfRecordTeam(t, ctx)
		other := newTestOwnerOfRecordTeam(t, ctx)
		tagged := team.client(t, ctx, nil, true)
		// a member claim's refusal is remembered, under a key that names no
		// member, so it is tried on a tagged client of its own
		memberTagged := team.client(t, ctx, nil, true)
		untagged := team.client(t, ctx, nil, false)
		untaggedChild := team.client(t, ctx, untagged, false)
		otherTagged := other.client(t, ctx, nil, true)
		apiKeyId := server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO account_api_key (api_key_id, network_id, api_key, name) VALUES ($1, $2, $3, 'synthetic')`, apiKeyId, team.networkId, "synthetic-"+apiKeyId.String()))
		})
		memberId := server.NewId()

		for _, refused := range []struct {
			name          string
			byJwt         *ByJwt
			requireClient bool
			rotated       bool
		}{
			{"the owner of record's network credential", NewByJwt(team.networkId, team.ownerOfRecordId, "team", false, false), false, true},
			{"an untagged client's", untagged, true, true},
			{"an untagged client's child's", untaggedChild, true, true},
			{"the tagged client's without strict_exp", testOwnerOfRecordVariant(tagged, func(variant *ByJwt) { variant.StrictExp = false }), true, true},
			{"the tagged client's with an API key's claim", testOwnerOfRecordVariant(tagged, func(variant *ByJwt) { variant.ApiKeyId = &apiKeyId }), true, true},
			{"a tagged client's with a member claim", testOwnerOfRecordVariant(memberTagged, func(variant *ByJwt) { variant.MemberId = &memberId }), true, false},
			{"an untagged client's naming another team's tagged root", testOwnerOfRecordVariant(untagged, func(variant *ByJwt) { variant.RootClientId = otherTagged.ClientId }), true, false},
		} {
			rotatedBefore := stateQueryCount(StateQueryConnectH1, refused.byJwt.ClientId != nil, stateQueryCredentialRotated)
			for attempt := range 2 {
				if err := ValidateByJwtState(ctx, refused.byJwt, refused.requireClient); !errors.Is(err, ErrByJwtInactive) {
					t.Fatalf("%s, check %d: %v", refused.name, attempt, err)
				}
			}
			if refused.rotated {
				// refused by the owner of record's change time each time, never remembered
				if rotated := stateQueryCount(StateQueryConnectH1, refused.byJwt.ClientId != nil, stateQueryCredentialRotated) - rotatedBefore; rotated != 2 {
					t.Errorf("%s: 2 checks counted %.0f rotated state queries", refused.name, rotated)
				}
			}
		}
		// the tagged client's own credential, beside them
		if err := ValidateByJwtState(ctx, tagged, true); err != nil {
			t.Fatalf("the tagged client's credential: %v", err)
		}
	})
}

// The exception is the client's: the tagged client's removal ends its
// credential and its window client's, and that refusal is remembered as any
// client's is, one state query for every retry. A personal network's tagged
// sidecar, whose credential names its owner, still ends at its owner's
// credential rotation: the exception holds in a team alone.
func TestTeamTaggedLineageCredentialEndsWithItsClient(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := WithStateQuerySource(t.Context(), StateQueryConnectH1)
		team := newTestOwnerOfRecordTeam(t, ctx)
		tagged := team.client(t, ctx, nil, true)
		window := team.client(t, ctx, tagged, false)
		for _, valid := range []*ByJwt{tagged, window} {
			if err := ValidateByJwtState(ctx, valid, true); err != nil {
				t.Fatal(err)
			}
		}
		team.deactivate(ctx, tagged, window)
		for _, removed := range []*ByJwt{tagged, window} {
			before := stateQueryCount(StateQueryConnectH1, true, stateQueryNoActiveRow)
			for attempt := range 3 {
				if err := ValidateByJwtState(ctx, removed, true); !errors.Is(err, ErrByJwtInactive) {
					t.Fatalf("a removed client's credential, check %d: %v", attempt, err)
				}
			}
			if queries := stateQueryCount(StateQueryConnectH1, true, stateQueryNoActiveRow) - before; queries != 1 {
				t.Errorf("3 checks of a removed client ran %.0f state queries, want the first alone", queries)
			}
		}

		// a personal network's tagged sidecar
		personal := newTestingStateRefusalNetwork(ctx)
		personalTagged := personal.taggedClient(ctx, nil)
		personalTagged.SetStrictExp(true)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client_owner(client_id,sponsor_user_id,tagged,admin_refused,strict_exp) VALUES($1,$2,true,true,true)`, *personalTagged.ClientId, personal.userId))
		})
		if err := ValidateByJwtState(ctx, personalTagged, true); err != nil {
			t.Fatalf("a personal network's tagged sidecar: %v", err)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_user SET credential_change_time = $2 WHERE user_id = $1`, personal.userId, server.NowUtc().Add(time.Minute)))
		})
		if err := ValidateByJwtState(ctx, personalTagged, true); !errors.Is(err, ErrByJwtInactive) {
			t.Fatalf("a personal network's tagged sidecar outlived its owner's rotation: %v", err)
		}
	})
}

// On the revocation record, the first check of each lineage credential is the
// state query's, whose projection marks the client, and the record answers the
// later ones with no query. The record still asks each credential's claims: the
// tagged client's credential without strict_exp is refused from the record as
// rotated. A removal's tombstones refuse both credentials with no query.
func TestTeamTaggedLineageOnTheRevocationRecord(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		defer Testing_SetRevocationRecordEnabled(true)()
		ctx := WithStateQuerySource(t.Context(), StateQueryConnectH1)
		team := newTestOwnerOfRecordTeam(t, ctx)
		tagged := team.client(t, ctx, nil, true)
		window := team.client(t, ctx, tagged, false)
		for _, lineage := range []*ByJwt{tagged, window} {
			before := testingStateQueries(StateQueryConnectH1, true)
			for attempt := range 3 {
				if err := ValidateByJwtState(ctx, lineage, true); err != nil {
					t.Fatalf("check %d of a lineage credential: %v", attempt, err)
				}
			}
			if queries := testingStateQueries(StateQueryConnectH1, true) - before; queries != 1 {
				t.Errorf("3 checks of a lineage credential ran %.0f state queries, want the first alone", queries)
			}
			value, ok := testingRevocationField(ctx, t, Testing_RevocationRecordKey(team.networkId, *tagged.ClientId), Testing_RevocationClientField(*lineage.ClientId))
			if !ok || len(value) < 2 || value[len(value)-2:] != " "+revocationOwnerOfRecordTagged {
				t.Fatalf("the client's projection %q carries no mark", value)
			}
		}

		before := testingStateQueries(StateQueryConnectH1, true)
		looseClaims := testOwnerOfRecordVariant(tagged, func(variant *ByJwt) { variant.StrictExp = false })
		if err := ValidateByJwtState(ctx, looseClaims, true); !errors.Is(err, ErrByJwtInactive) {
			t.Fatalf("the tagged client's credential without strict_exp: %v", err)
		}
		team.deactivate(ctx, tagged, window)
		for _, removed := range []*ByJwt{tagged, window} {
			if err := ValidateByJwtState(ctx, removed, true); !errors.Is(err, ErrByJwtInactive) {
				t.Fatalf("a removed client's credential: %v", err)
			}
		}
		if queries := testingStateQueries(StateQueryConnectH1, true) - before; queries != 0 {
			t.Errorf("the record's refusals ran %.0f state queries", queries)
		}
	})
}
