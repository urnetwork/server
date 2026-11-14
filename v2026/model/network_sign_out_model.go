// bringyourctl network sign-out-all signs a network out of every device (owner
// decision 2026-10-29, support inbox 7881). The account, its sign-in methods
// and its data stay; only the network's credentials end, and the next sign-in
// works as before.
//
// The commit runs what the server already runs to end an account's
// credentials, in one transaction under the network's exclusive session
// lifecycle lock:
//   - the credential rotation of a password reset (AuthPasswordSet): it
//     journals a reset session retirement, retires the network's pending auth
//     codes, raises the credential cutoff in every Redis revocation record of
//     the network (session.RotateCredentialRecordsInTx) and sets the user's
//     credential_change_time. Every API and connect token created before the
//     cutoff is refused at once, by the revocation record readers on every
//     instance and by the SQL state query alike.
//   - with --remove-clients, the deactivation behind /network/remove-clients
//     (deactivateNetworkClientsInTx): each client's revocation record
//     tombstone, its provide keys, and the close of its connections and
//     contracts.
//
// After the commit it enforces the journaled retirement as the session
// maintenance task does (RecoverSessionOperations): every tracked session
// created before the cutoff gets its revocation marker, and the clients bound
// to those sessions are deactivated. When that fails, the maintenance task
// finishes it; the rotation already refuses those sessions' tokens.
//
// The dry run only reads. Its digest binds the network, its user, the user's
// credential epoch and sign-in methods, the flag, every top-level client (the
// devices), and every live session and pending auth code it printed. A commit
// recomputes that census under the lifecycle lock and refuses any other
// digest. The journal's
// operation id derives from the digest, so committing the same digest again
// finds its operation and changes nothing: it never signs out the sign-ins made
// after the first commit. The user's credential_change_time covers every
// network the user administers, so a user who administers another network is
// refused.
package model

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

const networkSignOutDigestVersion = "urnetwork network sign-out-all v1"

// --remove-clients deactivates at most this many clients, in its one
// transaction: the bound of the synchronous /network/remove-clients path.
const NetworkSignOutMaxRemovedClients = RemoveNetworkClientsBatchCount

// After the commit, the session cleanup runs at most this many batches of
// sessionCleanupBatchSize clients; the maintenance task finishes any rest.
const networkSignOutCleanupBatchLimit = 100

var ErrNetworkSignOutNotFound = errors.New("network sign-out-all: no such network, or it has no admin user")

var ErrNetworkSignOutRefused = errors.New("network sign-out-all: refused; nothing was committed")

var ErrNetworkSignOutDigestMismatch = errors.New("network sign-out-all: --expect does not match the digest of the network as it is now; nothing was committed; run the dry run again")

// A way the user signs in. Its secrets (password hash, SSO token, seedphrase)
// are never read.
type NetworkSignOutSignIn struct {
	// password, sso, wallet or seedphrase
	Kind string `json:"kind"`
	// the password auth type, the SSO provider or the blockchain
	Type string `json:"type,omitempty"`
	// the email or phone, or the wallet address
	Identifier string `json:"identifier,omitempty"`
	// for a password sign-in
	Verified bool `json:"verified,omitempty"`
}

func (self *NetworkSignOutSignIn) String() string {
	parts := []string{self.Kind}
	if self.Type != "" {
		parts = append(parts, self.Type)
	}
	if self.Identifier != "" {
		parts = append(parts, self.Identifier)
	}
	if self.Kind == "password" {
		if self.Verified {
			parts = append(parts, "(verified)")
		} else {
			parts = append(parts, "(unverified)")
		}
	}
	return strings.Join(parts, " ")
}

// An active client of the network.
type NetworkSignOutClient struct {
	ClientId server.Id `json:"client_id"`
	// nil for a top-level client: a device's own sign-in
	SourceClientId *server.Id `json:"source_client_id,omitempty"`
	Description    string     `json:"description,omitempty"`
	// the tracked session the client is bound to
	SessionId  *server.Id `json:"session_id,omitempty"`
	CreateTime time.Time  `json:"create_time"`
	AuthTime   time.Time  `json:"auth_time"`
}

