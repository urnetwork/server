// Claiming a Top 200 spot from one tap (apple BTEASY.md, "Claiming a Top 200 spot").
// The tables behind controller/sn_fleet_auto.go. The server chooses devices, batches
// manifest changes once per settlement epoch, carries each device its binding and
// relays the result. It holds no key that signs a binding: the hotkey and each client
// key sign on their own devices, and a device checks everything it's handed.
package model

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/urnetwork/server"
)

// A kind of transaction the relay owes.
const (
	SnFleetAutoRelayBind   = "bind"
	SnFleetAutoRelayRevoke = "revoke"
	SnFleetAutoRelayTopUp  = "top_up"
)

// Where one relayed transaction stands.
const (
	// nothing signed yet
	SnFleetAutoRelayQueued = "queued"
	// signed and maybe sent; only its receipt settles it
	SnFleetAutoRelaySent = "sent"
	// final on chain
	SnFleetAutoRelayDone = "done"
	// refused before any gas was spent, or reverted
	SnFleetAutoRelayFailed = "failed"
)

// Which key pays. They are separate so a top-up's funding can't drain the binds'.
const (
	SnFleetAutoRelayKeyBinding = "binding"
	SnFleetAutoRelayKeyTopUp   = "top_up"
)

// The coldkey's signed authorization of a hotkey.
type SnFleetAutoAuthorization struct {
	NetworkId  server.Id
	Coldkey    [32]byte
	Hotkey     [32]byte
	FleetId    [32]byte
	Message    string
	Signature  []byte
	CreateTime time.Time
	UpdateTime time.Time
}

// One device that asked for work.
type SnFleetAutoDevice struct {
	NetworkId     server.Id
	ClientId      server.Id
	LastCheckTime time.Time
	LeftTime      *time.Time
}

// One manifest.
type SnFleetAutoGeneration struct {
	NetworkId      server.Id
	Generation     uint64
	BuiltEpoch     uint64
	FleetId        [32]byte
	Hotkey         [32]byte
	Manifest       string
	CommitmentHash [32]byte
	// the finalized block of the coordinator's mirror of this commitment
	MirroredBlock *uint64
	CreateTime    time.Time
}

// One chosen device of a manifest. A device kept from an earlier generation has no
// binding here: BindingJson is empty.
type SnFleetAutoMember struct {
	NetworkId       server.Id
	Generation      uint64
	ClientId        server.Id
	ClientKey       [32]byte
	PrefixHash      [32]byte
	ValidFromEpoch  uint64
	ValidToEpoch    uint64
	Digest          []byte
	BindingJson     string
	HotkeySignature []byte
	ClientSignature []byte
	Refusal         string
	RelayId         *server.Id
	UpdateTime      time.Time
}

// One transaction the relay owes.
type SnFleetAutoRelay struct {
	RelayId        server.Id
	NetworkId      server.Id
	Kind           string
	Operation      string
	FromKey        string
	ToAddress      string
	Calldata       []byte
	ValueWei       string
	Status         string
	FromAddress    string
	Nonce          *uint64
	TxHash         string
	RawTransaction []byte
	Error          string
	CreateTime     time.Time
	UpdateTime     time.Time
}

// A network's one top-up.
type SnFleetAutoTopUp struct {
	NetworkId  server.Id
	Coldkey    [32]byte
	BurnRao    uint64
	FeeRao     uint64
	AmountRao  uint64
	RelayId    server.Id
	CreateTime time.Time
	// the relay's status
	Status string
}

const snFleetAutoAuthorizationColumns = `network_id, coldkey, hotkey, fleet_id, message, signature, create_time, update_time`

func scanSnFleetAutoAuthorization(rows server.PgResult) *SnFleetAutoAuthorization {
	out := &SnFleetAutoAuthorization{}
	var coldkey, hotkey, fleetId []byte
	server.Raise(rows.Scan(&out.NetworkId, &coldkey, &hotkey, &fleetId, &out.Message, &out.Signature, &out.CreateTime, &out.UpdateTime))
	out.Coldkey, out.Hotkey, out.FleetId = [32]byte(coldkey), [32]byte(hotkey), [32]byte(fleetId)
	return out
}

// ErrSnFleetAutoHotkeyTaken: another network's coldkey authorized this hotkey first.
var ErrSnFleetAutoHotkeyTaken = errors.New("this hotkey is authorized for another network")

