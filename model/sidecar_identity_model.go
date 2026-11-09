package model

// §5.15's identity tables (§5.5, X10, D-CORE-5; §2.8 api-5): a sidecar's
// durable identity, one row per sidecar_id in sidecar_identity, and the
// append-only log of its client generations in sidecar_identity_binding, each
// row holding the dual-signed binding's bytes verbatim. A client is reaped and
// re-parented; the identity is not, so neither table has a foreign key to
// network_client, the reaper (RemoveDisconnectedNetworkClients and the
// dependents it removes) never names them, and only a network's deletion
// removes its rows (removeSidecarIdentitiesInTx).
//
// The server is untrusted for both tables (I-4): a binding counts only once a
// verifier checks both signatures and the identity key against its own pin or
// the config, and status is a candidate a client verifies against the config
// (§5.6). The rules api enforces are plan-CORE-binding-wire's: a sidecar_id's
// first binding is generation 1 and creates its identity row; each later one
// is the newest plus one under that row's identity key; a client binds exactly
// once, a byte-identical retry excepted, so a copied token cannot bind a key of
// its own and refresh under it (§5.7). The caller verifies the signatures and
// that the bearer is the binding's client in the binding's network.
//
// Nothing here reads or writes either table unless MG1 is linked
// (server.SidecarMg1Linked).

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"time"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
)

// An identity's status, as api holds it.
const (
	SidecarIdentityStatusActive  = "active"
	SidecarIdentityStatusRevoked = "revoked"
)

// The identity key's algorithm in v0 (owner decision 44).
const SidecarIdentityAlgEd25519 = "ed25519"

// The most bindings one page of GET /sidecar/identity/<sidecar_id> lists
// (plan-CORE-client-to-sidecar-lookup): a format value of the route.
const SidecarIdentityBindingPageCount = 64

// What a bind appends, from a binding the caller verified
// (sidecar.VerifySidecarIdentityBinding).
type SidecarBindingRecord struct {
	NetworkId         server.Id
	SidecarId         server.Id
	ClientId          server.Id
	Generation        int64
	ClientPublicKey   []byte
	IdentityPublicKey []byte
	// The marshalled SignedSidecarIdentityBinding as received, which
	// sidecar_identity_binding.signatures keeps verbatim.
	SignedBinding []byte
}

// What a bind did, each refusal named by its route code
// (plan-CORE-binding-wire #6).
type SidecarBindOutcome string

const (
	// The binding is the sidecar's new newest generation.
	SidecarBindAppended SidecarBindOutcome = "appended"
	// The client's stored binding has exactly these bytes: a retry, which
	// writes nothing.
	SidecarBindRetried SidecarBindOutcome = "retried"
	// The identity is revoked.
	SidecarBindSidecarRevoked SidecarBindOutcome = "sidecar_revoked"
	// Not the newest generation plus one, another identity key or network than
	// the identity row's, or a re-registration the projection does not admit.
	SidecarBindGenerationConflict SidecarBindOutcome = "generation_conflict"
	// The client is bound already, with other bytes, under any sidecar_id.
	SidecarBindClientBound SidecarBindOutcome = "client_bound"
)

// The re-registration seam (§5.6, §19.10): after a Postgres restore lost a
// sidecar's identity row, the sidecar posts its newest binding again, whose
// generation is past 1, and api takes it only once the re-published
// projection admits that identity key as the sidecar_id's (CORE-M2-07):
// sidecarProjectionAdmitsIdentity. A test replaces it.
var sidecarReregistrationAdmits = sidecarProjectionAdmitsIdentity

// Whether the network's verified log, as this process's sequencer holds it,
// admits sidecarId under exactly identityPublicKey: a live, unparked row of
// that key. No sequencer, a log that does not load, no such row and another
// key all answer false, so a restored binding waits, refused as
// generation_conflict, and the sidecar posts it again at its next refresh.
func sidecarProjectionAdmitsIdentity(ctx context.Context, networkId server.Id, sidecarId server.Id, identityPublicKey []byte) bool {
	sequencer, err := CurrentConfigLogSequencer()
	if err != nil {
		return false
	}
	admitted, err := sequencer.AdmittedIdentityKey(ctx, networkId, sidecar.Id(sidecarId))
	if err != nil {
		glog.Warningf("[sidecar][%s]the projection did not load for a re-registration: %s\n", networkId, err)
		return false
	}
	return admitted != nil && bytes.Equal(admitted, identityPublicKey)
}

