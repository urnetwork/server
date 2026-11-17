package model

// §21.9's productions (ABUSE-M5-17; D-ABUSE-6, D-ABUSE-7, R-ABUSE-5, Q-ABUSE-5, I-1, I-13, I-18; owner decision 93):
// what a signed-off legal request exports for counsel, as one file.
//
// AbuseProductionRows is the allowlist, by §2.8 row, and exactly §21.9's list: accounts and identities (api-1,
// api-5), the projection's reach graph (api-7), abuse cases (api-14), connect-1's contract pairs while retained,
// evs's push history (evs-4, evs-12, evs-14), its drops' state (evs-28) and request log (evs-7), gkm's membership as
// sidecar ids (gkm-4, gkm-5, gkm-7, gkm-10, the rows §2.8 says it shows in), edge records (tls-3, mx-2) and send
// records (smtp-3). TestProductionAllowlistMatchesDesign holds the list to §21.9's text. Each entry's exporter takes
// named columns, never a whole row, so a column added to a table reaches no production until an exporter names it:
//
//   - never content, keys, internal names or paths (§2.5, I-1): no password hash or salt, SSO token, identity or
//     client public key, binding signature, appeal or review reason, reviewer id, or device description;
//   - never relay frames or dials (connect-2), inbound sender addresses (mx-2 keeps none), client addresses (I-13:
//     tls-3's visitor hash is keyed and stays as it is, unhashed by nothing) or DNS queries (dns-2);
//   - only what is retained: no prospective collection exists, and nothing here starts a log (owner decision 93).
//
// A row whose reader this build does not have says so in its section, state unavailable with the reason, so a file
// never looks whole when it is not: the reach graph's rows on the first metadata cluster, evs's drops and request log,
// and gkm's objects; evs's push history while no process has installed its source (SetAbuseProductionPushHistorySource,
// which calls evsservice.ExportPushHistory per repo). A section past its row bound says truncated.
//
// AbuseReader.ProduceLegalRequest is the one way to a production: it refuses a closed request and one lacking either
// sign-off or signed twice by one person, writes the read's abuse_access_log row first (lookup kind production, the
// request's case), and marks the request produced. A sealed request stays sealed: a production writes no hint, notice,
// admin event or log line naming a network (owner decisions 93, 184). Nothing reads or writes the tables unless the
// binary links MG5 (server.SidecarMg5Linked).

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"time"

	"github.com/urnetwork/sidecar/evsservice"

	"github.com/urnetwork/server"
)

// What abuse_access_log says of a production's read.
const AbuseLookupKindProduction AbuseLookupKind = "production"

// A production's file format version.
const AbuseProductionVersion = 1

// A section's state.
const (
	AbuseProductionStateExported  = "exported"
	AbuseProductionStateTruncated = "truncated"
	// this build has no reader for the row, or the process no source: the reason says which
	AbuseProductionStateUnavailable = "unavailable"
)

// The request lacks a sign-off, or one person signed both.
var ErrLegalProductionUnsigned = errors.New("legal production not signed off")

// One entry of the allowlist.
type AbuseProductionRow struct {
	// the entry's section name in a production
	Name string
	// the §2.8 rows it reaches
	Rows []string
	// §21.9's words for what it takes
	Reach string
}