// SetSnFleetAutoAuthorization stores the coldkey's authorization, checks in the same
// transaction that the caller administers the network, and owes the admin the notice
// when it changes the hotkey: from then on devices sign bindings to that hotkey with
// nobody looking, unless they pinned another.
func SetSnFleetAutoAuthorization(ctx context.Context, deploymentKey StDeploymentKey, userId server.Id, authorization *SnFleetAutoAuthorization) (returnErr error) {
	key := requireStDeploymentKey(deploymentKey)
	defer func() {
		if recovered := recover(); recovered != nil {
			if abort, ok := recovered.(snFleetAutoAbort); ok {
				returnErr = abort.cause
				return
			}
			panic(recovered)
		}
	}()
	server.Tx(ctx, func(tx server.PgTx) {
		var adminUserId server.Id
		err := tx.QueryRow(ctx, `SELECT admin_user_id FROM network WHERE network_id = $1 FOR SHARE`, authorization.NetworkId).Scan(&adminUserId)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && adminUserId != userId {
			panic(snFleetAutoAbort{cause: errors.New("only the network's owner can authorize its hotkey")})
		}
		server.Raise(err)

		var taken bool
		server.Raise(tx.QueryRow(
			ctx,
			`SELECT EXISTS (SELECT 1 FROM sn_fleet_auto_authorization WHERE deployment_key = $1 AND hotkey = $2 AND network_id <> $3)`,
			key, authorization.Hotkey[:], authorization.NetworkId,
		).Scan(&taken))
		if taken {
			panic(snFleetAutoAbort{cause: ErrSnFleetAutoHotkeyTaken})
		}

		var previous []byte
		err = tx.QueryRow(ctx, `SELECT hotkey FROM sn_fleet_auto_authorization WHERE deployment_key = $1 AND network_id = $2 FOR UPDATE`, key, authorization.NetworkId).Scan(&previous)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			server.Raise(err)
		}
		now := server.NowUtc()
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO sn_fleet_auto_authorization (deployment_key, network_id, coldkey, hotkey, fleet_id, message, signature, create_time, update_time)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8)
				ON CONFLICT (deployment_key, network_id) DO UPDATE
				SET coldkey = EXCLUDED.coldkey, hotkey = EXCLUDED.hotkey, fleet_id = EXCLUDED.fleet_id,
					message = EXCLUDED.message, signature = EXCLUDED.signature, update_time = EXCLUDED.update_time
			`,
			key, authorization.NetworkId, authorization.Coldkey[:], authorization.Hotkey[:], authorization.FleetId[:], authorization.Message, authorization.Signature, now,
		))
		oweSubnetConsentNoticeInTx(ctx, tx, authorization.NetworkId, SecurityWalletDestinationFleetHotkey, func() *securityWallet {
			return subnetStoredKeyWallet(previous)
		}, authorization.Hotkey, 0, 0)
	})
	return nil
}

type snFleetAutoAbort struct {
	cause error
}

func GetSnFleetAutoAuthorization(ctx context.Context, deploymentKey StDeploymentKey, networkId server.Id) (authorization *SnFleetAutoAuthorization) {
	key := requireStDeploymentKey(deploymentKey)
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, `SELECT `+snFleetAutoAuthorizationColumns+` FROM sn_fleet_auto_authorization WHERE deployment_key = $1 AND network_id = $2`, key, networkId)
		server.WithPgResult(rows, err, func() {
			if rows.Next() {
				authorization = scanSnFleetAutoAuthorization(rows)
			}
		})
	})
	return
}

// Every authorization of the deployment: at most one a network, a short list.
func GetSnFleetAutoAuthorizations(ctx context.Context, deploymentKey StDeploymentKey) []*SnFleetAutoAuthorization {
	key := requireStDeploymentKey(deploymentKey)
	authorizations := []*SnFleetAutoAuthorization{}
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, `SELECT `+snFleetAutoAuthorizationColumns+` FROM sn_fleet_auto_authorization WHERE deployment_key = $1 ORDER BY create_time, network_id`, key)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				authorizations = append(authorizations, scanSnFleetAutoAuthorization(rows))
			}
		})
	})
	return authorizations
}

// The networks with an authorization: the short list every device's check is
// answered from.
func GetSnFleetAutoNetworkIds(ctx context.Context, deploymentKey StDeploymentKey) map[server.Id]bool {
	key := requireStDeploymentKey(deploymentKey)
	networkIds := map[server.Id]bool{}
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, `SELECT network_id FROM sn_fleet_auto_authorization WHERE deployment_key = $1`, key)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var networkId server.Id
				server.Raise(rows.Scan(&networkId))
				networkIds[networkId] = true
			}
		})
	})
	return networkIds
}

// Records that a device of an authorized network asked: it runs a release with this
// support and has signing on.
func TouchSnFleetAutoDevice(ctx context.Context, deploymentKey StDeploymentKey, networkId server.Id, clientId server.Id, now time.Time) {
	key := requireStDeploymentKey(deploymentKey)
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO sn_fleet_auto_device (deployment_key, network_id, client_id, last_check_time)
				VALUES ($1, $2, $3, $4)
				ON CONFLICT (deployment_key, network_id, client_id) DO UPDATE SET last_check_time = EXCLUDED.last_check_time
			`,
			key, networkId, clientId, now,
		))
		// one statement, so a device's parallel requests don't fail each other
	}, server.TxReadCommitted)
}

