//go:build sidecar_e2e && sidecar_pending && sidecar_localstack

// Every read and write of server state the team world (world_team_test.go) and the control-plane case
// (control_plane_concurrency_test.go) make outside the hosts and api's routes, in one file, as the e2e conventions ask
// (QD-1028's layout move will move sidecar's model code): a person's sign-up, which no route the world serves makes; a
// person's credentials, signed as POST /auth/network-switch would mint them; the team tier, by the team grant
// invoice.paid writes; and the counts and rows the case reconciles: a network's clients, its members, its rev/
// objects' subjects and its auth keys' uses.

package sidecare2e

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// A person who signed up: a user with a verified email sign-in and the personal network the sign-up makes
// (model.Testing_CreateNetwork), and that network's credential.
func worldNewPerson(ctx context.Context, name string) (person *WorldPerson, returnErr error) {
	server.HandleError(func() {
		networkId := server.NewId()
		userId := server.NewId()
		networkName := fmt.Sprintf("e2e%s%s", strings.ReplaceAll(strings.ToLower(name), "-", ""), strings.ReplaceAll(networkId.String(), "-", "")[24:32])
		email := model.Testing_CreateNetwork(ctx, networkId, networkName, userId)
		person = &WorldPerson{
			Name:              name,
			UserId:            userId,
			PersonalNetworkId: networkId,
			Email:             email,
			PersonalRoot:      "Bearer " + session.NewByJwt(networkId, userId, networkName, false, false).Testing_Sign(),
		}
	}, func(err error) {
		returnErr = fmt.Errorf("%s's sign-up: %w", name, err)
	})
	return
}

// A person's network credential for a team, naming their membership, as POST /auth/network-switch mints one (§7.5).
func worldTeamCredential(networkId server.Id, networkName string, userId server.Id, memberId server.Id) string {
	credential := session.NewByJwt(networkId, userId, networkName, false, false)
	credential.MemberId = &memberId
	return "Bearer " + credential.Testing_Sign()
}

// Puts networkId, a team team-create made, on the team tier for period from now, as invoice.paid's first period does
// (model.AddTeamGrantInTx; QUOTA-M6-01, QUOTA-M6-02), then refreshes the tier's caches after the commit as its writer
// does (model.UpdateProNetwork), so NetworkTierOf answers team.
func worldGrantTeamTier(ctx context.Context, networkId server.Id, period time.Duration) (returnErr error) {
	server.HandleError(func() {
		now := server.NowUtc()
		server.Tx(ctx, func(tx server.PgTx) {
			if _, err := model.AddTeamGrantInTx(ctx, tx, networkId, now, now.Add(period), 0); err != nil {
				returnErr = err
			}
		})
		if returnErr != nil {
			return
		}
		model.UpdateProNetwork(ctx, networkId)
		if tier := model.NetworkTierOf(ctx, networkId); tier != model.NetworkTierTeam {
			returnErr = fmt.Errorf("network %s reads tier %s after its team grant", networkId, tier)
		}
	}, func(err error) {
		returnErr = err
	})
	return
}

// Gives pro.yml's Pro tier byteCount of data, which a team grant writes as its balance (§20.9's grant: pro's data, 10
// TiB), until the returned pop, where the test stack's pro.yml carries none, so model.AddTeamGrantInTx refuses; a
// process whose pro.yml sets it keeps its own.
func worldInstallTeamData(byteCount int64) func() {
	config := model.Pro()
	previous := config.Pro.Data
	if 0 < previous {
		return func() {}
	}
	config.Pro.Data = model.ByteCount(byteCount)
	return func() {
		config.Pro.Data = previous
	}
}

// taskworker's part for the log's effects (taskworker.go's model.ApplyConfigLogEffectsTask): a task worker claiming
// that task alone, composed as taskworker composes it, over the world's stores and api's sequencer, which the
// sequencer's Effects hook schedules a network at a time after each append. The world runs no taskworker, and what a
// team reads from the effects needs them: network_member's role mirrored from AdminSet and MemberAdmit (TEAM-M6-03),
// on which api's role gate reads each member's permissions. Runs until the returned stop.
func worldRunConfigLogEffects(ctx context.Context) func() {
	settings := task.DefaultTaskWorkerSettings()
	settings.ClaimRegisteredTargetsOnly = true
	workerCtx, stopWorker := context.WithCancel(ctx)
	worker := task.NewTaskWorker(workerCtx, settings)
	worker.AddTargets(task.NewTaskTargetWithPost(model.ApplyConfigLogEffectsTask, model.ApplyConfigLogEffectsTaskPost))
	var lifetime sync.WaitGroup
	lifetime.Go(func() {
		// as taskworker's own supervision: an error ends a pass, and the next starts after the worker's retry wait
		for workerCtx.Err() == nil {
			if server.HandleError(worker.Run) == nil {
				return
			}
			select {
			case <-workerCtx.Done():
				return
			case <-time.After(settings.RetryTimeoutAfterError):
			}
		}
	})
	return func() {
		stopWorker()
		worker.Drain()
		worker.Close()
		lifetime.Wait()
	}
}

// Installs limitKeyValues as pro.yml's team: sidecar: block for the case's life (§20.4's Team column), which the test
// stack's pro.yml does not carry, until the returned pop.
func worldInstallTeamLimits(limitKeyValues map[string]int64) func() {
	return model.Testing_SetSidecarLimits(model.NetworkTierTeam, limitKeyValues)
}

// The network's clients as api holds them: each live (active) client's id, by the client_id; a client the network
// removed or deactivated is not counted.
func worldNetworkActiveClientIds(ctx context.Context, networkId server.Id) (clientIds map[server.Id]bool, returnErr error) {
	server.HandleError(func() {
		clientIds = map[server.Id]bool{}
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`SELECT client_id FROM network_client WHERE network_id = $1 AND active`,
				networkId,
			)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var clientId server.Id
					server.Raise(result.Scan(&clientId))
					clientIds[clientId] = true
				}
			})
		})
	}, func(err error) {
		returnErr = err
	})
	return
}

// The team's members as api's rows hold them: each member_id and its role.
func worldNetworkMemberRoles(ctx context.Context, networkId server.Id) (roles map[server.Id]string, returnErr error) {
	server.HandleError(func() {
		roles = map[server.Id]string{}
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`SELECT member_id, role FROM network_member WHERE network_id = $1`,
				networkId,
			)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var memberId server.Id
					var role string
					server.Raise(result.Scan(&memberId, &role))
					roles[memberId] = role
				}
			})
		})
	}, func(err error) {
		returnErr = err
	})
	return
}

// Sets one of pro.yml's free: sidecar: limits, the tier the world's personal networks are on, to value, until the
// returned pop: a limits phase's cap set just above what a network holds.
func worldSetFreeLimit(limitKey string, value int64) func() {
	return model.Testing_SetSidecarLimits(model.NetworkTierFree, map[string]int64{limitKey: value})
}

// The admissions §20.4's config_admissions counts in state at now, as api's sequencer counts them
// (model.ConfigAdmissionCount): what a limits phase sets its cap above.
func worldConfigAdmissionCount(state *sidecar.ConfigState, now time.Time) int64 {
	return model.ConfigAdmissionCount(state, now.UnixMilli())
}
