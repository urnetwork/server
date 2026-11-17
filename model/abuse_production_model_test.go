package model

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
)

// §21.9's productions (abuse_production_model.go; ABUSE-M5-17): the allowlist against §21.9's text, a production's
// sections, its access log row before anything is read, canaries in content, keys and reasons reaching no export,
// and a sealed request's production writing no notice, admin event or hint. On the private stack with sidecar_pending.

// The §2.8 rows §21.9's Production paragraph names, through its bullets and up to "It never reaches", and gkm's
// membership as the rows §2.8 says it shows in.
func testProductionDesignRows(t testing.TB) []string {
	t.Helper()
	sectionBytes, err := os.ReadFile(filepath.Join("..", "..", "sidecar", "design", "source", "sections", "21-ABUSE.md"))
	if err != nil {
		t.Fatal(err)
	}
	section := string(sectionBytes)
	start := strings.Index(section, "**Production** reaches only")
	end := strings.Index(section, "It never reaches")
	if start < 0 || end < start {
		t.Fatal("§21.9's Production paragraph moved")
	}
	paragraph := section[start:end]
	rowPattern := regexp.MustCompile(`\b(api|connect|evs|gkm|tls|mx|smtp|dns|ops)-[0-9]+\b`)
	rows := rowPattern.FindAllString(paragraph, -1)
	if !strings.Contains(paragraph, "gkm's membership as sidecar ids") {
		t.Fatal("§21.9 no longer names gkm's membership")
	}
	listBytes, err := os.ReadFile(filepath.Join("..", "..", "sidecar", "metadata_list.yml"))
	if err != nil {
		t.Fatal(err)
	}
	membership := regexp.MustCompile(`membership shows only as sidecar ids in ([^,]+(?:, [^,]+)*) and ([a-z]+-[0-9]+), as in api-7`).FindStringSubmatch(string(listBytes))
	if membership == nil {
		t.Fatal("§2.8 no longer says where gkm's membership shows")
	}
	rows = append(rows, rowPattern.FindAllString(membership[1]+" "+membership[2], -1)...)
	slices.Sort(rows)
	return slices.Compact(rows)
}

// The allowlist is exactly §21.9's list, and holds none of the rows §21.9 says a production never reaches.
func TestProductionAllowlistMatchesDesign(t *testing.T) {
	allowed := []string{}
	names := map[string]bool{}
	for _, row := range AbuseProductionRows() {
		if names[row.Name] || row.Reach == "" || len(row.Rows) == 0 {
			t.Fatalf("entry %+v", row)
		}
		names[row.Name] = true
		allowed = append(allowed, row.Rows...)
	}
	slices.Sort(allowed)
	if design := testProductionDesignRows(t); !slices.Equal(allowed, design) {
		t.Fatalf("the allowlist reaches %v, §21.9 %v", allowed, design)
	}
	for _, never := range []string{"connect-2", "connect-3", "tls-2", "dns-2"} {
		if slices.Contains(allowed, never) {
			t.Fatalf("the allowlist reaches %s", never)
		}
	}
}

// A push history source a test fills.
type testProductionPushHistory struct {
	rows []*evsservice.EvsPushHistoryRow
}

func (self *testProductionPushHistory) PushHistory(ctx context.Context, networkId server.Id, write func(row *evsservice.EvsPushHistoryRow) error) (*evsservice.EvsPushHistoryExport, error) {
	for _, row := range self.rows {
		if err := write(row); err != nil {
			return nil, err
		}
	}
	return &evsservice.EvsPushHistoryExport{RefCount: 1, VersionCount: len(self.rows)}, nil
}

// One network with what a production reads, a canary planted in each column it must not: a password hash, an SSO
// token, an identity's and a client's public keys, a binding's signatures, a reviewer's reason, an appellant's text
// and a device's description.
type testProductionWorld struct {
	networkId server.Id
	userAuth  string
	sidecarId server.Id
	caseId    server.Id
	capture   *sidecartest.PrivacyCapture
}