// The network's devices that asked since then. A device that left is included, so
// the plan can tell it from one that never asked.
func GetSnFleetAutoDevices(ctx context.Context, deploymentKey StDeploymentKey, networkId server.Id, since time.Time) []*SnFleetAutoDevice {
	key := requireStDeploymentKey(deploymentKey)
	devices := []*SnFleetAutoDevice{}
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(
			ctx,
			`SELECT network_id, client_id, last_check_time, left_time FROM sn_fleet_auto_device WHERE deployment_key = $1 AND network_id = $2 AND $3 <= last_check_time ORDER BY client_id`,
			key, networkId, since,
		)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				device := &SnFleetAutoDevice{}
				server.Raise(rows.Scan(&device.NetworkId, &device.ClientId, &device.LastCheckTime, &device.LeftTime))
				devices = append(devices, device)
			}
		})
	})
	return devices
}

// A device left: it isn't chosen again, and what it hasn't finished is dropped. It
// has to rejoin through a person, by asking again after they reset it.
func SetSnFleetAutoDeviceLeft(ctx context.Context, deploymentKey StDeploymentKey, networkId server.Id, clientId server.Id, now time.Time) {
	key := requireStDeploymentKey(deploymentKey)
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO sn_fleet_auto_device (deployment_key, network_id, client_id, last_check_time, left_time)
				VALUES ($1, $2, $3, $4, $4)
				ON CONFLICT (deployment_key, network_id, client_id) DO UPDATE SET left_time = EXCLUDED.left_time, last_check_time = EXCLUDED.last_check_time
			`,
			key, networkId, clientId, now,
		))
		// a binding not yet on chain is dropped. Its relay row, if any, is the
		// relay's: a sent transaction may still land and is revoked by its own Leave
		server.RaisePgResult(tx.Exec(
			ctx,
			`UPDATE sn_fleet_auto_member SET refusal = 'device_left', update_time = $4 WHERE deployment_key = $1 AND network_id = $2 AND client_id = $3 AND relay_id IS NULL AND digest IS NOT NULL AND refusal IS NULL`,
			key, networkId, clientId, now,
		))
	})
}

// The network has asked before, but a person reset this device: it may be chosen
// again.
func ClearSnFleetAutoDeviceLeft(ctx context.Context, deploymentKey StDeploymentKey, networkId server.Id, clientId server.Id) {
	key := requireStDeploymentKey(deploymentKey)
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`UPDATE sn_fleet_auto_device SET left_time = NULL WHERE deployment_key = $1 AND network_id = $2 AND client_id = $3`,
			key, networkId, clientId,
		))
	})
}

const snFleetAutoGenerationColumns = `network_id, generation, built_epoch, fleet_id, hotkey, manifest, commitment_hash, mirrored_block, create_time`

func scanSnFleetAutoGeneration(rows server.PgResult) *SnFleetAutoGeneration {
	out := &SnFleetAutoGeneration{}
	var generation, builtEpoch int64
	var mirrored *int64
	var fleetId, hotkey, hash []byte
	server.Raise(rows.Scan(&out.NetworkId, &generation, &builtEpoch, &fleetId, &hotkey, &out.Manifest, &hash, &mirrored, &out.CreateTime))
	out.Generation, out.BuiltEpoch = uint64(generation), uint64(builtEpoch)
	out.FleetId, out.Hotkey, out.CommitmentHash = [32]byte(fleetId), [32]byte(hotkey), [32]byte(hash)
	if mirrored != nil {
		block := uint64(*mirrored)
		out.MirroredBlock = &block
	}
	return out
}

// InsertSnFleetAutoGeneration stores a new manifest and its members. Manifest changes
// are batched once per epoch: a second one for a network in the same epoch changes
// nothing and returns false.
func InsertSnFleetAutoGeneration(ctx context.Context, deploymentKey StDeploymentKey, generation *SnFleetAutoGeneration, members []*SnFleetAutoMember) (inserted bool) {
	key := requireStDeploymentKey(deploymentKey)
	server.Tx(ctx, func(tx server.PgTx) {
		inserted = false
		now := server.NowUtc()
		tag, err := tx.Exec(
			ctx,
			`
				INSERT INTO sn_fleet_auto_generation (deployment_key, network_id, generation, built_epoch, fleet_id, hotkey, manifest, commitment_hash, create_time)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
				ON CONFLICT (deployment_key, network_id, built_epoch) DO NOTHING
			`,
			key, generation.NetworkId, int64(generation.Generation), int64(generation.BuiltEpoch), generation.FleetId[:], generation.Hotkey[:], generation.Manifest, generation.CommitmentHash[:], now,
		)
		server.Raise(err)
		if tag.RowsAffected() == 0 {
			return
		}
		inserted = true
		for _, member := range members {
			var validFrom, validTo *int64
			var digest []byte
			var bindingJson *string
			if member.BindingJson != "" {
				from, to := int64(member.ValidFromEpoch), int64(member.ValidToEpoch)
				validFrom, validTo, digest, bindingJson = &from, &to, member.Digest, &member.BindingJson
			}
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					INSERT INTO sn_fleet_auto_member (deployment_key, network_id, generation, client_id, client_key, prefix_hash, valid_from_epoch, valid_to_epoch, digest, binding_json, update_time)
					VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
				`,
				key, generation.NetworkId, int64(generation.Generation), member.ClientId, member.ClientKey[:], member.PrefixHash[:], validFrom, validTo, digest, bindingJson, now,
			))
		}
	})
	return
}

