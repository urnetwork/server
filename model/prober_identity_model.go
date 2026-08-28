package model

import (
	"context"
	"fmt"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/session"
)

// The egress prober needs a network client jwt to connect through providers at
// all. Producing one used to be a manual operator step -- create a network,
// call /network/auth-client, paste the token into an env file -- which meant a
// deployment nobody had done that for simply never probed egress, quietly.
// This file is the server-side replacement: one persisted identity, created
// once and refreshed on a schedule, by the bootstrap task in
// taskworker/work/prober_bootstrap_work.go.
//
// The whole risk here is that the task re-runs every six hours forever. Every
// constant and every query below exists to make a repeated run a no-op rather
// than a second account, a second client, or another balance grant.
const (
	// MaxProberBootstrapAttempts bounds how many times account creation is
	// attempted before the task gives up and only logs.
	//
	// This bound is the single most important thing in this file. Creation is
	// the one irreversible, side-effect-carrying step: a retry loop that kept
	// creating networks would fill the deployment with orphan accounts, and it
	// would do so four times a day forever with nobody watching. Failing loudly
	// and STOPPING is strictly better than that -- an operator can read one
	// error line, but nobody unwinds a thousand accounts.
	MaxProberBootstrapAttempts = 5

	// ProberMinTransferBalance is the active balance below which the prober's
	// network is topped up. At or above it nothing is granted -- that check is
	// what keeps a six-hourly task from stacking a grant every six hours.
	ProberMinTransferBalance = 4 * Gib

	// ProberTransferBalanceTopUp / ProberTransferBalanceDuration are one grant.
	// Deliberately generous relative to what probing costs (a tunnel handshake
	// and a few small https requests per provider): the failure this feature
	// exists to remove is silent, so erring toward "never runs dry" is cheap
	// and erring the other way is invisible.
	ProberTransferBalanceTopUp    = 32 * Gib
	ProberTransferBalanceDuration = 30 * 24 * time.Hour

	// ProberJwtRefreshAge is how old a minted client jwt may get before it is
	// re-minted.
	//
	// It is deliberately NOT derived from the jwt's own lifetime. That lifetime
	// (jwt.expiryDuration) is unexported, so this package cannot read it, and it
	// has already been changed once. A duplicated copy here would drift
	// silently, and the direction it drifts is the bad one: a shortened lifetime
	// with a stale copy here means an EXPIRED prober credential. A short,
	// self-chosen refresh age needs to know nothing about the deadline it is
	// staying clear of.
	ProberJwtRefreshAge = 7 * 24 * time.Hour

	// The prober's client is a long-lived server-managed identity, not a user
	// device. The description is what an operator sees in the device list, so
	// it says what the client is.
	ProberClientDescription = "URnetwork egress prober"
	ProberClientDeviceSpec  = "urnetwork/egress-prober"
)

// ProberIdentity is the persisted singleton row. Every field except
// CreateAttempts is empty until the step that fills it has committed, and the
// bootstrap decides what to do next purely from which of them are still empty
// -- so an interrupted run resumes at the right step instead of starting over.
type ProberIdentity struct {
	NetworkId   *server.Id `json:"network_id,omitempty"`
	UserId      *server.Id `json:"user_id,omitempty"`
	NetworkName string     `json:"network_name,omitempty"`

	ClientId *server.Id `json:"client_id,omitempty"`
	// the minted credential itself. Nothing in this repository consumes it yet
	// -- delivering it to the prober process is a separate step -- but
	// re-minting a token that was then discarded would be pointless, so it is
	// stored.
	ByClientJwt string `json:"by_client_jwt,omitempty"`

	CreateAttempts int        `json:"create_attempts"`
	CreateTime     *time.Time `json:"create_time,omitempty"`
	LastMintTime   *time.Time `json:"last_mint_time,omitempty"`
}

// HasNetwork reports whether the account exists. This row is the ONLY authority
// on that question; see the migration comment for why a name lookup cannot be.
func (self *ProberIdentity) HasNetwork() bool {
	return self != nil && self.NetworkId != nil
}

