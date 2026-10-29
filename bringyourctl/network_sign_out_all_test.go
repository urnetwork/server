package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/docopt/docopt-go"
	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// `bringyourctl network sign-out-all` (model/network_sign_out_model.go; owner
// decision 2026-10-29, support inbox 7881).

// Pure: the command is a dry run unless --commit is given, and --commit needs
// the dry run's digest.
func TestNetworkSignOutAllCommandLine(t *testing.T) {
	parse := func(args []string) (docopt.Opts, error) {
		return (&docopt.Parser{HelpHandler: docopt.NoHelpHandler}).ParseArgs(bringyourctlUsage, args, "test")
	}
	networkId := server.NewId().String()
	for _, args := range [][]string{
		{"network", "sign-out-all", "--network_id=" + networkId},
		{"network", "sign-out-all", "--network_id=" + networkId, "--remove-clients"},
		{"network", "sign-out-all", "--network_id=" + networkId, "--commit", "--expect=digest"},
		{"network", "sign-out-all", "--network_id=" + networkId, "--remove-clients", "--commit", "--expect=digest"},
	} {
		opts, err := parse(args)
		if err != nil {
			t.Fatal(args, err)
		}
		network, _ := opts.Bool("network")
		signOutAll, _ := opts.Bool("sign-out-all")
		networkIdArg, _ := opts.String("--network_id")
		removeClients, _ := opts.Bool("--remove-clients")
		commit, _ := opts.Bool("--commit")
		expect, _ := opts.String("--expect")
		if !network || !signOutAll || networkIdArg != networkId ||
			removeClients != slices.Contains(args, "--remove-clients") ||
			commit != slices.Contains(args, "--commit") || (expect == "digest") != commit {
			t.Fatal("command line", args, opts)
		}
	}
	for _, args := range [][]string{
		{"network", "sign-out-all"},
		{"network", "sign-out-all", "--network_id=" + networkId, "--commit"},
		{"network", "sign-out-all", "--network_id=" + networkId, "--expect=digest"},
		{"network", "sign-out-all", "--network_id=" + networkId, "--dry-run"},
	} {
		if _, err := parse(args); err == nil {
			t.Fatal("an invalid command line was accepted", args)
		}
	}
}

// Pure: the dry run's process is read only before it opens a database
// session, and main runs the command.
func TestNetworkSignOutAllDryRunProcessIsReadOnly(t *testing.T) {
	source, err := os.ReadFile("network_sign_out_all.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	readOnly := strings.Index(text, "if !commit {\n\t\tserver.RequirePgReadOnly()\n\t}")
	run := strings.Index(text, "return runNetworkSignOutAll(")
	if readOnly < 0 || run < readOnly {
		t.Fatal("the dry run opens a database session before its process is read only")
	}
	main, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(main), "os.Exit(networkSignOutAllCommand(opts))") {
		t.Fatal("main does not run the sign-out command")
	}
}

// A network with an older sign-in of each kind the server checks: an untagged
// network token, a tracked session's network token, and a client token of that
// session, plus a pending auth code.
type networkSignOutTestNetwork struct {
	networkId server.Id
	userId    server.Id
	name      string
	clientId  server.Id
	// an untagged network token, answered by the SQL state query
	legacy *session.ByJwt
	// a tracked session's network token
	tagged *session.ByJwt
	// a client token of the tracked session
	client     *session.ByJwt
	authCodeId server.Id
}