// The network's newest manifest, nil before its first.
func GetSnFleetAutoNewestGeneration(ctx context.Context, deploymentKey StDeploymentKey, networkId server.Id) (generation *SnFleetAutoGeneration) {
	key := requireStDeploymentKey(deploymentKey)
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(
			ctx,
			`SELECT `+snFleetAutoGenerationColumns+` FROM sn_fleet_auto_generation WHERE deployment_key = $1 AND network_id = $2 ORDER BY generation DESC LIMIT 1`,
			key, networkId,
		)
		server.WithPgResult(rows, err, func() {
			if rows.Next() {
				generation = scanSnFleetAutoGeneration(rows)
			}
		})
	})
	return
}

func SetSnFleetAutoGenerationMirrored(ctx context.Context, deploymentKey StDeploymentKey, networkId server.Id, generation uint64, block uint64) {
	key := requireStDeploymentKey(deploymentKey)
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`UPDATE sn_fleet_auto_generation SET mirrored_block = $4 WHERE deployment_key = $1 AND network_id = $2 AND generation = $3`,
			key, networkId, int64(generation), int64(block),
		))
	})
}

const snFleetAutoMemberColumns = `network_id, generation, client_id, client_key, prefix_hash, valid_from_epoch, valid_to_epoch, digest, binding_json, hotkey_signature, client_signature, refusal, relay_id, update_time`

func scanSnFleetAutoMember(rows server.PgResult) *SnFleetAutoMember {
	out := &SnFleetAutoMember{}
	var generation int64
	var validFrom, validTo *int64
	var clientKey, prefix []byte
	var bindingJson, refusal *string
	server.Raise(rows.Scan(&out.NetworkId, &generation, &out.ClientId, &clientKey, &prefix, &validFrom, &validTo, &out.Digest, &bindingJson, &out.HotkeySignature, &out.ClientSignature, &refusal, &out.RelayId, &out.UpdateTime))
	out.Generation = uint64(generation)
	out.ClientKey, out.PrefixHash = [32]byte(clientKey), [32]byte(prefix)
	if validFrom != nil {
		out.ValidFromEpoch = uint64(*validFrom)
	}
	if validTo != nil {
		out.ValidToEpoch = uint64(*validTo)
	}
	if bindingJson != nil {
		out.BindingJson = *bindingJson
	}
	if refusal != nil {
		out.Refusal = *refusal
	}
	return out
}