// What a sign-out acts on, read in one snapshot.
type NetworkSignOutPlan struct {
	NetworkId   server.Id `json:"network_id"`
	NetworkName string    `json:"network_name"`
	// the network's admin user: a network has no other user
	UserId server.Id `json:"user_id"`
	// network_user.auth_type and network_user.user_auth, the account's
	// original sign-in
	UserAuthType string                  `json:"user_auth_type"`
	UserAuth     string                  `json:"user_auth"`
	SignIns      []*NetworkSignOutSignIn `json:"sign_ins"`
	// tokens created before it are already refused
	CredentialChangeTime time.Time `json:"credential_change_time"`
	// other networks the user administers; any refuses the sign-out
	OtherNetworkIds []server.Id                  `json:"other_network_ids"`
	Clients         []*NetworkSignOutClient      `json:"clients"`
	Sessions        []session.LiveNetworkSession `json:"sessions"`
	// active auth codes, by id; a code is a sign-in secret and is never read
	AuthCodeIds   []server.Id `json:"auth_code_ids"`
	RemoveClients bool        `json:"remove_clients"`
	// why a commit is refused whatever its digest; empty when it may commit
	Refusal string `json:"refusal,omitempty"`
	// empty when refused
	Digest string `json:"digest,omitempty"`
	// the journal operation a commit of this digest records
	OperationId server.Id `json:"operation_id"`
}

// The active top-level clients: the devices' own sign-ins.
func (self *NetworkSignOutPlan) TopLevelClients() []*NetworkSignOutClient {
	clients := []*NetworkSignOutClient{}
	for _, client := range self.Clients {
		if client.SourceClientId == nil {
			clients = append(clients, client)
		}
	}
	return clients
}

// The active clients that a client created.
func (self *NetworkSignOutPlan) ChildClientCount() int {
	return len(self.Clients) - len(self.TopLevelClients())
}

// The active clients bound to one of the live sessions: without
// --remove-clients, the clients the session cleanup deactivates.
func (self *NetworkSignOutPlan) SessionBoundClientCount() int {
	count := 0
	for _, client := range self.Clients {
		if client.SessionId == nil {
			continue
		}
		for _, liveSession := range self.Sessions {
			if liveSession.SessionId == *client.SessionId {
				count += 1
				break
			}
		}
	}
	return count
}

// The canonical census text the digest covers, one line each. A name or
// identifier is quoted, so no field can add a line. Of the clients it covers
// the top-level ones, the devices: a running device creates and ends child
// clients all the time (network primary of support inbox 7881 had about 10k
// active and 136k ended), so a digest over them would refuse almost every
// commit. A new device changes the digest; --remove-clients deactivates the
// children active at the commit.
func (self *NetworkSignOutPlan) digestLines() []string {
	lines := []string{
		networkSignOutDigestVersion,
		fmt.Sprintf("network %s %s", self.NetworkId, strconv.Quote(self.NetworkName)),
		fmt.Sprintf(
			"user %s %s %s credential_change_time %d",
			self.UserId,
			strconv.Quote(self.UserAuthType),
			strconv.Quote(self.UserAuth),
			self.CredentialChangeTime.UnixMicro(),
		),
	}
	for _, signIn := range self.SignIns {
		lines = append(lines, fmt.Sprintf(
			"sign_in %s %s %s %t",
			signIn.Kind,
			strconv.Quote(signIn.Type),
			strconv.Quote(signIn.Identifier),
			signIn.Verified,
		))
	}
	for _, networkId := range self.OtherNetworkIds {
		lines = append(lines, fmt.Sprintf("other_network %s", networkId))
	}
	lines = append(lines, fmt.Sprintf("remove_clients %t", self.RemoveClients))
	for _, client := range self.TopLevelClients() {
		sessionId := "-"
		if client.SessionId != nil {
			sessionId = client.SessionId.String()
		}
		lines = append(lines, fmt.Sprintf("client %s session %s", client.ClientId, sessionId))
	}
	for _, liveSession := range self.Sessions {
		lines = append(lines, fmt.Sprintf("session %s", liveSession.SessionId))
	}
	for _, authCodeId := range self.AuthCodeIds {
		lines = append(lines, fmt.Sprintf("auth_code %s", authCodeId))
	}
	return lines
}