// The allowlist, in §21.9's order. See the file header.
func AbuseProductionRows() []*AbuseProductionRow {
	return []*AbuseProductionRow{
		{Name: "accounts", Rows: []string{"api-1"}, Reach: "accounts: the network, its users and members, their sign-in methods, tier and verification"},
		{Name: "identities", Rows: []string{"api-5"}, Reach: "identities: sidecar ids, status and times, and each binding's generation, client id and time"},
		{Name: "reach_graph", Rows: []string{"api-7"}, Reach: "the projection's reach graph"},
		{Name: "abuse_cases", Rows: []string{"api-14"}, Reach: "abuse cases, their actions and appeals, with no reviewer or reason"},
		{Name: "contracts", Rows: []string{"connect-1"}, Reach: "contract pairs (client ids, bytes, times), for the horizon connect-1 states"},
		{Name: "push_history", Rows: []string{"evs-4", "evs-12", "evs-14"}, Reach: "push history: sidecar ids, times, path tokens, padded sizes, DROP statements included"},
		{Name: "drops", Rows: []string{"evs-28"}, Reach: "drops', drop jobs' and dry runs' state for 30 days after each ends"},
		{Name: "request_log", Rows: []string{"evs-7"}, Reach: "evs's 7-day request log"},
		{Name: "group_membership", Rows: []string{"gkm-4", "gkm-5", "gkm-7", "gkm-10"}, Reach: "gkm's membership as sidecar ids"},
		{Name: "edge_connections", Rows: []string{"tls-3"}, Reach: "edge records for 24 h, with keyed visitor-address hashes"},
		{Name: "mail_connections", Rows: []string{"mx-2"}, Reach: "edge records for 24 h, with no sender address"},
		{Name: "sends", Rows: []string{"smtp-3"}, Reach: "send records for 30 days, from M9"},
	}
}

// Tunables of a production.
type AbuseProductionSettings struct {
	// The most records one section of one network takes; a section past it says truncated. 100,000: a busy
	// network's day of edge records, past which counsel narrows the order.
	SectionRecordMaxCount int
	// The most client ids of one network a contract export reads by. 10,000.
	ClientMaxCount int
}

// The defaults, each with its field's reason.
func DefaultAbuseProductionSettings() *AbuseProductionSettings {
	return &AbuseProductionSettings{
		SectionRecordMaxCount: 100000,
		ClientMaxCount:        10000,
	}
}

// One section of a production: one allowlist entry for one network.
type AbuseProductionSection struct {
	Name      string    `json:"name"`
	Rows      []string  `json:"rows"`
	NetworkId server.Id `json:"network_id"`
	State     string    `json:"state"`
	Reason    string    `json:"reason,omitempty"`
	Records   []any     `json:"records"`
}

// A production: the file counsel receives.
type AbuseProduction struct {
	Version              int                       `json:"version"`
	RequestId            server.Id                 `json:"request_id"`
	CaseId               server.Id                 `json:"case_id"`
	Instrument           string                    `json:"instrument"`
	Country              string                    `json:"country"`
	NetworkIds           []server.Id               `json:"network_ids"`
	LegalSignoffId       server.Id                 `json:"legal_signoff_id"`
	EngineeringSignoffId server.Id                 `json:"engineering_signoff_id"`
	ProduceTimeMs        int64                     `json:"produce_time_ms"`
	Sections             []*AbuseProductionSection `json:"sections"`
}

// What gives a production evs's push history: for each repo of the network, evsservice.ExportPushHistory over the
// repo's metadata cluster. Installed by a process that holds the network's repos and its file clusters.
type AbuseProductionPushHistorySource interface {
	PushHistory(ctx context.Context, networkId server.Id, write func(row *evsservice.EvsPushHistoryRow) error) (*evsservice.EvsPushHistoryExport, error)
}

var abuseProductionPushHistorySource atomic.Pointer[AbuseProductionPushHistorySource]

// Installs the process's push history source; nil removes it.
func SetAbuseProductionPushHistorySource(source AbuseProductionPushHistorySource) {
	if source == nil {
		abuseProductionPushHistorySource.Store(nil)
		return
	}
	abuseProductionPushHistorySource.Store(&source)
}

func currentAbuseProductionPushHistorySource() AbuseProductionPushHistorySource {
	if source := abuseProductionPushHistorySource.Load(); source != nil {
		return *source
	}
	return nil
}

// A push history source over evs's own readers: the network's repos, and each repo's stores, from the process.
type evsAbuseProductionPushHistorySource struct {
	repoIds  func(ctx context.Context, networkId server.Id) ([]server.Id, error)
	storesOf func(ctx context.Context, repoId server.Id) (*evsservice.EvsPushHistoryStores, error)
	settings *evsservice.EvsPushHistorySettings
}