func GetSnFleetAutoMembers(ctx context.Context, deploymentKey StDeploymentKey, networkId server.Id, generation uint64) []*SnFleetAutoMember {
	key := requireStDeploymentKey(deploymentKey)
	members := []*SnFleetAutoMember{}
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(
			ctx,
			`SELECT `+snFleetAutoMemberColumns+` FROM sn_fleet_auto_member WHERE deployment_key = $1 AND network_id = $2 AND generation = $3 ORDER BY client_id`,
			key, networkId, int64(generation),
		)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				members = append(members, scanSnFleetAutoMember(rows))
			}
		})
	})
	return members
}

// The binding to offer this device: its newest with the hotkey's signature, unless
// it refused, and for a generation still current.
func GetSnFleetAutoDeviceMember(ctx context.Context, deploymentKey StDeploymentKey, networkId server.Id, clientId server.Id) (member *SnFleetAutoMember) {
	key := requireStDeploymentKey(deploymentKey)
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(
			ctx,
			`
				SELECT `+snFleetAutoMemberColumns+` FROM sn_fleet_auto_member
				WHERE deployment_key = $1 AND client_id = $2 AND network_id = $3 AND digest IS NOT NULL
				ORDER BY generation DESC LIMIT 1
			`,
			key, clientId, networkId,
		)
		server.WithPgResult(rows, err, func() {
			if rows.Next() {
				member = scanSnFleetAutoMember(rows)
			}
		})
	})
	return
}

// Stores the hotkey's signature on the binding it signed, by the digest the signer
// computed. Returns false when no open binding has that digest.
func SetSnFleetAutoHotkeySignature(ctx context.Context, deploymentKey StDeploymentKey, networkId server.Id, digest []byte, signature []byte) (stored bool) {
	key := requireStDeploymentKey(deploymentKey)
	server.Tx(ctx, func(tx server.PgTx) {
		tag, err := tx.Exec(
			ctx,
			`
				UPDATE sn_fleet_auto_member SET hotkey_signature = $4, update_time = $5
				WHERE deployment_key = $1 AND network_id = $2 AND digest = $3 AND hotkey_signature IS NULL AND refusal IS NULL
			`,
			key, networkId, digest, signature, server.NowUtc(),
		)
		server.Raise(err)
		stored = 0 < tag.RowsAffected()
	})
	return
}

// Stores a device's signature on its own binding. Returns false when it isn't this
// client's open binding.
func SetSnFleetAutoClientSignature(ctx context.Context, deploymentKey StDeploymentKey, networkId server.Id, clientId server.Id, digest []byte, signature []byte) (stored bool) {
	key := requireStDeploymentKey(deploymentKey)
	server.Tx(ctx, func(tx server.PgTx) {
		tag, err := tx.Exec(
			ctx,
			`
				UPDATE sn_fleet_auto_member SET client_signature = $5, update_time = $6
				WHERE deployment_key = $1 AND network_id = $2 AND client_id = $3 AND digest = $4 AND refusal IS NULL
			`,
			key, networkId, clientId, digest, signature, server.NowUtc(),
		)
		server.Raise(err)
		stored = 0 < tag.RowsAffected()
	})
	return
}