func GetProberIdentity(ctx context.Context) *ProberIdentity {
	var identity *ProberIdentity

	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
					network_id,
					user_id,
					network_name,
					client_id,
					by_client_jwt,
					create_attempts,
					create_time,
					last_mint_time
				FROM prober_identity
				WHERE singleton
			`,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				identity = &ProberIdentity{}
				var networkName *string
				var byClientJwt *string
				server.Raise(result.Scan(
					&identity.NetworkId,
					&identity.UserId,
					&networkName,
					&identity.ClientId,
					&byClientJwt,
					&identity.CreateAttempts,
					&identity.CreateTime,
					&identity.LastMintTime,
				))
				if networkName != nil {
					identity.NetworkName = *networkName
				}
				if byClientJwt != nil {
					identity.ByClientJwt = *byClientJwt
				}
			}
		})
	})

	return identity
}

// claimProberIdentityCreate takes the exclusive right to create the prober's
// account, and is the reason this task can run forever without ever creating a
// second one.
//
// The two outcomes, which the caller depends on exactly:
//
//   - claimed == true: the row exists with network_id still NULL and this call
//     owns the attempt. createAttempts is how many attempts came BEFORE this one
//     (0 on the very first), so the caller can refuse past a bound.
//   - claimed == false: no row came back, which happens only when the
//     ON CONFLICT branch's `WHERE network_id IS NULL` was false -- the account
//     already exists. Nothing to create. This is the steady state, reached every
//     six hours forever after the first successful run.
//
// The claim commits BEFORE the account is created, and it must. Claim-then-
// create means a crash in between costs one attempt off a bounded counter;
// create-then-claim (or a claim inside a transaction that the create's failure
// rolls back) means a crash leaves an account nothing remembers -- and the next
// run creates another one, forever.
func claimProberIdentityCreate(ctx context.Context) (createAttempts int, claimed bool) {
	server.Tx(ctx, func(tx server.PgTx) {
		result, err := tx.Query(
			ctx,
			`
				INSERT INTO prober_identity (singleton, create_attempts)
				VALUES (true, 0)
				ON CONFLICT (singleton) DO UPDATE
				SET create_attempts = prober_identity.create_attempts + 1
				WHERE prober_identity.network_id IS NULL
				RETURNING create_attempts
			`,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&createAttempts))
				claimed = true
			}
		})
	})

	return
}

// setProberIdentityNetwork records the created account. `network_id IS NULL` in
// the WHERE makes the write single-assignment: the identity can be filled in
// once and never repointed, so a second run that somehow got past the claim
// cannot overwrite the live identity with its own network. A false return
// therefore means "someone else already won", which the caller reports as an
// orphan rather than treating as success.
func setProberIdentityNetwork(
	ctx context.Context,
	networkId server.Id,
	userId server.Id,
	networkName string,
) (stored bool) {
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE prober_identity
				SET
					network_id = $1,
					user_id = $2,
					network_name = $3,
					create_time = $4
				WHERE singleton AND network_id IS NULL
			`,
			networkId,
			userId,
			networkName,
			server.NowUtc(),
		))
		stored = 0 < tag.RowsAffected()
	})

	return
}

// setProberIdentityClient stores a freshly minted credential. client_id is
// written every time but never changes after the first mint -- the caller
// passes the stored id back in, so the prober keeps ONE client identity across
// refreshes instead of accumulating a new client per refresh forever.
func setProberIdentityClient(
	ctx context.Context,
	clientId server.Id,
	byClientJwt string,
	mintTime time.Time,
) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE prober_identity
				SET
					client_id = $1,
					by_client_jwt = $2,
					last_mint_time = $3
				WHERE singleton
			`,
			clientId,
			byClientJwt,
			mintTime,
		))
	})
}

// clearProberIdentityClient forgets the stored client, so the next mint
// provisions a fresh one. The network identity -- the part that must never be
// duplicated -- is deliberately untouched; only the client is replaced, and
// clients are re-provisionable by design.
//
// by_client_jwt is dropped with it. A credential naming a client that no longer
// exists is refused at auth anyway (see jwt.ValidateByJwtState), so keeping it
// would only make a dead token look like a live one.
func clearProberIdentityClient(ctx context.Context) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE prober_identity
				SET
					client_id = NULL,
					by_client_jwt = NULL,
					last_mint_time = NULL
				WHERE singleton
			`,
		))
	})
}