// sha256 of the census lines, each ended by a newline, in hex.
func networkSignOutDigest(lines []string) string {
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n") + "\n"))
	return hex.EncodeToString(sum[:])
}

// The journal operation of a commit: the same network and digest name the same
// operation, so a repeated commit finds the first one.
func networkSignOutOperationId(networkId server.Id, digest string) server.Id {
	sum := sha256.Sum256([]byte(strings.Join([]string{"urnetwork/network-sign-out-all/operation/v1", networkId.String(), digest}, "\x00")))
	id := sum[:16]
	// a version 8 (custom) RFC 9562 UUID
	id[6] = (id[6] & 0x0f) | 0x80
	id[8] = (id[8] & 0x3f) | 0x80
	return server.RequireIdFromBytes(id)
}

// Reads the census in tx. A missing network or admin user is
// ErrNetworkSignOutNotFound; an unavailable session store is its error.
func planNetworkSignOutInTx(
	ctx context.Context,
	tx server.PgTx,
	networkId server.Id,
	removeClients bool,
	now time.Time,
) (*NetworkSignOutPlan, error) {
	plan := &NetworkSignOutPlan{
		NetworkId:       networkId,
		RemoveClients:   removeClients,
		SignIns:         []*NetworkSignOutSignIn{},
		OtherNetworkIds: []server.Id{},
		Clients:         []*NetworkSignOutClient{},
		AuthCodeIds:     []server.Id{},
	}

	found := false
	result, err := tx.Query(
		ctx,
		`
			SELECT
				network.network_name,
				network.admin_user_id,
				network_user.auth_type,
				network_user.user_auth,
				network_user.credential_change_time
			FROM network
			INNER JOIN network_user ON network_user.user_id = network.admin_user_id
			WHERE network.network_id = $1
		`,
		networkId,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			var userAuthType *string
			var userAuth *string
			server.Raise(result.Scan(
				&plan.NetworkName,
				&plan.UserId,
				&userAuthType,
				&userAuth,
				&plan.CredentialChangeTime,
			))
			if userAuthType != nil {
				plan.UserAuthType = *userAuthType
			}
			if userAuth != nil {
				plan.UserAuth = *userAuth
			}
			found = true
		}
	})
	if !found {
		return nil, ErrNetworkSignOutNotFound
	}
	plan.CredentialChangeTime = plan.CredentialChangeTime.UTC()

	result, err = tx.Query(
		ctx,
		`
			SELECT network_id
			FROM network
			WHERE admin_user_id = $1 AND network_id <> $2
			ORDER BY network_id
		`,
		plan.UserId,
		networkId,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var otherNetworkId server.Id
			server.Raise(result.Scan(&otherNetworkId))
			plan.OtherNetworkIds = append(plan.OtherNetworkIds, otherNetworkId)
		}
	})

	// only the identifying columns: no hash, salt, token or seedphrase
	result, err = tx.Query(
		ctx,
		`
			SELECT 'password', auth_type, user_auth, verified
			FROM network_user_auth_password
			WHERE user_id = $1

			UNION ALL

			SELECT 'sso', auth_type, user_auth, false
			FROM network_user_auth_sso
			WHERE user_id = $1

			UNION ALL

			SELECT 'wallet', blockchain, wallet_address, false
			FROM network_user_auth_wallet
			WHERE user_id = $1

			UNION ALL

			SELECT 'seedphrase', '', '', false
			FROM network_user_auth_seedphrase
			WHERE user_id = $1
		`,
		plan.UserId,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			signIn := &NetworkSignOutSignIn{}
			server.Raise(result.Scan(&signIn.Kind, &signIn.Type, &signIn.Identifier, &signIn.Verified))
			plan.SignIns = append(plan.SignIns, signIn)
		}
	})
	slices.SortFunc(plan.SignIns, func(a *NetworkSignOutSignIn, b *NetworkSignOutSignIn) int {
		if c := strings.Compare(a.Kind, b.Kind); c != 0 {
			return c
		}
		if c := strings.Compare(a.Type, b.Type); c != 0 {
			return c
		}
		return strings.Compare(a.Identifier, b.Identifier)
	})

	result, err = tx.Query(
		ctx,
		`
			SELECT
				client_id,
				source_client_id,
				COALESCE(description, ''),
				session_id,
				create_time,
				auth_time
			FROM network_client
			WHERE network_id = $1 AND active
			ORDER BY client_id
		`,
		networkId,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			client := &NetworkSignOutClient{}
			server.Raise(result.Scan(
				&client.ClientId,
				&client.SourceClientId,
				&client.Description,
				&client.SessionId,
				&client.CreateTime,
				&client.AuthTime,
			))
			client.CreateTime = client.CreateTime.UTC()
			client.AuthTime = client.AuthTime.UTC()
			plan.Clients = append(plan.Clients, client)
		}
	})

	result, err = tx.Query(
		ctx,
		`
			SELECT auth_code_id
			FROM auth_code
			WHERE network_id = $1 AND active
			ORDER BY auth_code_id
		`,
		networkId,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var authCodeId server.Id
			server.Raise(result.Scan(&authCodeId))
			plan.AuthCodeIds = append(plan.AuthCodeIds, authCodeId)
		}
	})

	sessions, err := session.LiveNetworkSessions(ctx, networkId, now)
	if err != nil {
		return nil, err
	}
	plan.Sessions = sessions

	switch {
	case 0 < len(plan.OtherNetworkIds):
		plan.Refusal = fmt.Sprintf(
			"user %s also administers %d other network(s); its credential_change_time covers every network it administers, so this would sign those out too",
			plan.UserId,
			len(plan.OtherNetworkIds),
		)
	case removeClients && NetworkSignOutMaxRemovedClients < len(plan.Clients):
		plan.Refusal = fmt.Sprintf(
			"--remove-clients deactivates at most %d clients in its one transaction, and the network has %d; sign out without it",
			NetworkSignOutMaxRemovedClients,
			len(plan.Clients),
		)
	default:
		plan.Digest = networkSignOutDigest(plan.digestLines())
		plan.OperationId = networkSignOutOperationId(networkId, plan.Digest)
	}
	return plan, nil
}