// The source a process with the network's repos (repoIds) and their metadata clusters (storesOf) installs.
func NewEvsAbuseProductionPushHistorySource(
	repoIds func(ctx context.Context, networkId server.Id) ([]server.Id, error),
	storesOf func(ctx context.Context, repoId server.Id) (*evsservice.EvsPushHistoryStores, error),
	settings *evsservice.EvsPushHistorySettings,
) AbuseProductionPushHistorySource {
	return &evsAbuseProductionPushHistorySource{repoIds: repoIds, storesOf: storesOf, settings: settings}
}

// See AbuseProductionPushHistorySource.
func (self *evsAbuseProductionPushHistorySource) PushHistory(ctx context.Context, networkId server.Id, write func(row *evsservice.EvsPushHistoryRow) error) (*evsservice.EvsPushHistoryExport, error) {
	repoIds, err := self.repoIds(ctx, networkId)
	if err != nil {
		return nil, err
	}
	total := &evsservice.EvsPushHistoryExport{}
	for _, repoId := range repoIds {
		stores, err := self.storesOf(ctx, repoId)
		if err != nil {
			return nil, err
		}
		export, err := evsservice.ExportPushHistory(ctx, stores, repoId, self.settings, write)
		if err != nil {
			return nil, err
		}
		total.RefCount += export.RefCount
		total.VersionCount += export.VersionCount
		total.Truncated = total.Truncated || export.Truncated
	}
	return total, nil
}

// Exports the request for counsel at now, the path in the file header.
func (self *AbuseReader) ProduceLegalRequest(ctx context.Context, requestId server.Id, now time.Time, settings *AbuseProductionSettings) (*AbuseProduction, error) {
	if !server.SidecarMg5Linked() {
		return nil, ErrAbuseUnlinked
	}
	request := getLegalRequest(ctx, requestId)
	if request == nil {
		return nil, ErrLegalRequestNotFound
	}
	if request.CloseTime != nil {
		return nil, ErrLegalRequestClosed
	}
	if !request.SignedOff() {
		return nil, ErrLegalProductionUnsigned
	}
	caseId := request.CaseId
	if err := self.logRead(ctx, AbuseLookupKindProduction, &caseId); err != nil {
		return nil, err
	}
	production := &AbuseProduction{
		Version:              AbuseProductionVersion,
		RequestId:            request.RequestId,
		CaseId:               request.CaseId,
		Instrument:           string(request.Instrument),
		Country:              request.Country,
		NetworkIds:           slices.Clone(request.NetworkIds),
		LegalSignoffId:       *request.LegalSignoffId,
		EngineeringSignoffId: *request.EngineeringSignoffId,
		ProduceTimeMs:        now.UnixMilli(),
		Sections:             []*AbuseProductionSection{},
	}
	for _, networkId := range request.NetworkIds {
		for _, row := range AbuseProductionRows() {
			production.Sections = append(production.Sections, exportAbuseProductionSection(ctx, row, networkId, settings))
		}
	}
	server.Tx(ctx, func(tx server.PgTx) {
		setLegalRequestProducedInTx(ctx, tx, request.RequestId)
	})
	return production, nil
}

