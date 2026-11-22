// The session cap (pro.yml max_sessions, enforce_session_cap), the exemption
// it shares with the concurrent client limit, and what a client's removal
// frees (session1118 FIXPLAN R-cap, R-dark, R-free; owner decisions
// 2026-11-18).
package model

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

func init() {
	session.SetSessionCapPolicySource(func() session.SessionCapPolicy {
		return Pro().SessionCapPolicy()
	})
	session.SetClientCapExemptUntilInTx(clientCapSessionExemptUntilInTx)
}

// SessionCapPolicy is the session cap pro.yml sets: max_sessions live
// sessions per network (zero or less, or no pro.yml, is no cap), refused only
// while enforce_session_cap is true. The retained marker cap is the server's
// (session.NetworkSessionRetainedLimit), under the same switch.
func (self *ProConfig) SessionCapPolicy() session.SessionCapPolicy {
	return session.SessionCapPolicy{
		Live:     max(0, self.MaxSessions),
		Retained: session.NetworkSessionRetainedLimit,
		Enforce:  self.EnforceSessionCap,
	}
}

// Testing_SetEnforceSessionCap overrides enforce_session_cap on the process's
// parsed config, returning a function that restores it. Same caveats as
// Testing_SetEnforceConcurrentClients.
func Testing_SetEnforceSessionCap(enforce bool) func() {
	c := Pro()
	prev := c.EnforceSessionCap
	c.EnforceSessionCap = enforce
	return func() { c.EnforceSessionCap = prev }
}

// Testing_SetMaxSessions overrides max_sessions on the process's parsed
// config, returning a function that restores it.
func Testing_SetMaxSessions(maxSessions int) func() {
	c := Pro()
	prev := c.MaxSessions
	c.MaxSessions = maxSessions
	return func() { c.MaxSessions = prev }
}

// The session cap exempts exactly the clients the concurrent client limit
// exempts (owner decision 2026-11-18, ~23:50Z): a top-level client that
// declared provide intent, while its qualification (a registered Public
// provide key, a passing egress probe, the reliability floor) is pending or
// qualified and its intent connection is live: ClientCapExemptUntil, the one
// predicate both apply. A client with only a Stream key (every connected app),
// a hosted proxy, and a network-only or friends-and-family provider count.
//
// A provider install, a top-level client created with provide intent, skips
// the creation gates of the client limits (AuthNetworkClientArgs.ProvideIntent)
// before it has any intent state; its own session is exempt from its creation
// for as long as a connection's intent presence lasts, after which its intent
// state decides.
const providerInstallSessionExemptTimeout = 6 * time.Minute

// The session mints' exemption (session.SetClientCapExemptUntilInTx): when the
// top-level client's exemption from the client caps ends, or the zero time.
func clientCapSessionExemptUntilInTx(ctx context.Context, tx server.PgTx, networkId server.Id, clientId server.Id) (time.Time, error) {
	// a mint can bind the client to another session: the next record looks
	// its session up again
	clientCapSessionMirror.stateLock.Lock()
	delete(clientCapSessionMirror.entries, clientId)
	clientCapSessionMirror.stateLock.Unlock()
	now := server.NowUtc()
	// Within the mint budget, as the mint's registration: a Redis that does
	// not answer is a mint Redis does not register, which falls back to an
	// untagged or unextended token (owner decision 3) rather than holding the
	// mint's transaction for RedisAuth's two seconds and failing it. The
	// hosted proxy's own session mint sets a budget of its own
	// (session.MintHostedSession).
	redisCtx, cancelRedis := context.WithTimeout(ctx, session.SessionMintRedisBudgetFor(ctx))
	exemptUntil, err := ClientCapExemptUntil(redisCtx, networkId, clientId, now)
	cancelRedis()
	if err != nil {
		return time.Time{}, errors.Join(session.ErrAuthUnavailable, session.ErrSessionStoreUnavailable, err)
	}
	if !exemptUntil.IsZero() {
		return exemptUntil, nil
	}
	var createTime time.Time
	err = tx.QueryRow(ctx, `SELECT create_time FROM network_client_provider_intent WHERE client_id = $1`, clientId).Scan(&createTime)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	if end := createTime.Add(providerInstallSessionExemptTimeout); now.Before(end) {
		return end, nil
	}
	return time.Time{}, nil
}

// The own session of each top-level client whose intent state this process
// wrote, and the exemption end last recorded on it (the zero time: none), so
// an intent connection's presence refresh (every two minutes) records only
// what changes: a renewal before the recorded end lapses.
type clientCapSessionMirrorEntry struct {
	sessionId *server.Id
	end       time.Time
	at        time.Time
}