// PlanNetworkSignOut is the dry run: it reads the census in one snapshot and
// writes nothing.
func PlanNetworkSignOut(ctx context.Context, networkId server.Id, removeClients bool) (plan *NetworkSignOutPlan, returnErr error) {
	server.Tx(ctx, func(tx server.PgTx) {
		plan, returnErr = planNetworkSignOutInTx(ctx, tx, networkId, removeClients, server.NowUtc())
	})
	return
}

// What a commit did, or found done.
type NetworkSignOutResult struct {
	NetworkId server.Id `json:"network_id"`
	// the census the commit acted on, or found when it refused; nil when the
	// digest was already committed
	Plan        *NetworkSignOutPlan `json:"plan,omitempty"`
	OperationId server.Id           `json:"operation_id"`
	// the digest was committed before: nothing changed
	AlreadyApplied bool `json:"already_applied"`
	// the journaled retirement's status: prepared, enforced or complete
	Status string `json:"status"`
	// credentials created before the cutoff are refused
	Cutoff                       time.Time `json:"cutoff"`
	PreviousCredentialChangeTime time.Time `json:"previous_credential_change_time"`
	RetiredAuthCodeCount         int64     `json:"retired_auth_code_count"`
	// the clients --remove-clients deactivated
	DeactivatedClientCount int64 `json:"deactivated_client_count"`
	// the sessions the enforcement after the commit revoked
	RevokedSessionCount int `json:"revoked_session_count"`
	// the session cleanup deactivated every client bound to a revoked session
	SessionCleanupComplete bool `json:"session_cleanup_complete"`
	// why the enforcement after the commit did not finish; the maintenance
	// task finishes it
	EnforcementError error `json:"-"`
}