// One section, its exporter's records, or unavailable with its reason.
func exportAbuseProductionSection(ctx context.Context, row *AbuseProductionRow, networkId server.Id, settings *AbuseProductionSettings) *AbuseProductionSection {
	section := &AbuseProductionSection{
		Name:      row.Name,
		Rows:      slices.Clone(row.Rows),
		NetworkId: networkId,
		State:     AbuseProductionStateExported,
		Records:   []any{},
	}
	unavailable := func(reason string) *AbuseProductionSection {
		section.State = AbuseProductionStateUnavailable
		section.Reason = reason
		return section
	}
	var records []any
	switch row.Name {
	case "accounts":
		records = exportAbuseProductionAccounts(ctx, networkId)
	case "identities":
		if !server.SidecarMg0Linked() {
			return unavailable("this binary does not link the identity tables")
		}
		records = exportAbuseProductionIdentities(ctx, networkId, settings)
	case "abuse_cases":
		records = exportAbuseProductionCases(ctx, networkId, settings)
	case "contracts":
		records = exportAbuseProductionContracts(ctx, networkId, settings)
	case "push_history":
		source := currentAbuseProductionPushHistorySource()
		if source == nil {
			return unavailable("no push history source is installed in this process")
		}
		truncated := false
		export, err := source.PushHistory(ctx, networkId, func(historyRow *evsservice.EvsPushHistoryRow) error {
			if settings.SectionRecordMaxCount <= len(records) {
				truncated = true
				return errAbuseProductionSectionFull
			}
			records = append(records, historyRow)
			return nil
		})
		if err != nil && !errors.Is(err, errAbuseProductionSectionFull) {
			return unavailable(fmt.Sprintf("the push history could not be read: %v", err))
		}
		if truncated || (export != nil && export.Truncated) {
			section.State = AbuseProductionStateTruncated
		}
	case "edge_connections":
		records = exportAbuseProductionTlsRecords(ctx, networkId, settings)
	case "mail_connections":
		if !server.SidecarMg7Linked() {
			return unavailable("mail_connection_record exists from M7 (MG7)")
		}
		records = exportAbuseProductionMailRecords(ctx, networkId, settings)
	case "sends":
		if !server.SidecarMg9Linked() {
			return unavailable("smtp_send exists from M9 (MG9)")
		}
		records = exportAbuseProductionSends(ctx, networkId, settings)
	case "reach_graph":
		return unavailable("the projection's rows sit on the first metadata cluster, which no production reader opens yet")
	case "drops":
		return unavailable("evs's drop table and dry runs sit on the repos' clusters, which no production reader opens yet")
	case "request_log":
		return unavailable("evs's request log is its service log, which no production reader reads yet")
	case "group_membership":
		return unavailable("gkm's objects sit on the metadata clusters, which no production reader opens yet")
	default:
		return unavailable("no exporter")
	}
	if records == nil {
		records = []any{}
	}
	if settings.SectionRecordMaxCount < len(records) {
		records = records[:settings.SectionRecordMaxCount]
		section.State = AbuseProductionStateTruncated
	}
	section.Records = records
	return section
}

// A push history writer's stop at the section's bound.
var errAbuseProductionSectionFull = errors.New("abuse production section full")

// Unix milliseconds, 0 for none.
func abuseProductionTimeMs(t *time.Time) int64 {
	if t == nil {
		return 0
	}
	return t.UnixMilli()
}

// api-1's account records.
type AbuseProductionNetworkRecord struct {
	Kind         string    `json:"kind"`
	NetworkId    server.Id `json:"network_id"`
	NetworkName  string    `json:"network_name"`
	AdminUserId  server.Id `json:"admin_user_id"`
	CreateTimeMs int64     `json:"create_time_ms"`
	Tier         string    `json:"tier"`
	Verified     bool      `json:"verified"`
}

type AbuseProductionUserRecord struct {
	Kind     string    `json:"kind"`
	UserId   server.Id `json:"user_id"`
	UserName string    `json:"user_name"`
	AuthType string    `json:"auth_type"`
	UserAuth string    `json:"user_auth"`
	// the member's role and pending state, for a member of an M6 network
	Role    string `json:"role,omitempty"`
	Pending bool   `json:"pending,omitempty"`
}

type AbuseProductionSignInRecord struct {
	Kind         string    `json:"kind"`
	UserId       server.Id `json:"user_id"`
	Method       string    `json:"method"`
	AuthType     string    `json:"auth_type"`
	Identifier   string    `json:"identifier"`
	Verified     bool      `json:"verified"`
	CreateTimeMs int64     `json:"create_time_ms"`
}