// Replaces the re-registration seam for a test, returning the restore.
func Testing_SetSidecarReregistrationAdmits(admits func(ctx context.Context, networkId server.Id, sidecarId server.Id, identityPublicKey []byte) bool) func() {
	previous := sidecarReregistrationAdmits
	sidecarReregistrationAdmits = admits
	return func() {
		sidecarReregistrationAdmits = previous
	}
}

// Appends record to its sidecar's bindings under plan-CORE-binding-wire's
// rules, and answers the outcome and the generation stored for the client. The
// first binding of a sidecar_id creates its identity row. The caller has
// verified the binding's signatures and that the bearer is its client in its
// network.
func BindSidecarIdentity(ctx context.Context, record *SidecarBindingRecord, now time.Time) (outcome SidecarBindOutcome, generation int64) {
	server.Tx(ctx, func(tx server.PgTx) {
		outcome, generation = bindSidecarIdentityInTx(ctx, tx, record, now)
	}, server.TxReadCommitted)
	return
}

func bindSidecarIdentityInTx(ctx context.Context, tx server.PgTx, record *SidecarBindingRecord, now time.Time) (SidecarBindOutcome, int64) {
	// a client binds once
	clientBinding := func() (SidecarBindOutcome, int64, bool) {
		var storedGeneration int64
		var storedSignatures []byte
		found := false
		result, err := tx.Query(
			ctx,
			`
				SELECT generation, signatures
				FROM sidecar_identity_binding
				WHERE client_id = $1
			`,
			record.ClientId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&storedGeneration, &storedSignatures))
				found = true
			}
		})
		if !found {
			return "", 0, false
		}
		if bytes.Equal(storedSignatures, record.SignedBinding) {
			return SidecarBindRetried, storedGeneration, true
		}
		return SidecarBindClientBound, 0, true
	}
	if outcome, storedGeneration, found := clientBinding(); found {
		return outcome, storedGeneration
	}

	// the identity row, locked so this sidecar's binds take their turns
	type identityRow struct {
		networkId         server.Id
		identityPublicKey []byte
		status            string
	}
	readIdentity := func() *identityRow {
		var row *identityRow
		result, err := tx.Query(
			ctx,
			`
				SELECT network_id, identity_public_key, status
				FROM sidecar_identity
				WHERE sidecar_id = $1
				FOR UPDATE
			`,
			record.SidecarId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				row = &identityRow{}
				server.Raise(result.Scan(&row.networkId, &row.identityPublicKey, &row.status))
			}
		})
		return row
	}
	identity := readIdentity()
	if identity == nil {
		if record.Generation != 1 && !sidecarReregistrationAdmits(ctx, record.NetworkId, record.SidecarId, record.IdentityPublicKey) {
			return SidecarBindGenerationConflict, 0
		}
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO sidecar_identity (
					sidecar_id,
					network_id,
					identity_public_key,
					identity_alg,
					status,
					create_time
				)
				VALUES ($1, $2, $3, $4, $5, $6)
				ON CONFLICT DO NOTHING
			`,
			record.SidecarId,
			record.NetworkId,
			record.IdentityPublicKey,
			SidecarIdentityAlgEd25519,
			SidecarIdentityStatusActive,
			now,
		))
		if tag.RowsAffected() == 0 {
			// a concurrent first bind created it: take the row it committed,
			// and the rules for a later generation apply
			identity = readIdentity()
			if identity == nil {
				return SidecarBindGenerationConflict, 0
			}
		}
	}
	if identity != nil {
		if identity.status == SidecarIdentityStatusRevoked {
			return SidecarBindSidecarRevoked, 0
		}
		if identity.networkId != record.NetworkId || !bytes.Equal(identity.identityPublicKey, record.IdentityPublicKey) {
			return SidecarBindGenerationConflict, 0
		}
		var newest int64
		server.Raise(tx.QueryRow(
			ctx,
			`
				SELECT COALESCE(MAX(generation), 0)
				FROM sidecar_identity_binding
				WHERE sidecar_id = $1
			`,
			record.SidecarId,
		).Scan(&newest))
		if record.Generation != newest+1 {
			return SidecarBindGenerationConflict, 0
		}
	}

	tag := server.RaisePgResult(tx.Exec(
		ctx,
		`
			INSERT INTO sidecar_identity_binding (
				sidecar_id,
				generation,
				client_id,
				client_public_key,
				signatures,
				create_time
			)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT DO NOTHING
		`,
		record.SidecarId,
		record.Generation,
		record.ClientId,
		record.ClientPublicKey,
		record.SignedBinding,
		now,
	))
	if tag.RowsAffected() == 0 {
		// the client bound under another sidecar_id meanwhile, or this
		// generation was taken
		if outcome, storedGeneration, found := clientBinding(); found {
			return outcome, storedGeneration
		}
		return SidecarBindGenerationConflict, 0
	}
	return SidecarBindAppended, record.Generation
}

// What GET /sidecar/identity answers about one sidecar (§5.15), never its
// user_id.
type SidecarIdentityView struct {
	SidecarId         server.Id
	NetworkId         server.Id
	IdentityPublicKey []byte
	IdentityAlg       string
	Status            string
	// The current binding's bytes: the newest generation's, or, from a client
	// lookup, that client's.
	Binding []byte
	// A page of the bindings, newest first (plan-CORE-client-to-sidecar-lookup).
	Bindings [][]byte
}

// The identity of sidecarId with its newest binding and a page of bindings
// below beforeGeneration, newest first (0: from the newest), or nil when api
// holds none for callerNetworkId. Only the caller's own network is served
// until CORE-M6-04 adds §8.11's shares.
func GetSidecarIdentity(ctx context.Context, callerNetworkId server.Id, sidecarId server.Id, beforeGeneration int64) *SidecarIdentityView {
	var view *SidecarIdentityView
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT network_id, identity_public_key, identity_alg, status
				FROM sidecar_identity
				WHERE sidecar_id = $1
			`,
			sidecarId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				view = &SidecarIdentityView{SidecarId: sidecarId, Bindings: [][]byte{}}
				server.Raise(result.Scan(&view.NetworkId, &view.IdentityPublicKey, &view.IdentityAlg, &view.Status))
			}
		})
		if view == nil || view.NetworkId != callerNetworkId {
			view = nil
			return
		}
		result, err = conn.Query(
			ctx,
			`
				SELECT signatures
				FROM sidecar_identity_binding
				WHERE sidecar_id = $1
				ORDER BY generation DESC
				LIMIT 1
			`,
			sidecarId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&view.Binding))
			}
		})
		before := beforeGeneration
		if before <= 0 {
			before = 1 << 62
		}
		result, err = conn.Query(
			ctx,
			`
				SELECT signatures
				FROM sidecar_identity_binding
				WHERE sidecar_id = $1 AND generation < $2
				ORDER BY generation DESC
				LIMIT $3
			`,
			sidecarId,
			before,
			SidecarIdentityBindingPageCount,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var binding []byte
				server.Raise(result.Scan(&binding))
				view.Bindings = append(view.Bindings, binding)
			}
		})
	})
	return view
}