// A device refused its newest open binding. The server stops offering it, and the
// next batch leaves the device out.
func SetSnFleetAutoRefusal(ctx context.Context, deploymentKey StDeploymentKey, networkId server.Id, clientId server.Id, code string) {
	key := requireStDeploymentKey(deploymentKey)
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE sn_fleet_auto_member SET refusal = $4, update_time = $5
				WHERE deployment_key = $1 AND network_id = $2 AND client_id = $3 AND digest IS NOT NULL AND client_signature IS NULL AND refusal IS NULL
			`,
			key, networkId, clientId, code, server.NowUtc(),
		))
	})
}

// The members of the network with both signatures that the relay hasn't taken.
func GetSnFleetAutoSignedMembers(ctx context.Context, deploymentKey StDeploymentKey, networkId server.Id, generation uint64) []*SnFleetAutoMember {
	key := requireStDeploymentKey(deploymentKey)
	members := []*SnFleetAutoMember{}
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(
			ctx,
			`
				SELECT `+snFleetAutoMemberColumns+` FROM sn_fleet_auto_member
				WHERE deployment_key = $1 AND network_id = $2 AND generation = $3
					AND hotkey_signature IS NOT NULL AND client_signature IS NOT NULL AND refusal IS NULL AND relay_id IS NULL
				ORDER BY client_id
			`,
			key, networkId, int64(generation),
		)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				members = append(members, scanSnFleetAutoMember(rows))
			}
		})
	})
	return members
}

const snFleetAutoRelayColumns = `relay_id, network_id, kind, operation, from_key, to_address, calldata, value_wei, status, from_address, nonce, tx_hash, raw_transaction, error, create_time, update_time`

func scanSnFleetAutoRelay(rows server.PgResult) *SnFleetAutoRelay {
	out := &SnFleetAutoRelay{}
	var fromAddress, txHash, relayError *string
	var nonce *int64
	server.Raise(rows.Scan(&out.RelayId, &out.NetworkId, &out.Kind, &out.Operation, &out.FromKey, &out.ToAddress, &out.Calldata, &out.ValueWei, &out.Status, &fromAddress, &nonce, &txHash, &out.RawTransaction, &relayError, &out.CreateTime, &out.UpdateTime))
	if fromAddress != nil {
		out.FromAddress = *fromAddress
	}
	if txHash != nil {
		out.TxHash = *txHash
	}
	if relayError != nil {
		out.Error = *relayError
	}
	if nonce != nil {
		value := uint64(*nonce)
		out.Nonce = &value
	}
	return out
}

func snFleetAutoEnqueueRelayInTx(ctx context.Context, tx server.PgTx, key string, relay *SnFleetAutoRelay) bool {
	if relay.ValueWei == "" {
		relay.ValueWei = "0"
	}
	now := server.NowUtc()
	tag, err := tx.Exec(
		ctx,
		`
			INSERT INTO sn_fleet_auto_relay (relay_id, deployment_key, network_id, kind, operation, from_key, to_address, calldata, value_wei, status, create_time, update_time)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $11)
			ON CONFLICT (deployment_key, operation) DO NOTHING
		`,
		relay.RelayId, key, relay.NetworkId, relay.Kind, relay.Operation, relay.FromKey, relay.ToAddress, relay.Calldata, relay.ValueWei, SnFleetAutoRelayQueued, now,
	)
	server.Raise(err)
	return 0 < tag.RowsAffected()
}

// EnqueueSnFleetAutoRelayForMember queues one bind and ties it to its member, so a
// member is relayed at most once. Returns false when it was queued already.
func EnqueueSnFleetAutoRelayForMember(ctx context.Context, deploymentKey StDeploymentKey, relay *SnFleetAutoRelay, member *SnFleetAutoMember) (queued bool) {
	key := requireStDeploymentKey(deploymentKey)
	server.Tx(ctx, func(tx server.PgTx) {
		queued = snFleetAutoEnqueueRelayInTx(ctx, tx, key, relay)
		if !queued {
			return
		}
		server.RaisePgResult(tx.Exec(
			ctx,
			`UPDATE sn_fleet_auto_member SET relay_id = $5, update_time = $6 WHERE deployment_key = $1 AND network_id = $2 AND generation = $3 AND client_id = $4`,
			key, member.NetworkId, int64(member.Generation), member.ClientId, relay.RelayId, server.NowUtc(),
		))
	})
	return
}

// EnqueueSnFleetAutoRelay queues one operation. A repeat is the same operation:
// false, and nothing changes.
func EnqueueSnFleetAutoRelay(ctx context.Context, deploymentKey StDeploymentKey, relay *SnFleetAutoRelay) (queued bool) {
	key := requireStDeploymentKey(deploymentKey)
	server.Tx(ctx, func(tx server.PgTx) {
		queued = snFleetAutoEnqueueRelayInTx(ctx, tx, key, relay)
	})
	return
}

// The relay's open work, oldest first: what was sent comes before anything new, so
// one account's nonces stay in order.
func GetSnFleetAutoRelayOpen(ctx context.Context, deploymentKey StDeploymentKey, limit int) []*SnFleetAutoRelay {
	key := requireStDeploymentKey(deploymentKey)
	relays := []*SnFleetAutoRelay{}
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(
			ctx,
			`
				SELECT `+snFleetAutoRelayColumns+` FROM sn_fleet_auto_relay
				WHERE deployment_key = $1 AND status IN ('queued', 'sent')
				ORDER BY (status = 'sent') DESC, create_time, relay_id
				LIMIT $2
			`,
			key, limit,
		)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				relays = append(relays, scanSnFleetAutoRelay(rows))
			}
		})
	})
	return relays
}

// The durable record of a signed transaction, written before it is sent. A restart
// settles it from its receipt or sends the same bytes again.
func SetSnFleetAutoRelaySent(ctx context.Context, relayId server.Id, fromAddress string, nonce uint64, txHash string, raw []byte) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`UPDATE sn_fleet_auto_relay SET status = $2, from_address = $3, nonce = $4, tx_hash = $5, raw_transaction = $6, update_time = $7 WHERE relay_id = $1`,
			relayId, SnFleetAutoRelaySent, fromAddress, int64(nonce), txHash, raw, server.NowUtc(),
		))
	})
}

func SetSnFleetAutoRelaySettled(ctx context.Context, relayId server.Id, status string, relayError string) {
	var stored *string
	if relayError != "" {
		stored = &relayError
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`UPDATE sn_fleet_auto_relay SET status = $2, error = $3, update_time = $4 WHERE relay_id = $1`,
			relayId, status, stored, server.NowUtc(),
		))
	})
}

// The least nonce a key's next transaction may use: past everything it signed, so a
// lagging endpoint can't make it reuse one.
func GetSnFleetAutoRelayNonceFloor(ctx context.Context, deploymentKey StDeploymentKey, fromAddress string) (floor uint64) {
	key := requireStDeploymentKey(deploymentKey)
	server.Db(ctx, func(conn server.PgConn) {
		var highest *int64
		server.Raise(conn.QueryRow(
			ctx,
			`SELECT MAX(nonce) FROM sn_fleet_auto_relay WHERE deployment_key = $1 AND from_address = $2`,
			key, fromAddress,
		).Scan(&highest))
		if highest != nil {
			floor = uint64(*highest) + 1
		}
	})
	return
}

// One relay row by its id, nil when there's none.
func GetSnFleetAutoRelay(ctx context.Context, relayId server.Id) (relay *SnFleetAutoRelay) {
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, `SELECT `+snFleetAutoRelayColumns+` FROM sn_fleet_auto_relay WHERE relay_id = $1`, relayId)
		server.WithPgResult(rows, err, func() {
			if rows.Next() {
				relay = scanSnFleetAutoRelay(rows)
			}
		})
	})
	return
}

// ClaimSnFleetAutoTopUp takes the network's one top-up and queues its transfer in the
// same transaction. A network that had one gets false, whatever happened to it.
func ClaimSnFleetAutoTopUp(ctx context.Context, deploymentKey StDeploymentKey, topUp *SnFleetAutoTopUp, relay *SnFleetAutoRelay) (claimed bool) {
	key := requireStDeploymentKey(deploymentKey)
	server.Tx(ctx, func(tx server.PgTx) {
		claimed = false
		tag, err := tx.Exec(
			ctx,
			`
				INSERT INTO sn_fleet_auto_top_up (deployment_key, network_id, coldkey, burn_rao, fee_rao, amount_rao, relay_id, create_time)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
				ON CONFLICT (deployment_key, network_id) DO NOTHING
			`,
			key, topUp.NetworkId, topUp.Coldkey[:], int64(topUp.BurnRao), int64(topUp.FeeRao), int64(topUp.AmountRao), relay.RelayId, server.NowUtc(),
		)
		server.Raise(err)
		if tag.RowsAffected() == 0 {
			return
		}
		claimed = true
		snFleetAutoEnqueueRelayInTx(ctx, tx, key, relay)
	})
	return
}

// The network's one top-up with where it stands, nil before it has had one.
func GetSnFleetAutoTopUp(ctx context.Context, deploymentKey StDeploymentKey, networkId server.Id) (topUp *SnFleetAutoTopUp) {
	key := requireStDeploymentKey(deploymentKey)
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(
			ctx,
			`
				SELECT t.network_id, t.coldkey, t.burn_rao, t.fee_rao, t.amount_rao, t.relay_id, t.create_time, r.status
				FROM sn_fleet_auto_top_up t
				INNER JOIN sn_fleet_auto_relay r ON r.relay_id = t.relay_id
				WHERE t.deployment_key = $1 AND t.network_id = $2
			`,
			key, networkId,
		)
		server.WithPgResult(rows, err, func() {
			if rows.Next() {
				topUp = &SnFleetAutoTopUp{}
				var coldkey []byte
				var burn, fee, amount int64
				server.Raise(rows.Scan(&topUp.NetworkId, &coldkey, &burn, &fee, &amount, &topUp.RelayId, &topUp.CreateTime, &topUp.Status))
				topUp.Coldkey, topUp.BurnRao, topUp.FeeRao, topUp.AmountRao = [32]byte(coldkey), uint64(burn), uint64(fee), uint64(amount)
			}
		})
	})
	return
}

// One client's binding versions, in bind order.
func GetSnFleetBindingVersionsForClients(ctx context.Context, deploymentKey StDeploymentKey, clientIds []server.Id) []*SnFleetBindingVersion {
	key := requireStDeploymentKey(deploymentKey)
	versions := []*SnFleetBindingVersion{}
	if len(clientIds) == 0 {
		return versions
	}
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(
			ctx,
			`SELECT `+snFleetBindingVersionColumns+` FROM sn_fleet_binding WHERE deployment_key = $1 AND client_id = ANY($2) ORDER BY client_id, bound_block, bound_log_index`,
			key, clientIds,
		)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				versions = append(versions, scanSnFleetBindingVersion(rows))
			}
		})
	})
	return versions
}

// CountSnFleetAutoStillToUpdate is the network's online providers that haven't asked
// for work since then: on an older release, or with signing turned off. Read on
// demand for one network's card, by its network index.
func CountSnFleetAutoStillToUpdate(ctx context.Context, deploymentKey StDeploymentKey, networkId server.Id, since time.Time) (count int64) {
	key := requireStDeploymentKey(deploymentKey)
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`
				SELECT count(*) FROM network_client
				WHERE network_client.network_id = $1 AND network_client.active AND network_client.source_client_id IS NULL
					AND EXISTS (
						SELECT 1 FROM network_client_connection
						WHERE network_client_connection.client_id = network_client.client_id AND network_client_connection.connected
					)
					AND EXISTS (
						SELECT 1 FROM provide_key
						WHERE provide_key.client_id = network_client.client_id AND provide_key.provide_mode = $2
					)
					AND NOT EXISTS (
						SELECT 1 FROM sn_fleet_auto_device
						WHERE sn_fleet_auto_device.deployment_key = $3
							AND sn_fleet_auto_device.network_id = network_client.network_id
							AND sn_fleet_auto_device.client_id = network_client.client_id
							AND $4 <= sn_fleet_auto_device.last_check_time
					)
			`,
			networkId, ProvideModePublic, key, since,
		).Scan(&count))
	})
	return
}

// The network's newest accepted consent exactly as it was signed, nil without one.
// A device pins the coldkey from this the first time it sees one.
func GetSnFleetAutoConsentOriginal(ctx context.Context, domainHash [32]byte, networkId server.Id) (original []byte) {
	server.Db(ctx, func(conn server.PgConn) {
		err := conn.QueryRow(
			ctx,
			`SELECT original FROM network_wallet_mapping_consent WHERE domain_hash = $1 AND network_id = $2 ORDER BY generation DESC LIMIT 1`,
			domainHash[:], networkId,
		).Scan(&original)
		if errors.Is(err, pgx.ErrNoRows) {
			return
		}
		server.Raise(err)
	})
	return
}

// Which of these clients are eligible for the verify set, the only ones the validator
// can measure.
func GetVerifyEligibleClients(ctx context.Context, clientIds []server.Id) map[server.Id]bool {
	eligible := map[server.Id]bool{}
	if len(clientIds) == 0 {
		return eligible
	}
	args := make([]any, len(clientIds))
	for i, clientId := range clientIds {
		args[i] = clientId.String()
	}
	server.Redis(ctx, func(r server.RedisClient) {
		members, err := r.SMIsMember(ctx, verifyEligibleKey, args...).Result()
		server.Raise(err)
		for i, member := range members {
			if member {
				eligible[clientIds[i]] = true
			}
		}
	})
	return eligible
}