// The network, its admin and members, their sign-in methods (with no hash, salt or token), its tier and
// network_verified.
func exportAbuseProductionAccounts(ctx context.Context, networkId server.Id) []any {
	records := []any{}
	userIds := []server.Id{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT network_id, network_name, admin_user_id, create_time FROM network WHERE network_id = $1`,
			networkId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				record := &AbuseProductionNetworkRecord{Kind: "network"}
				var createTime time.Time
				server.Raise(result.Scan(&record.NetworkId, &record.NetworkName, &record.AdminUserId, &createTime))
				record.CreateTimeMs = createTime.UnixMilli()
				records = append(records, record)
				userIds = append(userIds, record.AdminUserId)
			}
		})
	})
	if len(records) == 0 {
		return records
	}
	network := records[0].(*AbuseProductionNetworkRecord)
	network.Tier = string(NetworkTierOf(ctx, networkId))
	network.Verified = IsNetworkVerified(ctx, networkId)
	userRoles := map[server.Id]*AbuseProductionUserRecord{}
	server.Db(ctx, func(conn server.PgConn) {
		if server.SidecarMg6Linked() {
			result, err := conn.Query(
				ctx,
				`SELECT user_id, role, pending FROM network_member WHERE network_id = $1 ORDER BY member_id`,
				networkId,
			)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					member := &AbuseProductionUserRecord{}
					var userId server.Id
					server.Raise(result.Scan(&userId, &member.Role, &member.Pending))
					userRoles[userId] = member
					if !slices.Contains(userIds, userId) {
						userIds = append(userIds, userId)
					}
				}
			})
		}
		result, err := conn.Query(
			ctx,
			`
				SELECT user_id, user_name, auth_type::text, COALESCE(user_auth, '')
				FROM network_user
				WHERE user_id = ANY($1::uuid[])
				ORDER BY user_id
			`,
			userIds,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				user := &AbuseProductionUserRecord{Kind: "user"}
				server.Raise(result.Scan(&user.UserId, &user.UserName, &user.AuthType, &user.UserAuth))
				if member, ok := userRoles[user.UserId]; ok {
					user.Role, user.Pending = member.Role, member.Pending
				}
				records = append(records, user)
			}
		})
		// each sign-in method by what identifies it, never a hash, salt or token
		for _, method := range []struct {
			name string
			sql  string
		}{
			{"password", `SELECT user_id, auth_type, user_auth, verified, create_time FROM network_user_auth_password WHERE user_id = ANY($1::uuid[]) ORDER BY user_id, auth_type`},
			{"sso", `SELECT user_id, auth_type, user_auth, true, create_time FROM network_user_auth_sso WHERE user_id = ANY($1::uuid[]) ORDER BY user_id, auth_type`},
			{"wallet", `SELECT user_id, blockchain, wallet_address, true, create_time FROM network_user_auth_wallet WHERE user_id = ANY($1::uuid[]) ORDER BY user_id`},
		} {
			result, err := conn.Query(ctx, method.sql, userIds)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					signIn := &AbuseProductionSignInRecord{Kind: "sign_in", Method: method.name}
					var createTime time.Time
					server.Raise(result.Scan(&signIn.UserId, &signIn.AuthType, &signIn.Identifier, &signIn.Verified, &createTime))
					signIn.CreateTimeMs = createTime.UnixMilli()
					records = append(records, signIn)
				}
			})
		}
	})
	return records
}

// api-5's records: identities and bindings with no key and no signature.
type AbuseProductionIdentityRecord struct {
	Kind         string    `json:"kind"`
	SidecarId    server.Id `json:"sidecar_id"`
	Status       string    `json:"status"`
	CreateTimeMs int64     `json:"create_time_ms"`
	RevokeTimeMs int64     `json:"revoke_time_ms"`
}

type AbuseProductionBindingRecord struct {
	Kind         string    `json:"kind"`
	SidecarId    server.Id `json:"sidecar_id"`
	Generation   int64     `json:"generation"`
	ClientId     server.Id `json:"client_id"`
	CreateTimeMs int64     `json:"create_time_ms"`
}

func exportAbuseProductionIdentities(ctx context.Context, networkId server.Id, settings *AbuseProductionSettings) []any {
	records := []any{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT sidecar_id, status, create_time, revoke_time
				FROM sidecar_identity
				WHERE network_id = $1
				ORDER BY sidecar_id
				LIMIT $2
			`,
			networkId,
			settings.SectionRecordMaxCount+1,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				identity := &AbuseProductionIdentityRecord{Kind: "identity"}
				var createTime time.Time
				var revokeTime *time.Time
				server.Raise(result.Scan(&identity.SidecarId, &identity.Status, &createTime, &revokeTime))
				identity.CreateTimeMs = createTime.UnixMilli()
				identity.RevokeTimeMs = abuseProductionTimeMs(revokeTime)
				records = append(records, identity)
			}
		})
		result, err = conn.Query(
			ctx,
			`
				SELECT sidecar_identity_binding.sidecar_id, sidecar_identity_binding.generation,
					sidecar_identity_binding.client_id, sidecar_identity_binding.create_time
				FROM sidecar_identity_binding
				INNER JOIN sidecar_identity ON sidecar_identity.sidecar_id = sidecar_identity_binding.sidecar_id
				WHERE sidecar_identity.network_id = $1
				ORDER BY sidecar_identity_binding.sidecar_id, sidecar_identity_binding.generation
				LIMIT $2
			`,
			networkId,
			settings.SectionRecordMaxCount+1,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				binding := &AbuseProductionBindingRecord{Kind: "binding"}
				var createTime time.Time
				server.Raise(result.Scan(&binding.SidecarId, &binding.Generation, &binding.ClientId, &createTime))
				binding.CreateTimeMs = createTime.UnixMilli()
				records = append(records, binding)
			}
		})
	})
	return records
}