func newNetworkSignOutTestNetwork(t testing.TB, ctx context.Context, name string) *networkSignOutTestNetwork {
	t.Helper()
	network := &networkSignOutTestNetwork{
		networkId:  server.NewId(),
		userId:     server.NewId(),
		name:       name,
		clientId:   server.NewId(),
		authCodeId: server.NewId(),
	}
	model.Testing_CreateNetwork(ctx, network.networkId, name, network.userId)
	// every credential predates any sign-out of the test
	signInTime := server.NowUtc().Add(-time.Hour)
	network.legacy = session.NewByJwtWithCreateTime(network.networkId, network.userId, name, signInTime, false, false)
	network.tagged = session.NewByJwtWithCreateTime(network.networkId, network.userId, name, signInTime, false, false)
	if _, err := session.MintNetworkSession(ctx, network.tagged, "password"); err != nil {
		t.Fatal(err)
	}
	if network.tagged.SessionId == nil {
		t.Fatal("the session was not tracked")
	}
	deviceId := server.NewId()
	model.Testing_CreateDevice(ctx, network.networkId, deviceId, network.clientId, name, "test")
	network.client = network.tagged.Client(deviceId, network.clientId)
	server.Tx(ctx, func(tx server.PgTx) {
		// bound to the session, as auth-client binds the clients it mints
		server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET session_id=$2, session_create_time=$3 WHERE client_id=$1`, network.clientId, network.tagged.SessionId, network.tagged.CreateTime))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO auth_code(auth_code_id,network_id,user_id,auth_code,create_time,end_time,uses,remaining_uses) VALUES($1,$2,$3,$4,$5,$6,1,1)`, network.authCodeId, network.networkId, network.userId, network.authCodeId.String(), signInTime, server.NowUtc().Add(time.Hour)))
	})
	return network
}

// Each credential's state check with the revocation record read path on or
// off: nil when it is accepted.
func (self *networkSignOutTestNetwork) validate(ctx context.Context, revocationRecord bool) map[string]error {
	restore := session.Testing_SetRevocationRecordEnabled(revocationRecord)
	defer restore()
	return map[string]error{
		"legacy network token":  session.ValidateByJwtState(ctx, self.legacy, false),
		"session network token": session.ValidateByJwtState(ctx, self.tagged, false),
		"session client token":  session.ValidateByJwtState(ctx, self.client, true),
	}
}

func (self *networkSignOutTestNetwork) requireAccepted(t testing.TB, ctx context.Context) {
	t.Helper()
	for _, revocationRecord := range []bool{true, false} {
		for name, err := range self.validate(ctx, revocationRecord) {
			if err != nil {
				t.Fatalf("network %s %s refused (revocation record %t): %v", self.name, name, revocationRecord, err)
			}
		}
	}
}

func (self *networkSignOutTestNetwork) requireRefused(t testing.TB, ctx context.Context) {
	t.Helper()
	for _, revocationRecord := range []bool{true, false} {
		for name, err := range self.validate(ctx, revocationRecord) {
			if err == nil {
				t.Fatalf("network %s %s still accepted (revocation record %t)", self.name, name, revocationRecord)
			}
		}
	}
}