var clientCapSessionMirror = struct {
	stateLock sync.Mutex
	entries   map[server.Id]clientCapSessionMirrorEntry
}{entries: map[server.Id]clientCapSessionMirrorEntry{}}

// An entry is trusted this long; then the client's session is looked up again.
const clientCapSessionMirrorEntryAge = 30 * time.Minute

// The most clients the mirror remembers per process.
const clientCapSessionMirrorLimit = 65536

// A presence refresh records the exemption again only when the recorded end is
// this close (half an intent presence).
const clientCapSessionMirrorRenewMargin = providerInstallSessionExemptTimeout / 2

// mirrorClientCapSessionExemption records a top-level client's exemption from
// the client caps on its own session (session.UpdateSessionCapExemption) after
// a write of its intent state: the same predicate, read after the write. An
// observation (a presence refresh) records it only when the client's status
// is exempt and the recorded end is near. Best effort: a failure leaves the
// recorded end to lapse, and the client's next intent write or mint records it
// again.
func mirrorClientCapSessionExemption(ctx context.Context, networkId server.Id, clientId server.Id, observation bool, exemptStatus bool) {
	defer func() {
		if recovered := recover(); recovered != nil {
			glog.Infof("[session]client cap exemption of %s not recorded: %v\n", clientId, recovered)
		}
	}()
	now := server.NowUtc()
	clientCapSessionMirror.stateLock.Lock()
	entry, cached := clientCapSessionMirror.entries[clientId]
	clientCapSessionMirror.stateLock.Unlock()
	if cached && now.Sub(entry.at) >= clientCapSessionMirrorEntryAge {
		cached = false
	}
	if observation && cached && (entry.sessionId == nil || !exemptStatus || clientCapSessionMirrorRenewMargin < entry.end.Sub(now)) {
		return
	}
	if !cached {
		entry = clientCapSessionMirrorEntry{at: now}
		server.Db(ctx, func(conn server.PgConn) {
			err := conn.QueryRow(
				ctx,
				`SELECT session_id FROM network_client WHERE client_id = $1 AND network_id = $2 AND active AND source_client_id IS NULL`,
				clientId,
				networkId,
			).Scan(&entry.sessionId)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				server.Raise(err)
			}
		})
	}
	if entry.sessionId != nil {
		exemptUntil, err := ClientCapExemptUntil(ctx, networkId, clientId, now)
		server.Raise(err)
		server.Raise(session.UpdateSessionCapExemption(ctx, networkId, *entry.sessionId, exemptUntil))
		entry.end = exemptUntil
	}
	clientCapSessionMirror.stateLock.Lock()
	defer clientCapSessionMirror.stateLock.Unlock()
	if clientCapSessionMirrorLimit <= len(clientCapSessionMirror.entries) {
		clear(clientCapSessionMirror.entries)
	}
	clientCapSessionMirror.entries[clientId] = entry
}

// recoverSessionRefusal, deferred by a minting route's model entry, returns a
// session refusal its transaction raised (a *session.SessionError, such as the
// cap's 409 session_limit_reached) as the route's error, which the router
// answers with its status and code. Raised, it escaped the route as a 500
// (session1118 D F1). Any other panic continues.
func recoverSessionRefusal[R any](result *R, err *error) {
	recovered := recover()
	if recovered == nil {
		return
	}
	if recoveredErr, ok := recovered.(error); ok {
		var refusal *session.SessionError
		if errors.As(recoveredErr, &refusal) {
			var zero R
			*result = zero
			*err = recoveredErr
			return
		}
	}
	panic(recovered)
}

type sessionFreePathKey struct{}
type sessionFreeSkipKey struct{}
type sessionCleanupTargetsKey struct{}

// A deactivation under this context is a session revocation's cleanup of the
// target sessions' clients: its descendant cascade passes over a descendant
// bound to any other session, and that descendant's own descendants.
func withSessionCleanupTargets(ctx context.Context, targets []server.Id) context.Context {
	return context.WithValue(ctx, sessionCleanupTargetsKey{}, targets)
}

