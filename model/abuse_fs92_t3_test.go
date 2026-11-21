//go:build sidecar_localstack && sidecar_testhooks

package model_test

// FS-92 on T3 against api's real hold/ writes (ABUSE-M10-01; §21.18's M10 row, §21.9's preservation holds, §11.13's
// holds and delete rows, owner decisions 93, 184 and 629, D-ABUSE-8, R-ABUSE-5, Q-ABUSE-7, FI-5, FI-13c, FI-27):
// sidecartest/fault's FS-92 schedules (fs92_hold.go), the builders T2 runs on the fixture, run on T3 Nets (sidecart3)
// whose holds api places and ends (apiHoldApi, behind sidecartest.HoldApi). model.PlaceLegalHold, behind the staff
// path's controller.PlaceLegalHold, writes hold/ on the Net's first metadata cluster, on the local stack's MinIO,
// object before row, its abuse_legal_hold row in the test's Postgres; controller.RunAbuseHoldPass ends each hold at
// its kind's deadline, queueing evs's hold-end sweep (evsservice.EvsSweepHoldEndTaskRequest) before it deletes the
// object, and the schedule runs that sweep as taskworker would. So the drop job's deletes and evs's reads judge the
// bytes api writes, and api's expiry drives each end.
//
// Two readings of the tier, beside each run (holdT3RuleSettings):
//
//   - The local stack's MinIO is a stock image, which has not passed FS-114's settle cases, so sidecar.yml names no
//     settle_passed_build for it and every drop job waits on settle there, deleting nothing (fsDropSettled; evshost's
//     fs_format2_test.go). These runs declare the Net's clusters settled (settledBlobs) so the deletes and what follows
//     them run on MinIO. A stock MinIO evaluates no precondition on a DELETE, so the DELETE's LastModified guard is
//     not the store's here; no schedule here replaces a candidate between its HEAD and its DELETE.
//   - The drop job's young rule reads LastModified, so the store clock is on, and catalog.yml flags FS-92's case
//     store_clock for it (QD-264).
//
// (c) adds api's own answers to the network, GET /abuse/status and GET /abuse/cases, read with the network's bearer
// while the sealed hold stands, to the answers compared with the no-hold twin's.
//
// Each run's api holds rows only for its own Net's network, which the Net's close removes, so one Net's hold pass
// never ends or puts back another's hold.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/sidecartest/fault"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/api/handlers"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/sidecart3"
)

// ----- api behind the seam -----

// §21.9's api for one T3 Net's schedules: server's own placement, renewal and hold pass, over the Net's first metadata
// cluster and the test's Postgres, on the Net's clock. Safe for concurrent use.
type apiHoldApi struct {
	net *sidecartest.Net

	stateLock sync.Mutex
	// the networks api has a row for, made at their first hold as enrollment would have made them, each by its admin
	networkIds map[server.Id]server.Id
}

func newApiHoldApi(net *sidecartest.Net) *apiHoldApi {
	return &apiHoldApi{net: net, networkIds: map[server.Id]server.Id{}}
}

// api's hold/ writer on the Net's first metadata cluster, through the Net's blob recorder, so the oracles' history
// holds api's PUTs and DELETEs.
func (self *apiHoldApi) store() (*model.AbuseHoldStore, error) {
	cluster, err := self.net.Host().Blobs.FirstCluster()
	if err != nil {
		return nil, err
	}
	return model.NewAbuseHoldStore(cluster), nil
}

// The network's row, which api's placement needs, made once as enrollment makes it, and its admin.
func (self *apiHoldApi) EnsureNetwork(ctx context.Context, networkId server.Id) (adminUserId server.Id) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if adminUserId, ok := self.networkIds[networkId]; ok {
		return adminUserId
	}
	adminUserId = server.NewId()
	model.Testing_CreateNetwork(ctx, networkId, "hold"+strings.ReplaceAll(networkId.String()[24:], "-", ""), adminUserId)
	self.networkIds[networkId] = adminUserId
	return adminUserId
}

