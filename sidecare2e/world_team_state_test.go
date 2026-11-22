//go:build sidecar_e2e && sidecar_pending && sidecar_localstack

// Every read and write of server state the team world (world_team_test.go) and the control-plane case
// (control_plane_concurrency_test.go) make outside the hosts and api's routes, in one file, as the e2e conventions ask
// (QD-1028's layout move will move sidecar's model code): a person's sign-up, which no route the world serves makes; a
// person's credentials, signed as POST /auth/network-switch would mint them; the team tier, by the team grant
// invoice.paid writes; the counts and rows the case reconciles: a network's clients, its members, its unspent invites
// and its admin events; and the quota case's parts (control_plane_concurrency_quota_test.go): a network's limit rows
// (§20.5), its dns_entries count, its connected sidecar set, its stored count and api's over-limit list, with the two
// nightly jobs taskworker runs and the world does not, §20.7's ReconcileSidecarCounters and evs's nightly count.

package sidecare2e

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/sidecarhost"
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

// The invites of networkId's api holds for email, unspent (an accept deletes its invite's row), by invite_id: one per
// create, so an invite made twice shows twice.
func worldNetworkInviteIds(ctx context.Context, networkId server.Id, email string) (inviteIds []server.Id, returnErr error) {
	server.HandleError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`SELECT invite_id FROM network_invite WHERE network_id = $1 AND lower(email) = lower($2) ORDER BY invite_id`,
				networkId,
				email,
			)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var inviteId server.Id
					server.Raise(result.Scan(&inviteId))
					inviteIds = append(inviteIds, inviteId)
				}
			})
		})
	}, func(err error) {
		returnErr = err
	})
	return
}

// How many of networkId's admin events (§7.11's chain) name action on targetId: one per act api ran, so an act run
// twice shows twice.
func worldNetworkAdminEventCount(ctx context.Context, networkId server.Id, action string, targetId server.Id) (count int, returnErr error) {
	server.HandleError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(
				ctx,
				`SELECT COUNT(*) FROM network_admin_event WHERE network_id = $1 AND action = $2 AND target_id = $3`,
				networkId,
				action,
				targetId,
			).Scan(&count))
		})
	}, func(err error) {
		returnErr = err
	})
	return
}

// The memberships userId holds in networkId as api's rows hold them, by member_id: one per join, so a membership made
// twice shows twice.
func worldNetworkMembersOfUser(ctx context.Context, networkId server.Id, userId server.Id) (memberIds []server.Id, returnErr error) {
	server.HandleError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`SELECT member_id FROM network_member WHERE network_id = $1 AND user_id = $2`,
				networkId,
				userId,
			)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var memberId server.Id
					server.Raise(result.Scan(&memberId))
					memberIds = append(memberIds, memberId)
				}
			})
		})
	}, func(err error) {
		returnErr = err
	})
	return
}

// Sets one of networkId's sidecar limits by a network_sidecar_limit row of its own (§20.5's override, as an ops change
// raises a limit or a §21 case tightens one), replacing any row for limitKey, until the returned pop lifts it: a quota
// phase's limit set just above what the network holds. This process's snapshot reloads at once, and every host of the
// world is this process.
func worldSetNetworkLimit(ctx context.Context, networkId server.Id, limitKey string, value int64) (pop func(), returnErr error) {
	server.HandleError(func() {
		if err := model.SetNetworkSidecarLimit(ctx, networkId, limitKey, value, "e2e control-plane case: "+limitKey+" at its limit"); err != nil {
			returnErr = err
		}
	}, func(err error) {
		returnErr = err
	})
	if returnErr != nil {
		return nil, fmt.Errorf("%s's row: %w", limitKey, returnErr)
	}
	return func() {
		server.HandleError(func() {
			model.ClearNetworkSidecarLimit(context.Background(), networkId, limitKey)
		})
	}, nil
}

// §20's dns_entries count of networkId: its user RRsets, one unit each, as the record write counts them under the
// network's lock (model.DnsEntryCount).
func worldDnsEntryCount(ctx context.Context, networkId server.Id) (count int64, returnErr error) {
	server.HandleError(func() {
		count = model.DnsEntryCount(ctx, networkId)
	}, func(err error) {
		returnErr = fmt.Errorf("the dns entries: %w", err)
	})
	return
}

// How many of networkId's sidecar clients server counts connected against sidecar_clients: the live entries of its
// {sq_<network_id>}connected_sidecar set (§20.7's one ZCOUNT).
func worldSidecarConnectedCount(ctx context.Context, networkId server.Id) (count int64, returnErr error) {
	server.HandleError(func() {
		count = model.GetNetworkSidecarConnectedCount(ctx, networkId)
	}, func(err error) {
		returnErr = fmt.Errorf("the connected sidecar clients: %w", err)
	})
	return
}