func networkSignOutTestCredentialChangeTime(t testing.TB, ctx context.Context, userId server.Id) time.Time {
	t.Helper()
	var credentialChangeTime time.Time
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT credential_change_time FROM network_user WHERE user_id = $1`, userId).Scan(&credentialChangeTime))
	})
	return credentialChangeTime.UTC()
}

func runNetworkSignOutAllForTest(ctx context.Context, networkId server.Id, removeClients bool, commit bool, expect string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := runNetworkSignOutAll(ctx, &stdout, &stderr, networkId, removeClients, commit, expect)
	return code, stdout.String(), stderr.String()
}

var networkSignOutTestDigestLine = regexp.MustCompile(`(?m)^digest ([0-9a-f]{64})$`)

// The digest the dry run prints, and the commit command it ends with.
func networkSignOutTestDryRun(t testing.TB, ctx context.Context, networkId server.Id, removeClients bool) string {
	t.Helper()
	code, stdout, stderr := runNetworkSignOutAllForTest(ctx, networkId, removeClients, false, "")
	if code != 0 {
		t.Fatalf("dry run exit %d\n%s%s", code, stdout, stderr)
	}
	match := networkSignOutTestDigestLine.FindStringSubmatch(stdout)
	if match == nil {
		t.Fatalf("the dry run printed no digest\n%s", stdout)
	}
	removeClientsFlag := ""
	if removeClients {
		removeClientsFlag = " --remove-clients"
	}
	commitLine := fmt.Sprintf("to commit exactly this: bringyourctl network sign-out-all --network_id=%s%s --commit --expect=%s\n", networkId, removeClientsFlag, match[1])
	if !strings.Contains(stdout, commitLine) {
		t.Fatalf("the dry run did not end with its commit command\n%s", stdout)
	}
	return match[1]
}

// Everything a sign-out could change for the networks: their users' credential
// epochs, their clients, auth codes and session journal rows, and every Redis
// key that names them (revocation records and session state), with its value
// (DUMP, field expirations included) and expiration.
func networkSignOutTestState(t testing.TB, ctx context.Context, networkIds ...server.Id) string {
	t.Helper()
	lines := []string{}
	server.Db(ctx, func(conn server.PgConn) {
		for _, networkId := range networkIds {
			for _, query := range []struct {
				label string
				sql   string
			}{
				{"user", `SELECT network_user.user_id, network_user.credential_change_time FROM network INNER JOIN network_user ON network_user.user_id = network.admin_user_id WHERE network.network_id = $1`},
				{"client", `SELECT client_id, active, deactivate_time, session_id FROM network_client WHERE network_id = $1 ORDER BY client_id`},
				{"auth_code", `SELECT auth_code_id, remaining_uses FROM auth_code WHERE network_id = $1 ORDER BY auth_code_id`},
				{"operation", `SELECT operation_id, action, status, credential_epoch FROM network_session_operation WHERE network_id = $1 ORDER BY operation_id`},
			} {
				result, err := conn.Query(ctx, query.sql, networkId)
				server.WithPgResult(result, err, func() {
					for result.Next() {
						values, err := result.Values()
						server.Raise(err)
						lines = append(lines, fmt.Sprintf("pg %s %s %v", networkId, query.label, values))
					}
				})
			}
		}
	})
	server.Redis(ctx, func(r server.RedisClient) {
		for _, networkId := range networkIds {
			keys := []string{}
			var cursor uint64
			for {
				page, next, err := r.Scan(ctx, cursor, "*"+networkId.String()+"*", 1000).Result()
				server.Raise(err)
				keys = append(keys, page...)
				cursor = next
				if cursor == 0 {
					break
				}
			}
			slices.Sort(keys)
			keys = slices.Compact(keys)
			dumps := make([]*redis.StringCmd, len(keys))
			expireTimes := make([]*redis.Cmd, len(keys))
			_, err := r.Pipelined(ctx, func(pipe redis.Pipeliner) error {
				for i, key := range keys {
					dumps[i] = pipe.Dump(ctx, key)
					expireTimes[i] = pipe.Do(ctx, "PEXPIRETIME", key)
				}
				return nil
			})
			if err != nil && !errors.Is(err, server.RedisNil) {
				server.Raise(err)
			}
			for i, key := range keys {
				dump, err := dumps[i].Result()
				if err != nil && !errors.Is(err, server.RedisNil) {
					server.Raise(err)
				}
				expireTime, err := expireTimes[i].Int64()
				server.Raise(err)
				lines = append(lines, fmt.Sprintf("redis %s %x %d", key, dump, expireTime))
			}
		}
	})
	return strings.Join(lines, "\n")
}

// After a commit, every token of the network created before it is refused by
// both state check paths, including a record reader whose positive projection
// was warm, and the Redis revocation state exists: the credential cutoff in
// every record of the network, the session's revocation marker and record
// tombstones, and the removed client's tombstone.
func TestNetworkSignOutAllCommitRefusesEveryOldToken(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		defer session.Testing_SetSessionCreationEnabled(true)()
		target := newNetworkSignOutTestNetwork(t, ctx, "sign-out-target")
		// accepted, and the record reader projects each tagged credential
		target.requireAccepted(t, ctx)

		digest := networkSignOutTestDryRun(t, ctx, target.networkId, true)
		// The device creates a child client after the dry run. The digest binds
		// the devices, and the commit deactivates the children active then.
		childId := server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client (client_id, network_id, device_id, description, create_time, auth_time, source_client_id) SELECT $1, network_id, device_id, 'child', $3, $3, client_id FROM network_client WHERE client_id = $2`, childId, target.clientId, server.NowUtc()))
		})
		code, stdout, stderr := runNetworkSignOutAllForTest(ctx, target.networkId, true, true, digest)
		if code != 0 {
			t.Fatalf("commit exit %d\n%s%s", code, stdout, stderr)
		}
		for _, line := range []string{"network sign-out-all COMMIT\n", "clients deactivated: 2\n", "auth codes retired: 1\n", "sessions revoked: 1; their clients' cleanup is complete\n", "[audit]network sign-out-all network_id=" + target.networkId.String()} {
			if !strings.Contains(stdout, line) {
				t.Fatalf("the commit did not print %q\n%s", line, stdout)
			}
		}

		target.requireRefused(t, ctx)

		cutoff := networkSignOutTestCredentialChangeTime(t, ctx, target.userId)
		if !target.legacy.CreateTime.Before(cutoff) {
			t.Fatal("credential_change_time did not move past the old sign-ins", cutoff)
		}
		for _, key := range session.Testing_RevocationRecordShardKeys(target.networkId) {
			value, ok, err := session.Testing_RevocationRecordField(ctx, key, session.Testing_RevocationCredentialField)
			if err != nil {
				t.Fatal(err)
			}
			micros, parseErr := strconv.ParseInt(value, 10, 64)
			if !ok || parseErr != nil || micros < cutoff.UnixMicro() {
				t.Fatalf("record %s credential cutoff %q (present %t), want at least %d", key, value, ok, cutoff.UnixMicro())
			}
			value, ok, err = session.Testing_RevocationRecordField(ctx, key, session.Testing_RevocationSessionField(*target.tagged.SessionId))
			if err != nil {
				t.Fatal(err)
			}
			if !ok || value != session.Testing_RevocationTombstone {
				t.Fatalf("record %s session field %q (present %t), want the tombstone", key, value, ok)
			}
		}
		if err := session.CheckSession(ctx, target.networkId, *target.tagged.SessionId); !errors.Is(err, session.ErrSessionRevoked) {
			t.Fatal("the session has no revocation marker", err)
		}
		clientKey := session.Testing_RevocationRecordKey(target.networkId, target.clientId)
		value, ok, err := session.Testing_RevocationRecordField(ctx, clientKey, session.Testing_RevocationClientField(target.clientId))
		if err != nil || !ok || value != session.Testing_RevocationTombstone {
			t.Fatalf("record %s client field %q (present %t, %v), want the tombstone", clientKey, value, ok, err)
		}

		server.Db(ctx, func(conn server.PgConn) {
			for _, clientId := range []server.Id{target.clientId, childId} {
				var active bool
				server.Raise(conn.QueryRow(ctx, `SELECT active FROM network_client WHERE client_id = $1`, clientId).Scan(&active))
				if active {
					t.Fatal("a client is still active", clientId)
				}
			}
			var remainingUses int
			server.Raise(conn.QueryRow(ctx, `SELECT remaining_uses FROM auth_code WHERE auth_code_id = $1`, target.authCodeId).Scan(&remainingUses))
			if remainingUses != 0 {
				t.Fatal("the auth code still has uses", remainingUses)
			}
			var action, status string
			server.Raise(conn.QueryRow(ctx, `SELECT action, status FROM network_session_operation WHERE network_id = $1`, target.networkId).Scan(&action, &status))
			if action != "reset" || status != "complete" {
				t.Fatal("the session retirement was not journaled and enforced", action, status)
			}
		})
	})
}