// The case counsel files the order or report under: a law-enforcement case naming the network, barred when sealed.
func apiHoldCase(ctx context.Context, networkId server.Id, kind sidecar.HoldKind, sealed bool) (caseId server.Id, returnErr error) {
	category := model.AbuseCategoryOther
	if kind == sidecar.HoldKindReport2258a {
		category = model.AbuseCategoryCsam
	}
	server.Tx(ctx, func(tx server.PgTx) {
		caseId, _, returnErr = model.OpenAbuseCaseInTx(ctx, tx, &model.AbuseCaseArgs{
			NetworkId:     &networkId,
			Category:      category,
			Target:        model.AbuseTarget{Kind: model.AbuseTargetKindNetwork, Name: networkId.String()},
			ReporterClass: model.AbuseReporterClassLawEnforcement,
			JoinKind:      model.AbuseJoinKindNetwork,
			JoinKey:       networkId.String(),
			Barred:        sealed || category == model.AbuseCategoryCsam,
		}, server.NowUtc(), model.DefaultAbuseSettings())
	})
	return
}

// Places through controller.PlaceLegalHold, the staff route's path, at the Net's now: the object, then the row with
// its legal-request record owed, then the copy's kick.
func (self *apiHoldApi) PlaceHold(ctx context.Context, placement *sidecartest.HoldPlacement) (*sidecar.HoldRecord, error) {
	networkId := server.Id(placement.NetworkId)
	self.EnsureNetwork(ctx, networkId)
	caseId, err := apiHoldCase(ctx, networkId, placement.Kind, placement.Sealed)
	if err != nil {
		return nil, err
	}
	store, err := self.store()
	if err != nil {
		return nil, err
	}
	model.SetAbuseHoldStore(store)
	model.SetAbuseHoldSoftStateSource(model.NewAbuseHoldSoftStateSource(self.net.Host(), nil))
	restoreNow := controller.Testing_SetLegalHoldNow(self.net.Clock().Now)
	defer restoreNow()
	args := &model.LegalHoldArgs{NetworkId: networkId, CaseId: caseId, Kind: placement.Kind, Scope: placement.Scope}
	if !placement.ReportTime.IsZero() {
		reportTime := placement.ReportTime
		args.ReportTime = &reportTime
	}
	result, err := controller.PlaceLegalHold(ctx, args)
	if err != nil {
		return nil, err
	}
	if !result.Inserted {
		return nil, errors.New("api found the hold placed already")
	}
	// api says when the hold binds every deleter, the readers' cache bound after it (owner decision 629), which
	// the schedules wait out before they release a deleter meant to see the hold
	if bindTime := self.net.Clock().Now().Add(sidecar.DefaultHoldSetSettings().RereadInterval); !result.BindTime.Equal(bindTime) {
		return nil, fmt.Errorf("api's placement binds deleters from %s, not %s", result.BindTime, bindTime)
	}
	return result.Hold.Record(), nil
}

// Renews through controller.RenewLegalHold at the Net's now.
func (self *apiHoldApi) RenewHold(ctx context.Context, networkId sidecar.Id, caseId sidecar.Id) (*sidecar.HoldRecord, error) {
	store, err := self.store()
	if err != nil {
		return nil, err
	}
	model.SetAbuseHoldStore(store)
	restoreNow := controller.Testing_SetLegalHoldNow(self.net.Clock().Now)
	defer restoreNow()
	result, err := controller.RenewLegalHold(ctx, server.Id(networkId), server.Id(caseId))
	if err != nil {
		return nil, err
	}
	return result.Hold.Record(), nil
}

