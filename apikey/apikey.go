package apikey

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"fmt"

	"github.com/urnetwork/server"
)

type NetworkByApiKey struct {
	ApiKeyId    server.Id
	NetworkId   server.Id
	UserId      server.Id
	NetworkName string
	// in a team network, the membership of the key's creator, whose user is
	// UserId (TEAM-M6-02); nil in a personal network
	MemberId *server.Id
}

// The network the key stands for, or nil for no live key. A key past its expiry
// (§7.8) is no key; a legacy key has none. The expiry is MG0's column, read only
// where the binary links it.
//
// In a personal network the key acts as the network's owner, admin_user_id. A
// team's admin_user_id is a synthetic owner of record (D-TEAM-2), so there the
// key acts as its creator's membership, its user and member_id, and a key whose
// creator holds no live membership is no key: it never stands for the owner of
// record (TEAM-M6-02).
func GetNetworkByApiKey(apiKey string, ctx context.Context) *NetworkByApiKey {
	var result *NetworkByApiKey

	if server.SidecarMg6Linked() {
		return getTeamAwareNetworkByApiKey(apiKey, ctx)
	}

	server.Db(ctx, func(conn server.PgConn) {

		apiKeyHash := sha256.Sum256([]byte(apiKey))
		sql := `
			SELECT
				account_api_key.api_key_id,
				network.network_id,
				network.admin_user_id,
				network.network_name
			FROM account_api_key
			JOIN network ON network.network_id = account_api_key.network_id
			WHERE account_api_key.api_key = $1
		`
		args := []any{hex.EncodeToString(apiKeyHash[:])}
		if server.SidecarMg0Linked() {
			sql = `
				SELECT
					account_api_key.api_key_id,
					network.network_id,
					network.admin_user_id,
					network.network_name
				FROM account_api_key
				JOIN network ON network.network_id = account_api_key.network_id
				WHERE
					account_api_key.api_key = $1 AND
					(account_api_key.expire_time IS NULL OR $2 < account_api_key.expire_time)
			`
			args = append(args, server.NowUtc())
		}
		rows, err := conn.Query(ctx, sql, args...)

		server.WithPgResult(rows, err, func() {
			if rows.Next() {
				var r NetworkByApiKey
				server.RaisePgResult(rows.Scan(
					&r.ApiKeyId,
					&r.NetworkId,
					&r.UserId,
					&r.NetworkName,
				), err)
				result = &r
			}
		})
	})

	return result
}

// GetNetworkByApiKey where the binary links MG6's members and teams.
func getTeamAwareNetworkByApiKey(apiKey string, ctx context.Context) *NetworkByApiKey {
	var result *NetworkByApiKey
	server.Db(ctx, func(conn server.PgConn) {
		apiKeyHash := sha256.Sum256([]byte(apiKey))
		rows, err := conn.Query(
			ctx,
			`
				SELECT
					account_api_key.api_key_id,
					network.network_id,
					network.admin_user_id,
					network.network_name,
					network_team.network_id IS NOT NULL,
					network_member.user_id,
					network_member.member_id
				FROM account_api_key
				JOIN network ON network.network_id = account_api_key.network_id
				LEFT JOIN network_team ON network_team.network_id = network.network_id
				LEFT JOIN network_member ON
					network_member.network_id = network.network_id AND
					network_member.user_id = account_api_key.creator_user_id AND
					NOT network_member.pending
				WHERE
					account_api_key.api_key = $1 AND
					(account_api_key.expire_time IS NULL OR $2 < account_api_key.expire_time)
			`,
			hex.EncodeToString(apiKeyHash[:]),
			server.NowUtc(),
		)
		server.WithPgResult(rows, err, func() {
			if rows.Next() {
				var r NetworkByApiKey
				var team bool
				var memberUserId *server.Id
				var memberId *server.Id
				server.Raise(rows.Scan(
					&r.ApiKeyId,
					&r.NetworkId,
					&r.UserId,
					&r.NetworkName,
					&team,
					&memberUserId,
					&memberId,
				))
				if team {
					if memberUserId == nil || memberId == nil {
						// the creator holds no live membership: no key
						return
					}
					r.UserId = *memberUserId
					r.MemberId = memberId
				}
				result = &r
			}
		})
	})
	return result
}

type CreateApiKeyResult struct {
	Id     server.Id `json:"id,omitempty"`
	ApiKey string    `json:"api_key,omitempty"`
}

func Testing_CreateApiKey(networkId server.Id, ctx context.Context) (result *CreateApiKeyResult, err error) {
	var apiKeyId server.Id
	var apiKey string

	server.Tx(ctx, func(tx server.PgTx) {
		apiKeyId = server.NewId()

		var keyErr error
		apiKey, keyErr = generateApiKey()
		if keyErr != nil {
			err = keyErr
			return
		}

		apiKeyHash := sha256.Sum256([]byte(apiKey))
		// a failed insert raises, which ends the transaction at once
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO account_api_key
				(
					api_key_id,
					network_id,
					api_key,
					name
				)
				VALUES ($1, $2, $3, $4)
			`,
			apiKeyId,
			networkId,
			hex.EncodeToString(apiKeyHash[:]),
			"testkey",
		))
	})

	if err != nil {
		return nil, err
	}

	return &CreateApiKeyResult{
		Id:     apiKeyId,
		ApiKey: apiKey,
	}, nil
}

// for testing
func generateApiKey() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	e := base32.HexEncoding.WithPadding(base32.NoPadding)
	return fmt.Sprintf("urn_%s", e.EncodeToString(b)), nil
}