// api-14's records: the network's cases, actions and appeals, with no reviewer, reason or appellant's text.
type AbuseProductionCaseRecord struct {
	Kind          string    `json:"kind"`
	CaseId        server.Id `json:"case_id"`
	Category      string    `json:"category"`
	TargetKind    string    `json:"target_kind"`
	Target        string    `json:"target"`
	ReporterClass string    `json:"reporter_class"`
	JoinKind      string    `json:"join_kind"`
	JoinKey       string    `json:"join_key"`
	State         string    `json:"state"`
	Barred        bool      `json:"barred"`
	CreateTimeMs  int64     `json:"create_time_ms"`
	CloseTimeMs   int64     `json:"close_time_ms"`
}

type AbuseProductionActionRecord struct {
	Kind         string    `json:"kind"`
	ActionId     server.Id `json:"action_id"`
	CaseId       server.Id `json:"case_id"`
	ActionKind   string    `json:"action_kind"`
	Target       string    `json:"target"`
	ByMachine    bool      `json:"by_machine"`
	ReviewState  string    `json:"review_state"`
	UntilTimeMs  int64     `json:"until_time_ms"`
	CreateTimeMs int64     `json:"create_time_ms"`
}

type AbuseProductionAppealRecord struct {
	Kind         string    `json:"kind"`
	AppealId     server.Id `json:"appeal_id"`
	CaseId       server.Id `json:"case_id"`
	State        string    `json:"state"`
	CreateTimeMs int64     `json:"create_time_ms"`
	DecideTimeMs int64     `json:"decide_time_ms"`
}