// controller.RunAbuseHoldPass at the Net's now, its sweeps queued in tasks. The holds it ended are those of this
// api's networks that stood before it and stand no longer.
func (self *apiHoldApi) RunHoldPass(ctx context.Context, tasks sidecar.Tasks) ([]*sidecar.HoldRecord, error) {
	store, err := self.store()
	if err != nil {
		return nil, err
	}
	self.stateLock.Lock()
	networkIds := []server.Id{}
	for networkId := range self.networkIds {
		networkIds = append(networkIds, networkId)
	}
	self.stateLock.Unlock()
	before := map[model.LegalHoldKey]*model.LegalHold{}
	for _, networkId := range networkIds {
		for _, hold := range model.GetLegalHolds(ctx, networkId) {
			before[model.LegalHoldKey{NetworkId: hold.NetworkId, CaseId: hold.CaseId}] = hold
		}
	}
	result := controller.RunAbuseHoldPass(ctx, &controller.AbuseHoldPassDeps{Store: store, Tasks: tasks, Now: self.net.Clock().Now})
	if 0 < result.FailedCount {
		return nil, fmt.Errorf("api's hold pass failed %d holds", result.FailedCount)
	}
	after := map[model.LegalHoldKey]bool{}
	for _, networkId := range networkIds {
		for _, hold := range model.GetLegalHolds(ctx, networkId) {
			after[model.LegalHoldKey{NetworkId: hold.NetworkId, CaseId: hold.CaseId}] = true
		}
	}
	ended := []*sidecar.HoldRecord{}
	for key, hold := range before {
		if !after[key] {
			ended = append(ended, hold.Record())
		}
	}
	return ended, nil
}

// Removes the rows of this api's networks, so the next Net's pass reads none of them.
func (self *apiHoldApi) close(ctx context.Context) {
	self.stateLock.Lock()
	networkIds := []server.Id{}
	for networkId := range self.networkIds {
		networkIds = append(networkIds, networkId)
	}
	self.stateLock.Unlock()
	if len(networkIds) == 0 {
		return
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `DELETE FROM abuse_legal_hold WHERE network_id = ANY($1)`, networkIds))
	})
}

// ----- the tier's readings -----

// A BlobStore whose clusters read as having passed FS-114's settle cases: see the file header.
type settledBlobs struct {
	sidecar.BlobStore
}

func (self *settledBlobs) Cluster(clusterId string) (sidecar.BlobCluster, error) {
	cluster, err := self.BlobStore.Cluster(clusterId)
	if err != nil {
		return nil, err
	}
	return &settledCluster{BlobCluster: cluster}, nil
}

func (self *settledBlobs) FirstCluster() (sidecar.BlobCluster, error) {
	cluster, err := self.BlobStore.FirstCluster()
	if err != nil {
		return nil, err
	}
	return &settledCluster{BlobCluster: cluster}, nil
}

func (self *settledBlobs) SegmentCluster() (sidecar.BlobCluster, error) {
	cluster, err := self.BlobStore.SegmentCluster()
	if err != nil {
		return nil, err
	}
	return &settledCluster{BlobCluster: cluster}, nil
}

type settledCluster struct {
	sidecar.BlobCluster
}

func (self *settledCluster) SettlePassed() bool {
	return true
}

// Each hold api placed through a T3 Net of these settings, by Net, for a test that reads api's rows after a run.
var apiHoldApis sync.Map

// sidecart3's rule settings for the hold cases (see the file header): the store clock on, and each Net's clusters
// declared settled and its holds placed and ended by api. Holds are turned on for the test, as abuse.yml's
// holds_enabled turns them on once counsel confirms (§23.7).
func holdT3RuleSettings(t testing.TB) *fault.RuleSettings {
	popConfig := server.Testing_PushAbuseConfig("holds_enabled: true\n")
	t.Cleanup(func() {
		model.SetAbuseHoldStore(nil)
		model.SetAbuseHoldSoftStateSource(nil)
		popConfig()
	})
	settings := sidecart3.DefaultSettings()
	settings.StoreClock = sidecart3.DefaultStoreClockSettings()
	ruleSettings := sidecart3.RuleSettings(t, settings)
	engines := ruleSettings.Net.Engines
	ruleSettings.Net.Engines = func(ctx context.Context, net *sidecartest.Net) (*sidecar.ServiceHost, func(), error) {
		host, closeHost, err := engines(ctx, net)
		if err != nil {
			return nil, nil, err
		}
		host.Blobs = &settledBlobs{BlobStore: host.Blobs}
		api := newApiHoldApi(net)
		sidecartest.SetNetHoldApi(net, api)
		apiHoldApis.Store(net, api)
		return host, func() {
			api.close(context.Background())
			apiHoldApis.Delete(net)
			closeHost()
		}, nil
	}
	return ruleSettings
}

// ----- the runs -----

// FS-92 (a), the registered case, beside its no-hold twin, under every rule the row takes.
func TestFs92HoldsPlacedByApiOnT3(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		requireHoldTablesOnT3(tb)
		sidecart3.RunRow(tb, "FS-92", "", holdT3RuleSettings(tb))
	})
}