// The audit line of an applied commit.
func (self *NetworkSignOutResult) AuditLine() string {
	if self.AlreadyApplied || self.Plan == nil {
		return fmt.Sprintf(
			"[audit]network sign-out-all already_applied network_id=%s operation_id=%s status=%s cutoff=%s",
			self.NetworkId,
			self.OperationId,
			self.Status,
			self.Cutoff.Format(time.RFC3339Nano),
		)
	}
	return fmt.Sprintf(
		"[audit]network sign-out-all network_id=%s user_id=%s operation_id=%s cutoff=%s previous_credential_change_time=%s remove_clients=%t deactivated_clients=%d retired_auth_codes=%d live_sessions=%d digest=%s",
		self.Plan.NetworkId,
		self.Plan.UserId,
		self.OperationId,
		self.Cutoff.Format(time.RFC3339Nano),
		self.PreviousCredentialChangeTime.Format(time.RFC3339Nano),
		self.Plan.RemoveClients,
		self.DeactivatedClientCount,
		self.RetiredAuthCodeCount,
		len(self.Plan.Sessions),
		self.Plan.Digest,
	)
}

// The commit's cutoff, sampled after the exclusive lifecycle lock as the
// password reset samples it: the later of this process's clock and the
// database's, so an operator host whose clock runs behind the fleet's still
// covers every credential minted before the lock, and never earlier than the
// current credential_change_time.
func networkSignOutCutoffInTx(ctx context.Context, tx server.PgTx, credentialChangeTime time.Time) time.Time {
	cutoff := server.NowUtc()
	var databaseNow time.Time
	server.Raise(tx.QueryRow(ctx, `SELECT clock_timestamp() AT TIME ZONE 'UTC'`).Scan(&databaseNow))
	databaseNow = databaseNow.UTC()
	if cutoff.Before(databaseNow) {
		cutoff = databaseNow
	}
	if cutoff.Before(credentialChangeTime) {
		cutoff = credentialChangeTime
	}
	// the precision of a PostgreSQL timestamp and of the record's cutoff
	return cutoff.Truncate(time.Microsecond)
}