func exportAbuseProductionCases(ctx context.Context, networkId server.Id, settings *AbuseProductionSettings) []any {
	records := []any{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT `+abuseCaseColumns+`
				FROM abuse_case
				WHERE abuse_case.network_id = $1
				ORDER BY abuse_case.case_id
				LIMIT $2
			`,
			networkId,
			settings.SectionRecordMaxCount+1,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				abuseCase := scanAbuseCase(result)
				records = append(records, &AbuseProductionCaseRecord{
					Kind:          "case",
					CaseId:        abuseCase.CaseId,
					Category:      string(abuseCase.Category),
					TargetKind:    string(abuseCase.TargetKind),
					Target:        abuseCase.Target,
					ReporterClass: string(abuseCase.ReporterClass),
					JoinKind:      string(abuseCase.JoinKind),
					JoinKey:       abuseCase.JoinKey,
					State:         string(abuseCase.State),
					Barred:        abuseCase.Barred,
					CreateTimeMs:  abuseCase.CreateTime.UnixMilli(),
					CloseTimeMs:   abuseProductionTimeMs(abuseCase.CloseTime),
				})
			}
		})
		result, err = conn.Query(
			ctx,
			`
				SELECT `+abuseActionColumns+`
				FROM abuse_action
				WHERE abuse_action.network_id = $1
				ORDER BY abuse_action.create_time, abuse_action.action_id
				LIMIT $2
			`,
			networkId,
			settings.SectionRecordMaxCount+1,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				action := scanAbuseAction(result)
				records = append(records, &AbuseProductionActionRecord{
					Kind:         "action",
					ActionId:     action.ActionId,
					CaseId:       action.CaseId,
					ActionKind:   string(action.Kind),
					Target:       action.Target,
					ByMachine:    action.ByMachine,
					ReviewState:  string(action.ReviewState),
					UntilTimeMs:  abuseProductionTimeMs(action.UntilTime),
					CreateTimeMs: action.CreateTime.UnixMilli(),
				})
			}
		})
		result, err = conn.Query(
			ctx,
			`
				SELECT appeal_id, case_id, state, create_time, decide_time
				FROM abuse_appeal
				WHERE network_id = $1
				ORDER BY create_time, appeal_id
				LIMIT $2
			`,
			networkId,
			settings.SectionRecordMaxCount+1,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				appeal := &AbuseProductionAppealRecord{Kind: "appeal"}
				var createTime time.Time
				var decideTime *time.Time
				server.Raise(result.Scan(&appeal.AppealId, &appeal.CaseId, &appeal.State, &createTime, &decideTime))
				appeal.CreateTimeMs = createTime.UnixMilli()
				appeal.DecideTimeMs = abuseProductionTimeMs(decideTime)
				records = append(records, appeal)
			}
		})
	})
	return records
}

// connect-1's records: a contract pair, its bytes and times.
type AbuseProductionContractRecord struct {
	Kind              string    `json:"kind"`
	ContractId        server.Id `json:"contract_id"`
	SourceClientId    server.Id `json:"source_client_id"`
	DestinationClient server.Id `json:"destination_client_id"`
	ByteCount         int64     `json:"byte_count"`
	CreateTimeMs      int64     `json:"create_time_ms"`
	CloseTimeMs       int64     `json:"close_time_ms"`
}

// The contracts naming one of the network's clients, as retained, by the client indexes.
func exportAbuseProductionContracts(ctx context.Context, networkId server.Id, settings *AbuseProductionSettings) []any {
	records := []any{}
	server.Db(ctx, func(conn server.PgConn) {
		clientIds := []server.Id{}
		result, err := conn.Query(
			ctx,
			`SELECT client_id FROM network_client WHERE network_id = $1 ORDER BY client_id LIMIT $2`,
			networkId,
			settings.ClientMaxCount,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var clientId server.Id
				server.Raise(result.Scan(&clientId))
				clientIds = append(clientIds, clientId)
			}
		})
		if len(clientIds) == 0 {
			return
		}
		result, err = conn.Query(
			ctx,
			`
				SELECT contract_id, source_id, destination_id, transfer_byte_count, create_time, close_time
				FROM (
					SELECT contract_id, source_id, destination_id, transfer_byte_count, create_time, close_time
					FROM transfer_contract
					WHERE source_id = ANY($1::uuid[])
					UNION
					SELECT contract_id, source_id, destination_id, transfer_byte_count, create_time, close_time
					FROM transfer_contract
					WHERE destination_id = ANY($1::uuid[])
				) contract
				ORDER BY create_time, contract_id
				LIMIT $2
			`,
			clientIds,
			settings.SectionRecordMaxCount+1,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				contract := &AbuseProductionContractRecord{Kind: "contract"}
				var createTime time.Time
				var closeTime *time.Time
				server.Raise(result.Scan(&contract.ContractId, &contract.SourceClientId, &contract.DestinationClient, &contract.ByteCount, &createTime, &closeTime))
				contract.CreateTimeMs = createTime.UnixMilli()
				contract.CloseTimeMs = abuseProductionTimeMs(closeTime)
				records = append(records, contract)
			}
		})
	})
	return records
}

// tls-3's records: one edge connection, its visitor hash keyed as stored (I-13).
type AbuseProductionTlsRecord struct {
	Kind         string    `json:"kind"`
	ConnectionId server.Id `json:"connection_id"`
	Host         string    `json:"host"`
	StartTimeMs  int64     `json:"start_time_ms"`
	EndTimeMs    int64     `json:"end_time_ms"`
	ByteCount    int64     `json:"byte_count"`
	VisitorHash  string    `json:"visitor_hash"`
}

func exportAbuseProductionTlsRecords(ctx context.Context, networkId server.Id, settings *AbuseProductionSettings) []any {
	records := []any{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT connection_id, host, start_time, end_time, byte_count, visitor_hash
				FROM tls_connection_record
				WHERE network_id = $1
				ORDER BY end_time, connection_id
				LIMIT $2
			`,
			networkId,
			settings.SectionRecordMaxCount+1,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				record := &AbuseProductionTlsRecord{Kind: "tls_connection"}
				var startTime, endTime time.Time
				var visitorHash []byte
				server.Raise(result.Scan(&record.ConnectionId, &record.Host, &startTime, &endTime, &record.ByteCount, &visitorHash))
				record.StartTimeMs = startTime.UnixMilli()
				record.EndTimeMs = endTime.UnixMilli()
				record.VisitorHash = hex.EncodeToString(visitorHash)
				records = append(records, record)
			}
		})
	})
	return records
}