// The descendants the cascade deactivates: under a session cleanup, only those
// bound to a target session or to none.
func sessionCleanupDescendantsInTx(ctx context.Context, tx server.PgTx, clientIds []server.Id) ([]server.Id, error) {
	targets, ok := ctx.Value(sessionCleanupTargetsKey{}).([]server.Id)
	if !ok || len(clientIds) == 0 {
		return clientIds, nil
	}
	rows, err := tx.Query(
		ctx,
		`
			SELECT client_id
			FROM network_client
			WHERE
				client_id = ANY($1) AND
				(session_id IS NULL OR session_id = ANY($2))
			ORDER BY client_id
		`,
		clientIds,
		targets,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	kept := []server.Id{}
	for rows.Next() {
		var clientId server.Id
		if err := rows.Scan(&clientId); err != nil {
			return nil, err
		}
		kept = append(kept, clientId)
	}
	return kept, rows.Err()
}

// The removal path a deactivation under this context frees client sessions
// for (urnetwork_session_freed_on_removal_total{path}).
func withSessionFreePath(ctx context.Context, path string) context.Context {
	return context.WithValue(ctx, sessionFreePathKey{}, path)
}

// A deactivation under this context frees nothing: the session revocation
// cleanup deactivates the clients of sessions it has already revoked.
func withoutSessionFree(ctx context.Context) context.Context {
	return context.WithValue(ctx, sessionFreeSkipKey{}, true)
}

func sessionFreePath(ctx context.Context) string {
	if path, ok := ctx.Value(sessionFreePathKey{}).(string); ok {
		return path
	}
	return session.SessionFreePathOther
}

// After the transaction that deactivated or deleted clients commits, frees
// the client sessions they were bound to that no active client of the
// network carries any more (R-free: a client's removal frees its own session,
// with no marker; the client-active check refuses its every credential).
// session.FreeClientSessions frees only client-owned sessions that no network
// credential ever carried, so a sign-in or code session that other clients or
// a network credential share is never freed. Reads one index range
// (network_client_session_active) in the transaction and writes nothing to
// SQL; takes no lifecycle lock.
func queueClientSessionFreeInTx(ctx context.Context, tx server.PgTx, networkSessionIds map[server.Id][]server.Id) error {
	if skip, _ := ctx.Value(sessionFreeSkipKey{}).(bool); skip {
		return nil
	}
	networkIds := []server.Id{}
	sessionIds := []server.Id{}
	for networkId, sids := range networkSessionIds {
		seen := map[server.Id]bool{}
		for _, sid := range sids {
			if !seen[sid] {
				seen[sid] = true
				networkIds = append(networkIds, networkId)
				sessionIds = append(sessionIds, sid)
			}
		}
	}
	if len(sessionIds) == 0 {
		return nil
	}
	bound := map[[2]server.Id]bool{}
	rows, err := tx.Query(
		ctx,
		`
			SELECT DISTINCT network_client.network_id, network_client.session_id
			FROM unnest($1::uuid[], $2::uuid[]) AS candidate(network_id, session_id)
			INNER JOIN network_client ON
				network_client.network_id = candidate.network_id AND
				network_client.session_id = candidate.session_id
			WHERE
				network_client.active AND
				network_client.session_id IS NOT NULL
		`,
		networkIds,
		sessionIds,
	)
	if err != nil {
		return err
	}
	for rows.Next() {
		var networkId, sid server.Id
		if err := rows.Scan(&networkId, &sid); err != nil {
			rows.Close()
			return err
		}
		bound[[2]server.Id{networkId, sid}] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	free := map[server.Id][]server.Id{}
	for i, sid := range sessionIds {
		if !bound[[2]server.Id{networkIds[i], sid}] {
			free[networkIds[i]] = append(free[networkIds[i]], sid)
		}
	}
	if len(free) == 0 {
		return nil
	}
	path := sessionFreePath(ctx)
	server.AddTxPostCommit(tx, "session_free/"+server.NewId().String(), func() any {
		postCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), server.TxPostCommitTimeout)
		defer cancel()
		return freeClientSessions(postCtx, free, path)
	})
	return nil
}

// How many networks' frees run at once after one commit: an idle reap batch
// marks up to 10,000 clients across as many networks, one script each, inside
// the post's bounded time.
const sessionFreeConcurrency = 16

// Frees each network's sessions (session.FreeClientSessions), a bounded number
// of networks at once. A free that fails or runs out of time leaves its
// sessions listed until their horizon; the first error is returned.
func freeClientSessions(ctx context.Context, free map[server.Id][]server.Id, path string) error {
	type work struct {
		networkId  server.Id
		sessionIds []server.Id
	}
	works := make(chan work)
	errs := make(chan error, len(free))
	var wait sync.WaitGroup
	for range min(sessionFreeConcurrency, len(free)) {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for w := range works {
				server.HandleError(func() {
					if _, err := session.FreeClientSessions(ctx, w.networkId, w.sessionIds, path); err != nil {
						errs <- err
					}
				}, func(err error) { errs <- err })
			}
		}()
	}
	for networkId, sessionIds := range free {
		works <- work{networkId: networkId, sessionIds: sessionIds}
	}
	close(works)
	wait.Wait()
	close(errs)
	for err := range errs {
		return err
	}
	return nil
}