// The identity a client of any generation is bound to, with that client's
// binding, or nil when it has none in callerNetworkId
// (plan-CORE-client-to-sidecar-lookup).
func GetSidecarIdentityByClient(ctx context.Context, callerNetworkId server.Id, clientId server.Id) *SidecarIdentityView {
	var view *SidecarIdentityView
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
					sidecar_identity.sidecar_id,
					sidecar_identity.network_id,
					sidecar_identity.identity_public_key,
					sidecar_identity.identity_alg,
					sidecar_identity.status,
					sidecar_identity_binding.signatures
				FROM sidecar_identity_binding
				INNER JOIN sidecar_identity ON sidecar_identity.sidecar_id = sidecar_identity_binding.sidecar_id
				WHERE sidecar_identity_binding.client_id = $1
			`,
			clientId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				view = &SidecarIdentityView{}
				server.Raise(result.Scan(
					&view.SidecarId,
					&view.NetworkId,
					&view.IdentityPublicKey,
					&view.IdentityAlg,
					&view.Status,
					&view.Binding,
				))
			}
		})
	})
	if view == nil || view.NetworkId != callerNetworkId {
		return nil
	}
	return view
}

// The client has no binding §7.4's proof could use: none, a revoked identity,
// or one its sidecar has since replaced by a newer generation.
var ErrClientBindingKeyNotFound = errors.New("Client has no current binding.")

// The key §7.4's DPoP proof of a strict_exp bearer is checked against: the
// ClientKey the client's binding names, only while that binding is its
// sidecar's newest generation and the identity is not revoked, so an older
// generation's key is refused rather than taken as a key of the client
// (TEAM-M1-02). ErrClientBindingKeyNotFound otherwise, and always while MG1 is
// not linked.
func GetClientBindingKey(ctx context.Context, clientId server.Id) ([]byte, error) {
	if !server.SidecarMg1Linked() {
		return nil, ErrClientBindingKeyNotFound
	}
	var clientPublicKey []byte
	found := false
	// a failed read refuses the proof, as every lookup of the P0 fix does
	lookupErr := server.HandleError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`
				SELECT sidecar_identity_binding.client_public_key
				FROM sidecar_identity_binding
				INNER JOIN sidecar_identity ON sidecar_identity.sidecar_id = sidecar_identity_binding.sidecar_id
				WHERE
					sidecar_identity_binding.client_id = $1 AND
					sidecar_identity.status = $2 AND
					sidecar_identity_binding.generation = (
						SELECT MAX(newest.generation)
						FROM sidecar_identity_binding newest
						WHERE newest.sidecar_id = sidecar_identity_binding.sidecar_id
					)
			`,
				clientId,
				SidecarIdentityStatusActive,
			)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&clientPublicKey))
					found = true
				}
			})
		})
	})
	if lookupErr != nil || !found {
		return nil, ErrClientBindingKeyNotFound
	}
	return clientPublicKey, nil
}

// Marks sidecarId's identity revoked at revokeTime, once: what api does when a
// SidecarRevoke that verifies against the log is sequenced (TEAM-M2-16). Every
// later bind of it answers sidecar_revoked, and its clients' proofs stop. A
// revoked identity is never active again. Reports whether this call revoked
// it.
func SetSidecarIdentityRevoked(ctx context.Context, sidecarId server.Id, revokeTime time.Time) (revoked bool) {
	server.Tx(ctx, func(tx server.PgTx) {
		revoked = SetSidecarIdentityRevokedInTx(ctx, tx, sidecarId, revokeTime)
	})
	return
}

// SetSidecarIdentityRevoked in the caller's transaction.
func SetSidecarIdentityRevokedInTx(ctx context.Context, tx server.PgTx, sidecarId server.Id, revokeTime time.Time) bool {
	revoked, err := setSidecarIdentityRevokedInTx(ctx, tx, sidecarId, revokeTime)
	server.Raise(err)
	return revoked
}

// SetSidecarIdentityRevokedInTx, answering the statement's error.
func setSidecarIdentityRevokedInTx(ctx context.Context, tx server.PgTx, sidecarId server.Id, revokeTime time.Time) (bool, error) {
	tag, err := tx.Exec(
		ctx,
		`
			UPDATE sidecar_identity
			SET status = $2, revoke_time = $3
			WHERE sidecar_id = $1 AND status = $4
		`,
		sidecarId,
		SidecarIdentityStatusRevoked,
		revokeTime,
		SidecarIdentityStatusActive,
	)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// The name the identity's removal effect registers under.
const RemovalEffectSidecarIdentity = "sidecar_identity"

func init() {
	RegisterRemovalEffect(RemovalEffectSidecarIdentity, applySidecarIdentityRemovalEffectInTx)
}

// Marks each sidecar a removal revokes revoked at the removal's time: the step
// TEAM-M2-16's effects run when a SidecarRevoke that verifies against the log
// lands (CORE-M2-07), after which its binds answer sidecar_revoked and its
// clients' proofs stop. Idempotent, as every effect is: a revoked identity
// stays as it was. Nothing while MG1 is not linked.
func applySidecarIdentityRemovalEffectInTx(ctx context.Context, tx server.PgTx, removal *Removal) error {
	if !server.SidecarMg1Linked() {
		return nil
	}
	if removal.Time.IsZero() {
		return errors.New("a removal with no time")
	}
	for _, subject := range removal.SubjectsOf(sidecar.RevocationKindSidecar) {
		if _, err := setSidecarIdentityRevokedInTx(ctx, tx, subject.SubjectId, removal.Time); err != nil {
			return err
		}
	}
	return nil
}

// Removes every identity of networkId and its bindings, in the network's
// deletion transaction: the one way these rows go (§5.15).
func removeSidecarIdentitiesInTx(ctx context.Context, tx server.PgTx, networkId server.Id) {
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			DELETE FROM sidecar_identity_binding
			USING sidecar_identity
			WHERE
				sidecar_identity_binding.sidecar_id = sidecar_identity.sidecar_id AND
				sidecar_identity.network_id = $1
		`,
		networkId,
	))
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			DELETE FROM sidecar_identity
			WHERE network_id = $1
		`,
		networkId,
	))
}

// A registration a sidecar makes at api beside its identity and binding, which
// a Postgres restore can lose: §13's TLS endpoints, §15's mail host keys and
// §18's push token (§5.6, §19.10). present answers whether api still holds the
// client's; an error refreshes as usual and logs, since a failed read proves
// no loss.
type SidecarRegistrationCheck func(ctx context.Context, networkId server.Id, clientId server.Id) (present bool, err error)

var sidecarRegistrationChecksLock sync.Mutex
var sidecarRegistrationChecks = map[string]SidecarRegistrationCheck{}

// Adds the registration name to what restore_required asks about, the one hook
// each area uses rather than growing the refresh path (§5.6). A later check of
// the same name replaces the earlier.
func AddSidecarRegistrationCheck(name string, check SidecarRegistrationCheck) {
	sidecarRegistrationChecksLock.Lock()
	defer sidecarRegistrationChecksLock.Unlock()
	sidecarRegistrationChecks[name] = check
}

// Removes a registration check, for a test.
func Testing_RemoveSidecarRegistrationCheck(name string) {
	sidecarRegistrationChecksLock.Lock()
	defer sidecarRegistrationChecksLock.Unlock()
	delete(sidecarRegistrationChecks, name)
}

// Whether a strict_exp client's refresh answers restore_required (§5.6,
// §19.10): api lacks the client's binding, its identity, or a registration an
// area's check misses, as after a Postgres restore from an older dump. It is
// no refusal: the sidecar posts its newest binding again, re-registers, and
// refreshes again. Always false while MG1 is not linked, when there is nothing
// to read.
func SidecarRestoreRequired(ctx context.Context, networkId server.Id, clientId server.Id) bool {
	if !server.SidecarMg1Linked() {
		return false
	}
	present := false
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`
				SELECT EXISTS (
					SELECT 1
					FROM sidecar_identity_binding
					INNER JOIN sidecar_identity ON sidecar_identity.sidecar_id = sidecar_identity_binding.sidecar_id
					WHERE
						sidecar_identity_binding.client_id = $1 AND
						sidecar_identity.network_id = $2
				)
			`,
			clientId,
			networkId,
		).Scan(&present))
	})
	if !present {
		return true
	}
	checks := func() map[string]SidecarRegistrationCheck {
		sidecarRegistrationChecksLock.Lock()
		defer sidecarRegistrationChecksLock.Unlock()
		copied := map[string]SidecarRegistrationCheck{}
		for name, check := range sidecarRegistrationChecks {
			copied[name] = check
		}
		return copied
	}()
	for name, check := range checks {
		registered, err := check(ctx, networkId, clientId)
		if err != nil {
			glog.Warningf("[sidecar][%s]registration check %s failed for client %s: %s\n", networkId, name, clientId, err)
			continue
		}
		if !registered {
			return true
		}
	}
	return false
}