// FS-92 (b): a hold api places between the job's verify and its deletes.
func TestFs92BetweenVerifyAndDeleteOnT3(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		requireHoldTablesOnT3(tb)
		sidecart3.RunRegisteredClause(tb, "FS-92", "between verify and delete", holdT3RuleSettings(tb))
	})
}

// FS-92 (b): a hold api places between two of the job's DELETEs.
func TestFs92BetweenDeletesOnT3(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		requireHoldTablesOnT3(tb)
		sidecart3.RunRegisteredClause(tb, "FS-92", "between deletes", holdT3RuleSettings(tb))
	})
}

// FS-92 (b): a DELETE issued as api places the hold completes, and none follows it.
func TestFs92ADeleteInFlightOnT3(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		requireHoldTablesOnT3(tb)
		sidecart3.RunRegisteredClause(tb, "FS-92", "a delete in flight", holdT3RuleSettings(tb))
	})
}

// FS-92 (c): a sealed hold api places changes no answer the network takes, from evs or from api's GET /abuse/status
// and GET /abuse/cases, against a twin network with no hold.
func TestFs92SealedHoldAnswersEqualANoHoldTwinOnT3(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		requireHoldTablesOnT3(tb)
		ruleSettings := holdT3RuleSettings(tb)
		twins, err := fault.Fs92SealedHoldTwins(tb.Context(), func() *sidecartest.Net {
			return sidecartest.NewNet(tb.Context(), ruleSettings.Net)
		}, apiAbuseAnswers)
		if err != nil {
			tb.Fatal(err)
		}
		statusRead, casesRead := false, false
		for _, line := range twins.HeldAnswers.Lines() {
			statusRead = statusRead || strings.HasPrefix(line, "api GET /abuse/status: 200")
			casesRead = casesRead || strings.HasPrefix(line, "api GET /abuse/cases: 200")
		}
		if !statusRead || !casesRead {
			tb.Fatalf("api's answers were not compared: status %t, cases %t", statusRead, casesRead)
		}
	})
}

// fault.Fs92TierAnswers on T3: the network's row as enrollment leaves it, then api's GET /abuse/status and GET
// /abuse/cases under the network's own bearer, each answer's status and body kept. A sealed hold's barred case and
// its row must show in neither.
func apiAbuseAnswers(ctx context.Context, net *sidecartest.Net, networkId sidecar.Id, answers *fault.HoldAnswers) error {
	value, ok := apiHoldApis.Load(net)
	if !ok {
		return errors.New("the Net has no api for holds")
	}
	adminUserId := value.(*apiHoldApi).EnsureNetwork(ctx, server.Id(networkId))
	byJwt := session.NewByJwt(server.Id(networkId), adminUserId, "hold", false, false)
	bearer := "Bearer " + byJwt.Testing_Sign()
	abuseHandlers := handlers.NewAbuseHandlers(nil)
	routes := router.NewRouter(ctx, []*router.Route{
		router.NewRoute("GET", "/abuse/status", abuseHandlers.Status),
		router.NewRoute("GET", "/abuse/cases", abuseHandlers.Cases),
	})
	for _, path := range []string{"/abuse/status", "/abuse/cases"} {
		request := httptest.NewRequest("GET", path, nil)
		request.Header.Set("Authorization", bearer)
		recorder := httptest.NewRecorder()
		routes.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			return fmt.Errorf("api's %s answered %d: %s", path, recorder.Code, recorder.Body.String())
		}
		if !json.Valid(recorder.Body.Bytes()) {
			return fmt.Errorf("api's %s answered a body that is not JSON", path)
		}
		answers.Add(fmt.Sprintf("api GET %s: %d %s", path, recorder.Code, strings.TrimSpace(recorder.Body.String())))
	}
	return nil
}

// Skips a run in a build that does not link MG5, whose hold tables api writes.
func requireHoldTablesOnT3(t testing.TB) {
	t.Helper()
	if !server.SidecarMg5Linked() {
		t.Skip("MG5 is pending: these cases run with -tags sidecar_pending")
	}
}