// networkId's stored count as network_quota_state holds it (§20.7's stored and count rows): the nightly count moved
// by the usage batches' deltas since. 0 for a network with no row yet.
func worldNetworkStoredByteCount(ctx context.Context, networkId server.Id) (storedByteCount int64, returnErr error) {
	server.HandleError(func() {
		state, err := model.GetNetworkQuotaState(ctx, networkId)
		if err != nil {
			returnErr = err
		} else if state != nil {
			storedByteCount = state.StoredByteCount
		}
	}, func(err error) {
		returnErr = fmt.Errorf("the stored count: %w", err)
	})
	return
}

// The storage refusal api's over-limit list (Quota.OverLimit's answer, model.GetQuotaOverLimit) holds for networkId,
// nil while the list does not name it.
func worldStorageOverLimit(ctx context.Context, networkId server.Id) (refusal *sidecar.QuotaRefusal, returnErr error) {
	server.HandleError(func() {
		overLimit, err := model.GetQuotaOverLimit(ctx)
		if err != nil {
			returnErr = err
			return
		}
		refusal = overLimit.StorageNetworkIdRefusals[connect.Id(networkId)]
	}, func(err error) {
		returnErr = fmt.Errorf("the over-limit list: %w", err)
	})
	return
}

// §20.7's nightly ReconcileSidecarCounters, every page from the start (taskworker's QUOTA-M2-12 job, which the world
// does not run): storage_over recomputed from each network's stored count and its limit now, which carries a limit's
// change to the over-limit list.
func worldReconcileSidecarCounters(ctx context.Context, pageCount int) (returnErr error) {
	server.HandleError(func() {
		var after *server.Id
		for {
			result, err := model.ReconcileSidecarCounters(ctx, time.Now(), after, pageCount)
			if err != nil {
				returnErr = err
				return
			}
			if result.LastNetworkId == nil {
				return
			}
			after = result.LastNetworkId
		}
	}, func(err error) {
		returnErr = err
	})
	if returnErr != nil {
		return fmt.Errorf("the nightly reconcile: %w", returnErr)
	}
	return nil
}

// evs's nightly count (evsservice.NightlyPass, as taskworker's evs.nightly lists it, §11.13's bytes row) on a host of
// its own over the world's clusters, with a write budget of its own that reports beside every block's: the world runs no
// taskworker. In a world minutes old the pass deletes nothing, since every orphan, container and pack it judges must be
// a day old first.
type worldNightlyCount struct {
	host   *sidecar.ServiceHost
	budget *evsservice.WriteBudgetClient
	cancel context.CancelFunc
}

// The stand-in's host and budget, until close.
func newWorldNightlyCount(world *World) (*worldNightlyCount, error) {
	ctx, cancel := context.WithCancel(context.Background())
	host := sidecarhost.NewServiceHost(ctx, &sidecarhost.ServiceHostOptions{
		Blobs:         world.T3().Host().Blobs,
		ClockCheck:    sidecartest.NewFakeClockCheck(nil),
		ServiceTokens: world.T3().ServiceTokens(),
	})
	budget, err := evsservice.NewWriteBudget(ctx, host, nil, nil)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("the nightly count's write budget: %w", err)
	}
	host.Budget = budget
	return &worldNightlyCount{host: host, budget: budget, cancel: cancel}, nil
}

// Ends the stand-in's budget, whose report lapses within its ttl.
func (self *worldNightlyCount) close() {
	self.budget.Close()
	self.cancel()
}

// networkId's stored count as evs.nightly lists it: its repos' seg/, pack/ and hp/ objects' sizes, plus the record
// lengths its head windows and log pages give for versions still in batches, over every metadata cluster. An error for
// a cluster the pass did not count whole.
func (self *worldNightlyCount) storedByteCount(ctx context.Context, networkId server.Id) (int64, error) {
	pass, err := evsservice.NewNightlyPass(self.host, &evsservice.NightlyPassDeps{})
	if err != nil {
		return 0, err
	}
	total := int64(0)
	for _, clusterId := range self.host.Blobs.MetadataClusterIds() {
		for _, shard := range pass.Settings().Shards() {
			result, err := pass.Run(ctx, clusterId, shard, "")
			if err != nil {
				return 0, fmt.Errorf("the nightly pass of %s: %w", clusterId, err)
			}
			if !result.Complete || result.More {
				return 0, fmt.Errorf("the nightly pass of %s did not count it whole: complete %t, more %t", clusterId, result.Complete, result.More)
			}
			if bytes, ok := result.NetworkBytes[sidecar.Id(networkId)]; ok {
				total += bytes.StoredByteCount()
			}
		}
	}
	return total, nil
}