// mx-2's records: a session to the network, with no sender address, which mx-2 never keeps.
type AbuseProductionMailRecord struct {
	Kind        string `json:"kind"`
	StartTimeMs int64  `json:"start_time_ms"`
	ByteCount   int64  `json:"byte_count"`
	Result      string `json:"result"`
}

func exportAbuseProductionMailRecords(ctx context.Context, networkId server.Id, settings *AbuseProductionSettings) []any {
	records := []any{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT start_time, byte_count, result
				FROM mail_connection_record
				WHERE network_id = $1
				ORDER BY start_time
				LIMIT $2
			`,
			networkId,
			settings.SectionRecordMaxCount+1,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				record := &AbuseProductionMailRecord{Kind: "mail_connection"}
				var startTime time.Time
				server.Raise(result.Scan(&startTime, &record.ByteCount, &record.Result))
				record.StartTimeMs = startTime.UnixMilli()
				records = append(records, record)
			}
		})
	})
	return records
}

// smtp-3's records: one send from the shared sending set.
type AbuseProductionSendRecord struct {
	Kind            string    `json:"kind"`
	SendId          string    `json:"send_id"`
	ClientId        server.Id `json:"client_id"`
	SendingAddress  string    `json:"sending_address"`
	RecipientDomain string    `json:"recipient_domain"`
	MxHost          string    `json:"mx_host"`
	StartTimeMs     int64     `json:"start_time_ms"`
	EndTimeMs       int64     `json:"end_time_ms"`
	ByteCount       int64     `json:"byte_count"`
	MessageIdHash   string    `json:"message_id_hash"`
	RecipientCount  int64     `json:"recipient_count"`
	TlsVersion      int       `json:"tls_version"`
	TlsLevel        int       `json:"tls_level"`
	Result          string    `json:"result"`
	RetryOfSendId   string    `json:"retry_of_send_id"`
}

func exportAbuseProductionSends(ctx context.Context, networkId server.Id, settings *AbuseProductionSettings) []any {
	records := []any{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT send_id, client_id, host(sending_address), recipient_domain, mx_host, start_time, end_time,
					byte_count, message_id_hash, recipient_count, tls_version, tls_level, result, retry_of_send_id
				FROM smtp_send
				WHERE network_id = $1
				ORDER BY start_time, send_id
				LIMIT $2
			`,
			networkId,
			settings.SectionRecordMaxCount+1,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				record := &AbuseProductionSendRecord{Kind: "send"}
				var sendId, messageIdHash, retryOf []byte
				var startTime, endTime time.Time
				var tlsLevel int16
				server.Raise(result.Scan(&sendId, &record.ClientId, &record.SendingAddress, &record.RecipientDomain, &record.MxHost,
					&startTime, &endTime, &record.ByteCount, &messageIdHash, &record.RecipientCount, &record.TlsVersion, &tlsLevel,
					&record.Result, &retryOf))
				record.SendId = hex.EncodeToString(sendId)
				record.MessageIdHash = hex.EncodeToString(messageIdHash)
				record.RetryOfSendId = hex.EncodeToString(retryOf)
				record.TlsLevel = int(tlsLevel)
				record.StartTimeMs = startTime.UnixMilli()
				record.EndTimeMs = endTime.UnixMilli()
				records = append(records, record)
			}
		})
	})
	return records
}