func newTestProductionWorld(t testing.TB, ctx context.Context) *testProductionWorld {
	t.Helper()
	world := &testProductionWorld{networkId: server.NewId(), sidecarId: server.NewId(), capture: sidecartest.NewPrivacyCapture()}
	adminUserId := server.NewId()
	world.userAuth = Testing_CreateNetwork(ctx, world.networkId, "production-"+strings.ReplaceAll(world.networkId.String()[24:], "-", ""), adminUserId)
	capture := world.capture
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `UPDATE network_user SET password_hash = $2 WHERE user_id = $1`, adminUserId, capture.Canary("password hash")))
		server.RaisePgResult(tx.Exec(
			ctx,
			`INSERT INTO network_user_auth_sso (user_id, auth_type, auth_jwt, user_auth) VALUES ($1, 'google', $2, $3)`,
			adminUserId, capture.CanaryText("sso token"), "sso-"+world.userAuth,
		))
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO sidecar_identity (sidecar_id, network_id, identity_public_key, identity_alg, status, create_time)
				VALUES ($1, $2, $3, 'ed25519', 'active', now())
			`,
			world.sidecarId, world.networkId, capture.Canary("identity key"),
		))
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO sidecar_identity_binding (sidecar_id, generation, client_id, client_public_key, signatures, create_time)
				VALUES ($1, 1, $2, $3, $4, now())
			`,
			world.sidecarId, server.NewId(), capture.Canary("client key"), capture.Canary("signatures"),
		))
		server.RaisePgResult(tx.Exec(
			ctx,
			`INSERT INTO network_client (client_id, network_id, active, description) VALUES ($1, $2, true, $3)`,
			server.NewId(), world.networkId, capture.CanaryText("description"),
		))
		var err error
		networkId := world.networkId
		world.caseId, _, err = OpenAbuseCaseInTx(ctx, tx, &AbuseCaseArgs{
			NetworkId:     &networkId,
			Category:      AbuseCategoryPhishing,
			Target:        AbuseTarget{Kind: AbuseTargetKindHost, Name: "shop.production.example"},
			ReporterClass: AbuseReporterClassPublic,
			JoinKind:      AbuseJoinKindHost,
			JoinKey:       "shop.production.example",
		}, server.NowUtc(), DefaultAbuseSettings())
		if err != nil {
			t.Fatal(err)
		}
		reviewerId := server.NewId()
		if _, _, _, err := RecordAbuseActionInTx(ctx, tx, &AbuseActionArgs{
			ActionId:     server.NewId(),
			CaseId:       world.caseId,
			Kind:         AbuseActionKindHostKill,
			Target:       "shop.production.example",
			ReviewerId:   &reviewerId,
			ReviewReason: capture.CanaryText("review reason"),
		}, server.NowUtc(), DefaultAbuseSettings()); err != nil {
			t.Fatal(err)
		}
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO abuse_appeal (appeal_id, case_id, network_id, user_id, reason, state, create_time)
				VALUES ($1, $2, $3, $4, $5, 'pending', now())
			`,
			server.NewId(), world.caseId, world.networkId, adminUserId, capture.CanaryText("appeal reason"),
		))
	})
	return world
}

// A signed-off request naming the world's network.
func (self *testProductionWorld) signedRequest(t testing.TB, ctx context.Context, sealed bool) *LegalRequest {
	t.Helper()
	request := testFileLegalRequest(t, ctx, LegalInstrumentCourtOrder, sealed, self.networkId)
	if _, err := SignLegalProduction(ctx, request.RequestId, server.NewId(), LegalSignoffRoleLegal); err != nil {
		t.Fatal(err)
	}
	signed, err := SignLegalProduction(ctx, request.RequestId, server.NewId(), LegalSignoffRoleEngineering)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

// The production's section of name.
func testProductionSection(t testing.TB, production *AbuseProduction, name string) *AbuseProductionSection {
	t.Helper()
	for _, section := range production.Sections {
		if section.Name == name {
			return section
		}
	}
	t.Fatalf("no section %s", name)
	return nil
}

// The access log rows of a case, by lookup kind.
func testAbuseAccessLogKinds(ctx context.Context, caseId *server.Id) (kinds []string) {
	server.Db(ctx, func(conn server.PgConn) {
		sql := `SELECT lookup_kind FROM abuse_access_log WHERE case_id = $1 ORDER BY access_time, log_id`
		args := []any{caseId}
		if caseId == nil {
			sql = `SELECT lookup_kind FROM abuse_access_log WHERE case_id IS NULL ORDER BY access_time, log_id`
			args = nil
		}
		result, err := conn.Query(ctx, sql, args...)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var kind string
				server.Raise(result.Scan(&kind))
				kinds = append(kinds, kind)
			}
		})
	})
	return
}

// A production holds a section for each allowlist entry, the network's account, identity and case among them, an
// unavailable section saying why, and the push history once a source is installed; its read is logged before
// anything is read, a failed log producing nothing; and no canary reaches the file.
func TestProduceLegalRequestExportsAndLogs(t *testing.T) {
	requireLegalHoldTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestProductionWorld(t, ctx)
		request := world.signedRequest(t, ctx, false)
		reader, err := NewAbuseReader(server.NewId(), DefaultAbuseSettings())
		if err != nil {
			t.Fatal(err)
		}

		// a log that fails produces nothing and leaves the request pending
		restore := Testing_SetAbuseAccessHook(func(step string) error {
			if step == AbuseAccessStepLogBefore {
				return errors.New("the log is down")
			}
			return nil
		})
		_, err = reader.ProduceLegalRequest(ctx, request.RequestId, server.NowUtc(), DefaultAbuseProductionSettings())
		restore()
		if !errors.Is(err, ErrAbuseAccessLogFailed) {
			t.Fatalf("a failed log: %v", err)
		}
		if pending := getLegalRequest(ctx, request.RequestId); pending.ProductionState != LegalProductionStatePending {
			t.Fatalf("a failed log marked the request %s", pending.ProductionState)
		}

		// the log row stands before the first read of the network's rows
		logged := false
		restore = Testing_SetAbuseAccessHook(func(step string) error {
			if step == AbuseAccessStepLogAfter {
				logged = slices.Contains(testAbuseAccessLogKinds(ctx, &request.CaseId), string(AbuseLookupKindProduction))
			}
			return nil
		})
		production, err := reader.ProduceLegalRequest(ctx, request.RequestId, server.NowUtc(), DefaultAbuseProductionSettings())
		restore()
		if err != nil {
			t.Fatal(err)
		}
		if !logged {
			t.Fatal("no access log row before the read")
		}
		if production.Version != AbuseProductionVersion || production.RequestId != request.RequestId ||
			len(production.Sections) != len(AbuseProductionRows()) || production.LegalSignoffId == production.EngineeringSignoffId {
			t.Fatalf("the production: %+v", production)
		}
		if produced := getLegalRequest(ctx, request.RequestId); produced.ProductionState != LegalProductionStateProduced {
			t.Fatalf("the request reads %s", produced.ProductionState)
		}
		for _, name := range []string{"accounts", "identities", "abuse_cases", "contracts", "edge_connections"} {
			if section := testProductionSection(t, production, name); section.State != AbuseProductionStateExported || section.NetworkId != world.networkId {
				t.Errorf("%s: %+v", name, section)
			}
		}
		for _, name := range []string{"reach_graph", "push_history", "drops", "request_log", "group_membership"} {
			if section := testProductionSection(t, production, name); section.State != AbuseProductionStateUnavailable || section.Reason == "" || len(section.Records) != 0 {
				t.Errorf("%s: %+v", name, section)
			}
		}

		productionBytes, err := json.Marshal(production)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{world.userAuth, world.sidecarId.String(), world.caseId.String(), "shop.production.example", "host_kill", "sso-" + world.userAuth} {
			if !strings.Contains(string(productionBytes), want) {
				t.Errorf("the production lacks %q", want)
			}
		}
		world.capture.Add(sidecartest.PrivacyClassObject, "production", productionBytes)
		if findings := world.capture.Scan(); len(findings) != 0 {
			t.Fatalf("canaries in the production: %+v", findings)
		}

		// with a source, the push history is exported, its rows as the source gave them
		pushId := server.NewId()
		SetAbuseProductionPushHistorySource(&testProductionPushHistory{rows: []*evsservice.EvsPushHistoryRow{
			{PushId: sidecar.Id(pushId), Version: 1, TokenPaths: []string{}},
		}})
		defer SetAbuseProductionPushHistorySource(nil)
		production, err = reader.ProduceLegalRequest(ctx, request.RequestId, server.NowUtc(), DefaultAbuseProductionSettings())
		if err != nil {
			t.Fatal(err)
		}
		history := testProductionSection(t, production, "push_history")
		if history.State != AbuseProductionStateExported || len(history.Records) != 1 {
			t.Fatalf("the push history: %+v", history)
		}
		// a bound cuts a section and says so
		settings := DefaultAbuseProductionSettings()
		settings.SectionRecordMaxCount = 1
		production, err = reader.ProduceLegalRequest(ctx, request.RequestId, server.NowUtc(), settings)
		if err != nil {
			t.Fatal(err)
		}
		if accounts := testProductionSection(t, production, "accounts"); accounts.State != AbuseProductionStateTruncated || len(accounts.Records) != 1 {
			t.Fatalf("a bounded section: %+v", accounts)
		}
		if kinds := testAbuseAccessLogKinds(ctx, &request.CaseId); len(kinds) != 3 {
			t.Fatalf("access log rows %v", kinds)
		}
	})
}

// A production under a seal writes nothing the network could see: no admin event, no notice in the outbox, and the
// case stays barred.
func TestProduceSealedRequestWritesNoHint(t *testing.T) {
	requireLegalHoldTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestProductionWorld(t, ctx)
		counts := func() (adminEvents int64, messages int64) {
			server.Db(ctx, func(conn server.PgConn) {
				if server.SidecarMg1Linked() {
					server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM network_admin_event WHERE network_id = $1`, world.networkId).Scan(&adminEvents))
				}
				server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM account_message_outbox WHERE network_id = $1`, world.networkId).Scan(&messages))
			})
			return
		}
		adminEventsBefore, messagesBefore := counts()
		request := world.signedRequest(t, ctx, true)
		reader, err := NewAbuseReader(server.NewId(), DefaultAbuseSettings())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := reader.ProduceLegalRequest(ctx, request.RequestId, server.NowUtc(), DefaultAbuseProductionSettings()); err != nil {
			t.Fatal(err)
		}
		if adminEvents, messages := counts(); adminEvents != adminEventsBefore || messages != messagesBefore {
			t.Fatalf("a sealed production wrote %d admin events and %d messages", adminEvents-adminEventsBefore, messages-messagesBefore)
		}
		if abuseCase := GetAbuseCase(ctx, request.CaseId); !abuseCase.Barred {
			t.Fatal("the sealed request's case is not barred")
		}
	})
}