// getNetworkAdminUserId reads back the user the network was created for.
//
// NetworkCreate's result carries the network id but NOT the user id, and the
// user id is needed for every later re-mint (jwt.NewByJwt takes it), long after
// the create call is gone. Reading it from the row that was just written keeps
// the stored identity consistent with the database by construction.
func getNetworkAdminUserId(ctx context.Context, networkId server.Id) (userId server.Id, found bool) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT admin_user_id
				FROM network
				WHERE network_id = $1
			`,
			networkId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&userId))
				found = true
			}
		})
	})

	return
}

// ProberBootstrapStatus reports what one bootstrap pass actually did. It exists
// for logging and for tests; the task's own result stays empty.
type ProberBootstrapStatus struct {
	NetworkCreated  bool `json:"network_created"`
	BalanceGranted  bool `json:"balance_granted"`
	ClientJwtMinted bool `json:"client_jwt_minted"`
	// set when creation was refused because MaxProberBootstrapAttempts is spent
	CreateExhausted bool `json:"create_exhausted"`
}

// BootstrapProberIdentity brings the prober's credential up to date: it creates
// the network account if there is none, tops the balance up if it has run low,
// and mints a client jwt if there is none or the last one is getting old.
//
// Each of those three is separately conditional, because this runs every six
// hours forever. In the steady state -- account present, balance healthy, jwt
// fresh -- a pass performs no writes at all.
//
// clientSession is the taskworker's UNAUTHENTICATED session (ByJwt == nil). It
// is passed through to NetworkCreate and never read for identity; the session
// used to auth the client is a separate one built here from the stored row.
func BootstrapProberIdentity(clientSession *session.ClientSession) (*ProberBootstrapStatus, error) {
	ctx := clientSession.Ctx
	status := &ProberBootstrapStatus{}

	identity := GetProberIdentity(ctx)

	if !identity.HasNetwork() {
		created, err := createProberNetwork(clientSession, status)
		if err != nil {
			return status, err
		}
		if !created {
			// either the attempt bound is spent, the account already exists but
			// the row was written by a concurrent pass, or the create failed --
			// all of them already logged. Nothing below can run without the
			// identity, and the next pass picks it up.
			return status, nil
		}
		identity = GetProberIdentity(ctx)
		if !identity.HasNetwork() {
			return status, fmt.Errorf("prober identity has no network after a successful create")
		}
	}

	// Balance: only when it is actually low. GetActiveTransferBalanceByteCount
	// sums the live balances, so a grant from an earlier pass -- or the ordinary
	// daily free grant, which this network receives like any other -- suppresses
	// the next one. Granting unconditionally here would write a new
	// transfer_balance row four times a day forever.
	if GetActiveTransferBalanceByteCount(ctx, *identity.NetworkId) < ProberMinTransferBalance {
		startTime := server.NowUtc()
		err := AddBasicTransferBalance(
			ctx,
			*identity.NetworkId,
			ProberTransferBalanceTopUp,
			startTime,
			startTime.Add(ProberTransferBalanceDuration),
		)
		if err != nil {
			// not fatal to the pass: an existing credential keeps working and
			// the next pass tries again
			glog.Errorf("[proberboot]could not add transfer balance: %s\n", err)
		} else {
			status.BalanceGranted = true
			glog.Infof(
				"[proberboot]granted %s transfer balance to %s\n",
				ByteCountHumanReadable(ProberTransferBalanceTopUp),
				identity.NetworkName,
			)
		}
	}

	if proberJwtNeedsMint(identity, server.NowUtc()) {
		if err := mintProberClientJwt(ctx, identity, status); err != nil {
			return status, err
		}
	}

	return status, nil
}

// proberJwtNeedsMint is the "near expiry" test, expressed as an age rather than
// as a distance from a deadline this package cannot see (see
// ProberJwtRefreshAge). A missing credential always needs one.
func proberJwtNeedsMint(identity *ProberIdentity, now time.Time) bool {
	if identity.ClientId == nil || identity.ByClientJwt == "" || identity.LastMintTime == nil {
		return true
	}
	return ProberJwtRefreshAge <= now.Sub(*identity.LastMintTime)
}

// createProberNetwork creates the account, once, under the claim.
func createProberNetwork(
	clientSession *session.ClientSession,
	status *ProberBootstrapStatus,
) (created bool, returnErr error) {
	ctx := clientSession.Ctx

	createAttempts, claimed := claimProberIdentityCreate(ctx)
	if !claimed {
		// the account already exists -- the row's network_id is set. This is the
		// normal outcome of every pass after the first.
		return false, nil
	}

	if MaxProberBootstrapAttempts <= createAttempts {
		// STOP. Do not create. The task stays scheduled and keeps saying this
		// every six hours, which is the point: it is visible, and it is not
		// creating accounts while it waits to be looked at.
		status.CreateExhausted = true
		glog.Errorf(
			"[proberboot]giving up on creating the prober network after %d attempts; "+
				"no account will be created until the prober_identity row is cleared by hand\n",
			createAttempts,
		)
		return false, nil
	}

	// The seedphrase path is the only one that needs no human: no email to
	// verify, no wallet to sign with, no SSO. Terms must be set or it refuses.
	// UserName/NetworkName are not passed because that branch ignores both -- it
	// names the network with generateRandomNetworkName() regardless.
	//
	// clientSession here is the task's own unauthenticated session, which is
	// what NetworkCreate wants for a create: it reads no identity from it, only
	// the address, for its per-ip rate limit.
	result, err := NetworkCreate(
		NetworkCreateArgs{
			Terms: true,
		},
		clientSession,
	)
	if err != nil {
		glog.Errorf("[proberboot]network create failed (attempt %d): %s\n", createAttempts+1, err)
		return false, err
	}
	if result.Error != nil {
		glog.Errorf(
			"[proberboot]network create refused (attempt %d): %s\n",
			createAttempts+1,
			result.Error.Message,
		)
		return false, nil
	}
	if result.Network == nil {
		glog.Errorf("[proberboot]network create returned no network (attempt %d)\n", createAttempts+1)
		return false, nil
	}

	networkId := result.Network.NetworkId
	userId, found := getNetworkAdminUserId(ctx, networkId)
	if !found {
		glog.Errorf("[proberboot]created network %s has no admin user\n", networkId)
		return false, nil
	}

	// result.Seedphrase (model/network_model.go:95, populated at :197) is
	// DISCARDED here, on purpose. Only the network id, the admin user id and the
	// name are kept, and prober_identity has no column that could hold a phrase
	// (db_migrations.go:6455). Do not "fix" that by adding one.
	//
	// Nothing in this system needs it. This server holds the jwt signing keys
	// (jwt/by_jwt.go:68, byPrivateKeys), so mintProberClientJwt below re-mints
	// this account's client credential from the stored network_id/user_id
	// whenever it likes -- the only identity jwt.NewByJwt takes is those three
	// stored fields; its other two arguments are flags (jwt/by_jwt.go:217-223).
	// A seedphrase is a HUMAN login credential, and no human ever logs into a
	// machine-operated identity.
	//
	// Persisting it would therefore write a root credential into postgres --
	// recoverable from any dump, backup or replica, forever -- to enable a login
	// nobody performs. Note what that would undo: the platform deliberately keeps
	// only a salted hash of a seedphrase, never the phrase
	// (model/seedphrase_auth_model.go:44-45, model/auth_model_identity.go:136).
	// Writing the phrase into prober_identity would make this account the
	// exception to that, and the one worth stealing.
	//
	// The honest cost, stated plainly: this makes the account unrecoverable by a
	// human, by design. No person holds a login credential for it and none is
	// written down anywhere. The prober account this one replaced was lost in
	// exactly that way -- a seedphrase-only account whose phrase nobody recorded,
	// and a seedphrase has no reset path. Two things make it acceptable here and
	// only here.
	//
	// It is not a dead end. This account is created down the seedphrase branch, so
	// it HAS seedphrase auth (CreateSeedphraseAuthInTx, model/network_model.go:893)
	// -- and the stored by_client_jwt authenticates as the account, which is enough
	// to call /auth/regenerate-seedphrase and mint a fresh phrase on demand (see
	// the note on api/handlers.ProberCredentialResult for that chain). If the jwt
	// has expired, mintProberClientJwt makes another. So even the human-login case
	// does not want a stored phrase: the login can be manufactured from what is
	// already here, which is the last argument against persisting one.
	//
	// And it is re-creatable. DELETE this row -- not merely NULL its network_id,
	// which takes claimProberIdentityCreate's DO UPDATE branch and carries
	// create_attempts forward -- and the next pass claims a fresh row at
	// create_attempts = 0 and builds a new account. The worst case is an orphaned
	// network to clean up, not an outage nobody can undo.
	if !setProberIdentityNetwork(ctx, networkId, userId, result.Network.NetworkName) {
		// Another run recorded an identity first, so this network is an orphan.
		// It is named here because an account nobody knows about is exactly what
		// this table exists to prevent, and silence would leave it
		// undiscoverable.
		glog.Errorf(
			"[proberboot]prober identity was already claimed; network %s (%s) is ORPHANED and should be removed\n",
			networkId,
			result.Network.NetworkName,
		)
		return false, nil
	}

	status.NetworkCreated = true
	glog.Infof("[proberboot]created prober network %s (%s)\n", networkId, result.Network.NetworkName)
	return true, nil
}

// mintProberClientJwt mints (or re-mints) the prober's client credential.
//
// The session it builds is the only place a ByJwt is involved, and it is built
// from the STORED identity rather than from anything the task was handed -- the
// task's session is unauthenticated by construction.
func mintProberClientJwt(
	ctx context.Context,
	identity *ProberIdentity,
	status *ProberBootstrapStatus,
) error {
	// pro is re-derived from the source of truth inside AuthNetworkClient, so
	// the value carried here never reaches the minted credential.
	byJwt := jwt.NewByJwt(
		*identity.NetworkId,
		*identity.UserId,
		identity.NetworkName,
		false,
		false,
	)

	proberSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", byJwt)
	defer proberSession.Cancel()

	// identity.ClientId is nil only on the first mint. Every later mint passes
	// the stored id, which re-auths that same client and returns a fresh
	// by_client_jwt for it -- one durable prober identity, not one per refresh.
	result, err := authProberClient(proberSession, identity.ClientId)
	if err != nil {
		return err
	}

	if result.Error != nil && identity.ClientId != nil {
		// The stored client cannot be re-authed. AuthNetworkClient's re-auth
		// branch fails for exactly one reason that can reach here -- the client
		// or its device is gone or inactive ("Client does not exist.", "Client
		// needs to be migrated", "Device does not exist.") -- since the only
		// other error it returns is for roles/principal, which this caller never
		// sends. So any error on this path means the stored client is unusable,
		// and no message parsing is needed to know it.
		//
		// Without this, a client removed by any of the sweepers would strand the
		// refresh permanently: every later pass would read the same dead id and
		// fail identically, forever, which is precisely the silent-stop this
		// feature exists to remove. Forget the client and provision another
		// against the same network.
		glog.Errorf(
			"[proberboot]stored prober client %s could not be re-authed (%s); provisioning a new client\n",
			identity.ClientId,
			result.Error.Message,
		)
		clearProberIdentityClient(ctx)

		result, err = authProberClient(proberSession, nil)
		if err != nil {
			return err
		}
	}

	if result.Error != nil {
		return fmt.Errorf("could not auth the prober client: %s", result.Error.Message)
	}
	if result.ByClientJwt == nil || result.ClientId == nil {
		return fmt.Errorf("prober client auth returned no client credential")
	}

	setProberIdentityClient(ctx, *result.ClientId, *result.ByClientJwt, server.NowUtc())

	status.ClientJwtMinted = true
	// the jwt itself is never logged; it is the credential
	glog.Infof("[proberboot]minted a client jwt for prober client %s\n", *result.ClientId)
	return nil
}

// authProberClient mints one credential: a new client when clientId is nil, a
// fresh jwt for that same client when it is not.
//
// No roles and no principal are passed, on either path. validateClientIdentityArgs
// applies its network-session gate only when one of them is set, and the re-auth
// branch rejects them outright, so leaving both empty is what lets the first mint
// and every later re-mint take the same path. The client is labelled by its
// description instead.
func authProberClient(
	proberSession *session.ClientSession,
	clientId *server.Id,
) (*AuthNetworkClientResult, error) {
	return AuthNetworkClient(
		&AuthNetworkClientArgs{
			ClientId:    clientId,
			Description: ProberClientDescription,
			DeviceSpec:  ProberClientDeviceSpec,
		},
		proberSession,
	)
}