// A commit changes nothing of another network: its user's credential epoch,
// its tokens on both state check paths, its session, client, auth code,
// journal and every Redis key that names it stay as they were.
func TestNetworkSignOutAllLeavesOtherNetworks(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		defer session.Testing_SetSessionCreationEnabled(true)()
		target := newNetworkSignOutTestNetwork(t, ctx, "sign-out-target")
		other := newNetworkSignOutTestNetwork(t, ctx, "sign-out-other")
		target.requireAccepted(t, ctx)
		other.requireAccepted(t, ctx)
		before := networkSignOutTestState(t, ctx, other.networkId)

		digest := networkSignOutTestDryRun(t, ctx, target.networkId, true)
		code, stdout, stderr := runNetworkSignOutAllForTest(ctx, target.networkId, true, true, digest)
		if code != 0 {
			t.Fatalf("commit exit %d\n%s%s", code, stdout, stderr)
		}
		target.requireRefused(t, ctx)

		if after := networkSignOutTestState(t, ctx, other.networkId); after != before {
			t.Fatalf("the commit changed another network\nbefore:\n%s\nafter:\n%s", before, after)
		}
		other.requireAccepted(t, ctx)
		if err := session.CheckSession(ctx, other.networkId, *other.tagged.SessionId); err != nil {
			t.Fatal("the other network's session was revoked", err)
		}
	})
}