// CommitNetworkSignOut signs the network out of every device when the census
// still has the expected digest. A refusal returns the census it found.
func CommitNetworkSignOut(
	ctx context.Context,
	networkId server.Id,
	removeClients bool,
	expect string,
) (*NetworkSignOutResult, error) {
	if expect == "" {
		return nil, fmt.Errorf("%w: a commit needs the dry run's digest", ErrNetworkSignOutRefused)
	}
	operationId := networkSignOutOperationId(networkId, expect)

	var result *NetworkSignOutResult
	var refusal error
	server.Tx(ctx, func(tx server.PgTx) {
		// Tx may rerun the callback; nothing from an aborted attempt escapes
		result = nil
		refusal = nil

		// as a password reset: every mint holds the shared lock while it
		// registers, so each one admitted before this lock has committed
		server.Raise(session.LockSessionLifecycle(ctx, tx, networkId, true))

		applied, err := session.ReadJournaledRetirement(ctx, tx, networkId, operationId)
		server.Raise(err)
		if applied != nil {
			result = &NetworkSignOutResult{
				NetworkId:      networkId,
				OperationId:    operationId,
				AlreadyApplied: true,
				Status:         applied.Status,
				Cutoff:         applied.Cutoff.UTC(),
			}
			return
		}

		plan, err := planNetworkSignOutInTx(ctx, tx, networkId, removeClients, server.NowUtc())
		if errors.Is(err, ErrNetworkSignOutNotFound) {
			refusal = err
			return
		}
		server.Raise(err)
		if plan.Refusal != "" {
			result = &NetworkSignOutResult{NetworkId: networkId, Plan: plan}
			refusal = fmt.Errorf("%w: %s", ErrNetworkSignOutRefused, plan.Refusal)
			return
		}
		if plan.Digest != expect {
			result = &NetworkSignOutResult{NetworkId: networkId, Plan: plan}
			refusal = ErrNetworkSignOutDigestMismatch
			return
		}

		cutoff := networkSignOutCutoffInTx(ctx, tx, plan.CredentialChangeTime)

		// the password reset's rotation (AuthPasswordSet), for the network's user
		server.Raise(session.JournalSessionRetirementInTx(ctx, tx, networkId, operationId, "reset", cutoff))
		authCodeTag := server.RaisePgResult(tx.Exec(
			ctx,
			`UPDATE auth_code SET remaining_uses=0 WHERE network_id=$1 AND create_time<$2 AND active`,
			networkId,
			cutoff,
		))
		// every revocation record of the network refuses older credentials
		// before the rotation commits
		server.Raise(session.RotateCredentialRecordsInTx(ctx, tx, plan.UserId, cutoff))
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE network_user
				SET credential_change_time = $2
				WHERE user_id = $1
			`,
			plan.UserId,
			cutoff,
		))

		// the deactivation of /network/remove-clients
		var deactivatedClientCount int64
		if removeClients && 0 < len(plan.Clients) {
			clientIds := make([]server.Id, 0, len(plan.Clients))
			for _, client := range plan.Clients {
				clientIds = append(clientIds, client.ClientId)
			}
			deactivatedClientCount, err = deactivateNetworkClientsInTx(ctx, tx, clientIds, networkId)
			server.Raise(err)
		}

		result = &NetworkSignOutResult{
			NetworkId:                    networkId,
			Plan:                         plan,
			OperationId:                  operationId,
			Status:                       "prepared",
			Cutoff:                       cutoff,
			PreviousCredentialChangeTime: plan.CredentialChangeTime,
			RetiredAuthCodeCount:         authCodeTag.RowsAffected(),
			DeactivatedClientCount:       deactivatedClientCount,
		}
	}, server.TxReadCommitted, server.TxLockTimeout(removeNetworkClientsBatchLockTimeout))

	if refusal != nil {
		return result, refusal
	}
	if result.AlreadyApplied {
		glog.Infof("%s\n", result.AuditLine())
		return result, nil
	}
	glog.Infof("%s\n", result.AuditLine())

	revokedSessionCount, status, cleanupComplete, err := enforceNetworkSignOutRetirement(ctx, networkId, operationId)
	result.RevokedSessionCount = revokedSessionCount
	result.SessionCleanupComplete = cleanupComplete
	if status != "" {
		result.Status = status
	}
	result.EnforcementError = err
	return result, nil
}

// Enforces the journaled retirement now, as RecoverSessionOperations does for
// a prepared operation, then runs its client cleanup within a bound. Whatever
// is left, the session maintenance task finishes.
func enforceNetworkSignOutRetirement(
	ctx context.Context,
	networkId server.Id,
	operationId server.Id,
) (revokedSessionCount int, status string, cleanupComplete bool, returnErr error) {
	enforced, err := session.EnforceSessionOperation(ctx, networkId, operationId)
	if err != nil {
		return 0, "", false, err
	}
	revokedSessionCount = enforced.RevokedCount
	status = enforced.State
	if enforced.State != "enforced" {
		return revokedSessionCount, status, enforced.State == "complete", nil
	}
	operation := session.SessionCleanupOperation{
		NetworkId:        networkId,
		OperationId:      operationId,
		TargetSessionIds: enforced.TargetSessionIds,
	}
	for range networkSignOutCleanupBatchLimit {
		complete, err := CleanupSessionOperation(ctx, operation)
		if err != nil {
			return revokedSessionCount, status, false, err
		}
		if complete {
			return revokedSessionCount, "complete", true, nil
		}
	}
	return revokedSessionCount, status, false, nil
}