// A user who administers another network shares one credential epoch with it,
// so the sign-out is refused: the dry run prints no digest and a commit writes
// nothing to either network.
func TestNetworkSignOutAllRefusesAUserWhoAdministersAnotherNetwork(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		defer session.Testing_SetSessionCreationEnabled(true)()
		target := newNetworkSignOutTestNetwork(t, ctx, "sign-out-target")
		secondNetworkId := server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network (network_id, network_name, admin_user_id) VALUES ($1, $2, $3)`, secondNetworkId, "sign-out-second", target.userId))
		})
		before := networkSignOutTestState(t, ctx, target.networkId, secondNetworkId)

		code, stdout, stderr := runNetworkSignOutAllForTest(ctx, target.networkId, false, false, "")
		if code == 0 || !strings.Contains(stderr, "also administers 1 other network") || networkSignOutTestDigestLine.MatchString(stdout) || strings.Contains(stdout, "to commit") {
			t.Fatalf("the dry run did not refuse a shared user: exit %d\n%s%s", code, stdout, stderr)
		}
		code, stdout, stderr = runNetworkSignOutAllForTest(ctx, target.networkId, false, true, strings.Repeat("ab", 32))
		if code == 0 || !strings.Contains(stderr, model.ErrNetworkSignOutRefused.Error()) {
			t.Fatalf("the commit did not refuse a shared user: exit %d\n%s%s", code, stdout, stderr)
		}
		if after := networkSignOutTestState(t, ctx, target.networkId, secondNetworkId); after != before {
			t.Fatalf("a refused commit changed state\nbefore:\n%s\nafter:\n%s", before, after)
		}
		target.requireAccepted(t, ctx)
	})
}

// The dry run's process is read only and the dry run changes nothing: no row,
// no Redis key, and every token is still accepted.
func TestNetworkSignOutAllDryRunChangesNothing(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		defer session.Testing_SetSessionCreationEnabled(true)()
		target := newNetworkSignOutTestNetwork(t, ctx, "sign-out-target")
		target.requireAccepted(t, ctx)
		before := networkSignOutTestState(t, ctx, target.networkId)

		server.RequirePgReadOnly()
		t.Cleanup(server.Testing_ReleasePgReadOnly)
		code, stdout, stderr := runNetworkSignOutAllForTest(ctx, target.networkId, true, false, "")
		// the process really was read only: a write on its path fails
		writeRefused := false
		func() {
			defer func() {
				writeRefused = recover() != nil
			}()
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE network_user SET credential_change_time = now() WHERE user_id = $1`, target.userId))
			}, server.OptNoRetry())
		}()
		server.Testing_ReleasePgReadOnly()
		if code != 0 {
			t.Fatalf("dry run exit %d\n%s%s", code, stdout, stderr)
		}
		if !writeRefused {
			t.Fatal("the dry run's process accepted a write")
		}
		for _, line := range []string{
			"network sign-out-all DRY RUN: nothing was changed\n",
			"active clients: 1 (1 top-level, 0 child)\n",
			"live tracked sessions: 1\n",
			"active auth codes: 1\n",
			"session " + target.tagged.SessionId.String() + " accepted until ",
			"client " + target.clientId.String() + " top-level",
		} {
			if !strings.Contains(stdout, line) {
				t.Fatalf("the dry run did not print %q\n%s", line, stdout)
			}
		}
		if !networkSignOutTestDigestLine.MatchString(stdout) {
			t.Fatalf("the dry run printed no digest\n%s", stdout)
		}

		if after := networkSignOutTestState(t, ctx, target.networkId); after != before {
			t.Fatalf("the dry run changed state\nbefore:\n%s\nafter:\n%s", before, after)
		}
		target.requireAccepted(t, ctx)
	})
}

// A commit whose digest is not the census's is refused and writes nothing:
// a digest no dry run printed, and the dry run's own digest once the census
// changed after it.
func TestNetworkSignOutAllRefusesAnotherDigest(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		defer session.Testing_SetSessionCreationEnabled(true)()
		target := newNetworkSignOutTestNetwork(t, ctx, "sign-out-target")
		target.requireAccepted(t, ctx)
		digest := networkSignOutTestDryRun(t, ctx, target.networkId, false)
		before := networkSignOutTestState(t, ctx, target.networkId)

		code, stdout, stderr := runNetworkSignOutAllForTest(ctx, target.networkId, false, true, strings.Repeat("0", 64))
		if code == 0 || !strings.Contains(stderr, model.ErrNetworkSignOutDigestMismatch.Error()) || !strings.Contains(stdout, "COMMIT REFUSED: nothing was committed") {
			t.Fatalf("a digest no dry run printed was not refused: exit %d\n%s%s", code, stdout, stderr)
		}
		// the flag is part of the census
		code, stdout, stderr = runNetworkSignOutAllForTest(ctx, target.networkId, true, true, digest)
		if code == 0 || !strings.Contains(stderr, model.ErrNetworkSignOutDigestMismatch.Error()) {
			t.Fatalf("a commit with another flag was not refused: exit %d\n%s%s", code, stdout, stderr)
		}
		if after := networkSignOutTestState(t, ctx, target.networkId); after != before {
			t.Fatalf("a refused commit changed state\nbefore:\n%s\nafter:\n%s", before, after)
		}

		// a device signs in after the dry run
		model.Testing_CreateDevice(ctx, target.networkId, server.NewId(), server.NewId(), "sign-out-target", "test")
		before = networkSignOutTestState(t, ctx, target.networkId)
		code, stdout, stderr = runNetworkSignOutAllForTest(ctx, target.networkId, false, true, digest)
		if code == 0 || !strings.Contains(stderr, model.ErrNetworkSignOutDigestMismatch.Error()) || !strings.Contains(stdout, "active clients: 2 (2 top-level, 0 child)\n") {
			t.Fatalf("a commit of a changed census was not refused: exit %d\n%s%s", code, stdout, stderr)
		}
		if after := networkSignOutTestState(t, ctx, target.networkId); after != before {
			t.Fatalf("a refused commit changed state\nbefore:\n%s\nafter:\n%s", before, after)
		}
		target.requireAccepted(t, ctx)
	})
}

// Committing the same digest again changes nothing: it finds its operation,
// does not rotate the credential epoch again, and a sign-in made after the
// first commit stays signed in.
func TestNetworkSignOutAllRerunChangesNothing(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		defer session.Testing_SetSessionCreationEnabled(true)()
		target := newNetworkSignOutTestNetwork(t, ctx, "sign-out-target")
		digest := networkSignOutTestDryRun(t, ctx, target.networkId, true)
		code, stdout, stderr := runNetworkSignOutAllForTest(ctx, target.networkId, true, true, digest)
		if code != 0 {
			t.Fatalf("commit exit %d\n%s%s", code, stdout, stderr)
		}
		target.requireRefused(t, ctx)
		cutoff := networkSignOutTestCredentialChangeTime(t, ctx, target.userId)

		// the user signs in again after the sign-out
		fresh := session.NewByJwtWithCreateTime(target.networkId, target.userId, target.name, cutoff.Add(time.Millisecond), false, false)
		if _, err := session.MintNetworkSession(ctx, fresh, "password"); err != nil {
			t.Fatal(err)
		}
		requireFreshAccepted := func() {
			t.Helper()
			for _, revocationRecord := range []bool{true, false} {
				restore := session.Testing_SetRevocationRecordEnabled(revocationRecord)
				err := session.ValidateByJwtState(ctx, fresh, false)
				restore()
				if err != nil {
					t.Fatalf("the sign-in after the sign-out was refused (revocation record %t): %v", revocationRecord, err)
				}
			}
		}
		requireFreshAccepted()
		before := networkSignOutTestState(t, ctx, target.networkId)

		code, stdout, stderr = runNetworkSignOutAllForTest(ctx, target.networkId, true, true, digest)
		if code != 0 || !strings.Contains(stdout, "was already committed by operation") || !strings.Contains(stdout, "nothing changed") {
			t.Fatalf("the rerun was not a no-op: exit %d\n%s%s", code, stdout, stderr)
		}
		if after := networkSignOutTestState(t, ctx, target.networkId); after != before {
			t.Fatalf("the rerun changed state\nbefore:\n%s\nafter:\n%s", before, after)
		}
		if again := networkSignOutTestCredentialChangeTime(t, ctx, target.userId); !again.Equal(cutoff) {
			t.Fatal("the rerun rotated the credential epoch again", cutoff, again)
		}
		requireFreshAccepted()
	})
}
