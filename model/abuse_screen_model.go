package model

// §21.6's screen at the server's gates (ABUSE-M5-08; D-ABUSE-4, R-ABUSE-3, Q-ABUSE-4, I-14; owner decisions 14, 97,
// 419; QD-235, QD-188, QD-807). sidecar.ScreenName and sidecar.ScreenHost are pure; this file gives them what a
// network's facts are and turns a review verdict into QD-235's decided answer, a refusal with a case:
//
//   - GetNetworkScreenFacts reads, in one place, everything the screen knows of a network: probation, which a
//     restricted network takes too, and which a standing this build does not know takes as the closed direction
//     (§21.7); network_domain's verified domains (MG6, none before); the network's Team scope labels where the
//     process has a scope source; and the names a reviewer allowed it, the targets of its screen_allow actions not
//     lifted. NetworkScreenFacts.For gives ScreenFacts for one name, ReviewAllowed set only for that name.
//   - ScreenNetworkName and ScreenNetworkHost screen a label, alias or network name, and a host label in the
//     network's own zone. An allow passes; a deny answers ErrAbuseScreenDenied, which names no brand, so a gate says
//     the name is not available. A review verdict opens a case, category brand, reporter class screen, joined by the
//     network, or joins the case already open for that name (any age, so a retry a week on counts on the first case),
//     and answers *AbuseScreenReviewError with the case's id, which a gate answers as QuotaRefusal{limit_key:
//     screen_review} (AbuseScreenReviewRefusal). Nothing is stored in §12's tables and nothing reserves the name
//     meanwhile. On probation the pure screen already turns review into deny, so no case opens (§21.7: "no names held
//     for review").
//   - A gate that screens inside its own transaction (dns_records_model.go, dns_alias_model.go) screens with
//     ScreenWithNetworkFacts, which opens nothing, and once its transaction has rolled back hands the
//     *AbuseScreenReviewError to OpenAbuseScreenReview, so the case outlives the refused write.
//   - A reviewer's allow is an abuse_action of kind screen_allow under the review's case (controller's staff kind
//     screen_allow): its target is the name, one DNS label, and its case's network the network. The caller's retry
//     then passes. Lifting it lets no new name of that spelling pass; a live name is never withdrawn here, since an
//     existing name is never screened again, and the CT watch compares live names with the current lists.
//
// The gates, each a server gate because §21.4 counts every routed name as public: a label draw (DnsNameScreen, nil
// facts: a draw has no network to hold a name for), an alias claim, each new record owner's host labels, a rename
// (RenameNetwork), a new network's name (NetworkCreate and NetworkCheck, behind sidecar.yml's dns gate, on
// probation's facts since a new network is on probation), and tls/'s ValidateListen through its facts
// (TlsListenFacts.ScreenAllowedHosts), which reads a short cache per heartbeat and never a query. smtpedge's From
// screen is SMTP-M9-10's call of the pure function. Nothing here logs a name or a network (I-13).

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
)

// A reviewer's allow of one name for one network (§21.6, QD-235): its target is the name, one DNS label, and its
// case's network the network. It acts on nothing outside Postgres.
const AbuseActionKindScreenAllow AbuseActionKind = "screen_allow"

// Who a screen review case came from: the platform's own screen at a gate.
const AbuseReporterClassScreen AbuseReporterClass = "screen"

// QuotaRefusal.limit_key of a name held for review (QD-235).
const AbuseScreenReviewLimitKey = "screen_review"

// How a gate screens a name.
type AbuseScreenSubject string

const (
	// a label, an alias, a Team scope label or a network name, directly below <root> or standing for a network:
	// sidecar.ScreenName, with §12.5's reserved block
	AbuseScreenSubjectName AbuseScreenSubject = "name"
	// one host label inside a network's own zone, a record owner's or a published host's: sidecar.ScreenHost, with
	// §12.7's owner table in place of the block (owner decision 419)
	AbuseScreenSubjectHost AbuseScreenSubject = "host"
)

// The screen refused the name. The text names no brand: a gate says only that the name is not available (§21.6).
var ErrAbuseScreenDenied = errors.New("name not available")

// The screen sent a name to review (QD-235): the gate refuses it, and a reviewer's screen_allow lets a retry pass.
type AbuseScreenReviewError struct {
	NetworkId server.Id
	Subject   AbuseScreenSubject
	// the name screened, one lowercase label
	Name string
	// the case review opened or joined; nil until OpenAbuseScreenReview has run
	CaseId *server.Id
}

// Names no name and no brand.
func (self *AbuseScreenReviewError) Error() string {
	return "name held for review"
}

// The refusal a gate answers. QD-235 has it carry the case id, which QuotaRefusal (operator-core's, owner decision
// 251) has no field for yet (questions/abuse-m5-17-screen-review-case-id.md), so the wire carries the limit key alone
// and the case id stays in this error until the field lands.
func (self *AbuseScreenReviewError) Refusal() *sidecar.QuotaRefusal {
	return &sidecar.QuotaRefusal{LimitKey: AbuseScreenReviewLimitKey}
}

// The review error err holds, or nil.
func AbuseScreenReviewOf(err error) *AbuseScreenReviewError {
	review := &AbuseScreenReviewError{}
	if errors.As(err, &review) {
		return review
	}
	return nil
}

// Tunables of the screen's reads.
type AbuseScreenSettings struct {
	// The most names one network's facts take from its screen_allow actions. A network a reviewer allowed more
	// names than this reads the newest; past it an older allowed name goes to review again, the closed direction.
	// 1024, far past what one network's brands could need.
	AllowedNameMaxCount int
	// The most draws a seedphrase signup makes for a generated name the screen allows (§12.5's redraw on a refusal,
	// as a label draw redraws). A run past it refuses the signup, as a failed draw does.
	GeneratedNameDrawCount int
}

// The defaults, each with its field's reason.
func DefaultAbuseScreenSettings() *AbuseScreenSettings {
	return &AbuseScreenSettings{
		AllowedNameMaxCount:    1024,
		GeneratedNameDrawCount: 16,
	}
}

// What the screen reads of one network, for every name at once.
type NetworkScreenFacts struct {
	// network_domain's verified domains, lowercase, none before MG6
	VerifiedDomains []string
	// on probation or restricted, or in a standing this build does not know (§21.7): no name is held for review
	Probation bool
	// the network's Team scope labels, none where the process has no scope source
	ScopeLabels []string
	// the targets of the network's screen_allow actions not lifted, sorted
	AllowedNames []string
}

// ScreenFacts for name: the network's, with ReviewAllowed only when a reviewer allowed this name. A nil receiver
// is a network of which nothing is known: on probation, nothing verified or allowed, the closed direction.
func (self *NetworkScreenFacts) For(name string) *sidecar.ScreenFacts {
	if self == nil {
		return &sidecar.ScreenFacts{Probation: true}
	}
	_, allowed := slices.BinarySearch(self.AllowedNames, abuseName(name))
	return &sidecar.ScreenFacts{
		VerifiedDomains: slices.Clone(self.VerifiedDomains),
		Probation:       self.Probation,
		ReviewAllowed:   allowed,
		ScopeLabels:     slices.Clone(self.ScopeLabels),
	}
}

// Whether the network's standing takes probation's screen: probation, restricted, and any standing a gate does not
// pass whole. A suspended network creates no name anyway; its gates refuse it before the screen.
func abuseStandingScreensAsProbation(standing AbuseStanding) bool {
	switch standing {
	case AbuseStandingStandard, AbuseStandingTrusted, AbuseStandingSuspended:
		return false
	}
	return true
}

// The network's screen facts at now, read through query, found false for a network that does not exist.
func GetNetworkScreenFactsInTx(ctx context.Context, query server.PgCanQuery, networkId server.Id, now time.Time) (*NetworkScreenFacts, bool) {
	abuseFacts, ok := loadNetworkAbuseFactsWithConn(ctx, query, []server.Id{networkId}, now, false)[networkId]
	if !ok {
		return nil, false
	}
	standing := deriveNetworkAbuseStanding(networkId, abuseFacts.createTime, abuseFacts.row, now, DefaultAbuseSettings())
	facts := &NetworkScreenFacts{
		VerifiedDomains: getNetworkVerifiedDomainsInTx(ctx, query, networkId),
		Probation:       abuseStandingScreensAsProbation(standing.Standing),
		AllowedNames:    GetNetworkScreenAllowedNamesInTx(ctx, query, networkId, DefaultAbuseScreenSettings()),
	}
	if scopeLabels, ok := dnsNetworkScopeLabels(ctx, networkId); ok {
		facts.ScopeLabels = scopeLabels
	}
	return facts, true
}

// The network's screen facts at now, found false for a network that does not exist.
func GetNetworkScreenFacts(ctx context.Context, networkId server.Id, now time.Time) (facts *NetworkScreenFacts, found bool) {
	server.Db(ctx, func(conn server.PgConn) {
		facts, found = GetNetworkScreenFactsInTx(ctx, conn, networkId, now)
	})
	return
}

// ScreenFacts for one name of the network at now: GetNetworkScreenFacts's, For name. A network that does not exist
// reads as one of which nothing is known, the closed direction.
func ScreenFactsForNetwork(ctx context.Context, networkId server.Id, name string, now time.Time) *sidecar.ScreenFacts {
	facts, _ := GetNetworkScreenFacts(ctx, networkId, now)
	return facts.For(name)
}

// The targets of the network's screen_allow actions not lifted, lowercase and sorted, the newest
// AllowedNameMaxCount. None in a binary that does not link MG5.
func GetNetworkScreenAllowedNamesInTx(ctx context.Context, query server.PgCanQuery, networkId server.Id, settings *AbuseScreenSettings) []string {
	names := []string{}
	if !server.SidecarMg5Linked() || settings.AllowedNameMaxCount <= 0 {
		return names
	}
	result, err := query.Query(
		ctx,
		`
			SELECT abuse_action.target
			FROM abuse_action
			WHERE
				abuse_action.network_id = $1 AND
				abuse_action.kind = $2 AND
				abuse_action.review_state NOT IN ($3, $4)
			ORDER BY abuse_action.create_time DESC, abuse_action.action_id DESC
			LIMIT $5
		`,
		networkId,
		string(AbuseActionKindScreenAllow),
		string(AbuseReviewStateLifted),
		string(AbuseReviewStateLiftedWrong),
		settings.AllowedNameMaxCount,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var name string
			server.Raise(result.Scan(&name))
			names = append(names, abuseName(name))
		}
	})
	slices.Sort(names)
	return slices.Compact(names)
}

// The names a reviewer allowed the network, as GetNetworkScreenAllowedNamesInTx reads them.
func GetNetworkScreenAllowedNames(ctx context.Context, networkId server.Id) (names []string) {
	server.Db(ctx, func(conn server.PgConn) {
		names = GetNetworkScreenAllowedNamesInTx(ctx, conn, networkId, DefaultAbuseScreenSettings())
	})
	return
}

// Whether name is one label as the screen and a screen_allow take it: 1 to 63 lowercase letters, digits and
// dashes. The screen's own rules refuse the rest.
func isAbuseScreenName(name string) bool {
	if name == "" || 63 < len(name) {
		return false
	}
	for index := range len(name) {
		c := name[index]
		if !(('a' <= c && c <= 'z') || ('0' <= c && c <= '9') || c == '-') {
			return false
		}
	}
	return true
}

// The pure screen of name under the network's facts: nil for an allow, ErrAbuseScreenDenied for a deny, and an
// *AbuseScreenReviewError with no case for a review, which the caller hands to OpenAbuseScreenReview once it holds
// no transaction whose rollback would take the case with it.
func ScreenWithNetworkFacts(networkId server.Id, subject AbuseScreenSubject, name string, facts *NetworkScreenFacts) error {
	var verdict sidecar.ScreenVerdict
	switch subject {
	case AbuseScreenSubjectName:
		verdict = sidecar.ScreenName(name, facts.For(name))
	case AbuseScreenSubjectHost:
		verdict = sidecar.ScreenHost(name, facts.For(name))
	default:
		return ErrAbuseScreenDenied
	}
	switch verdict {
	case sidecar.ScreenVerdictAllow:
		return nil
	case sidecar.ScreenVerdictReview:
		return &AbuseScreenReviewError{NetworkId: networkId, Subject: subject, Name: abuseName(name)}
	}
	return ErrAbuseScreenDenied
}

// Opens the review's case, or joins the case already open for the network and name, in a transaction of its own,
// and answers review with its case id. With no abuse tables no case can hold the name, so the name is refused as a
// deny, the closed direction.
func OpenAbuseScreenReview(ctx context.Context, review *AbuseScreenReviewError, now time.Time) error {
	if !server.SidecarMg5Linked() {
		return ErrAbuseScreenDenied
	}
	if review.CaseId != nil {
		return review
	}
	if !isAbuseScreenName(review.Name) {
		return ErrAbuseScreenDenied
	}
	settings := DefaultAbuseSettings()
	targetKind := AbuseTargetKindLabel
	if review.Subject == AbuseScreenSubjectHost {
		targetKind = AbuseTargetKindHost
	}
	tier, standing := abuseReportStandingOf(ctx, review.NetworkId)
	reviewDueTime := AbuseReviewDue(AbuseSignalReport, tier, standing, now, abuseReportCalendar(settings))
	networkId := review.NetworkId
	var caseId server.Id
	var err error
	server.Tx(ctx, func(tx server.PgTx) {
		// the lock OpenAbuseCaseInTx takes for this network, category and target, taken first so that the lookup
		// below and the insert are one step; Postgres's transaction locks nest
		server.RaisePgResult(tx.Exec(
			ctx,
			`SELECT pg_advisory_xact_lock(hashtextextended('abuse_case:' || $1::uuid::text || ':' || $2 || ':' || $3, 0))`,
			networkId,
			string(AbuseCategoryBrand),
			review.Name,
		))
		found := false
		result, queryErr := tx.Query(
			ctx,
			`
				SELECT abuse_case.case_id
				FROM abuse_case
				WHERE
					abuse_case.network_id = $1 AND
					abuse_case.target = $2 AND
					abuse_case.category = $3 AND
					abuse_case.reporter_class = $4 AND
					abuse_case.state = 'open' AND
					NOT abuse_case.barred
				ORDER BY abuse_case.create_time DESC, abuse_case.case_id DESC
				LIMIT 1
			`,
			networkId,
			review.Name,
			string(AbuseCategoryBrand),
			string(AbuseReporterClassScreen),
		)
		server.WithPgResult(result, queryErr, func() {
			if result.Next() {
				server.Raise(result.Scan(&caseId))
				found = true
			}
		})
		if found {
			return
		}
		caseId, _, err = OpenAbuseCaseInTx(ctx, tx, &AbuseCaseArgs{
			NetworkId:     &networkId,
			Category:      AbuseCategoryBrand,
			Target:        AbuseTarget{Kind: targetKind, Name: review.Name},
			ReporterClass: AbuseReporterClassScreen,
			JoinKind:      AbuseJoinKindNetwork,
			JoinKey:       networkId.String(),
			ReviewDueTime: reviewDueTime,
		}, now, settings)
	})
	if err != nil {
		return err
	}
	return &AbuseScreenReviewError{
		NetworkId: review.NetworkId,
		Subject:   review.Subject,
		Name:      review.Name,
		CaseId:    &caseId,
	}
}

// Screens name, a label, an alias or a network name of the network, at a gate that holds no transaction: nil for an
// allow, ErrAbuseScreenDenied for a deny, and for a review an *AbuseScreenReviewError naming its case. A network
// that does not exist is refused.
func ScreenNetworkName(ctx context.Context, networkId server.Id, name string) error {
	return screenNetworkSubject(ctx, networkId, AbuseScreenSubjectName, name)
}

// ScreenNetworkName for one host label inside the network's own zone (ScreenHost's subject).
func ScreenNetworkHost(ctx context.Context, networkId server.Id, host string) error {
	return screenNetworkSubject(ctx, networkId, AbuseScreenSubjectHost, host)
}

func screenNetworkSubject(ctx context.Context, networkId server.Id, subject AbuseScreenSubject, name string) error {
	now := server.NowUtc()
	facts, found := GetNetworkScreenFacts(ctx, networkId, now)
	if !found {
		return ErrAbuseScreenDenied
	}
	err := ScreenWithNetworkFacts(networkId, subject, name, facts)
	if review := AbuseScreenReviewOf(err); review != nil {
		return OpenAbuseScreenReview(ctx, review, now)
	}
	return err
}

// Whether a new network's name is screened: sidecar.yml's dns gate, which serves every app user's signup, so the
// screen stays off until M5 opens it.
func networkCreateScreened() bool {
	return server.CurrentSidecarConfig().Service(server.SidecarGateDns).Enabled()
}

// Screens a new network's name, behind the dns gate (networkCreateScreened): nil while the gate is off, else
// ScreenName under probation's facts, since a new network is on probation (§21.7), so a review is a plain deny and
// no case opens. A refusal names no brand.
func ScreenNewNetworkName(name string) error {
	if !networkCreateScreened() {
		return nil
	}
	if sidecar.ScreenName(abuseName(name), &sidecar.ScreenFacts{Probation: true}) != sidecar.ScreenVerdictAllow {
		return ErrAbuseScreenDenied
	}
	return nil
}

// A generated network name the new-network screen allows: draw, called until one passes, at most
// GeneratedNameDrawCount times.
func drawScreenedNetworkName(draw func() (string, error), settings *AbuseScreenSettings) (string, error) {
	for range max(settings.GeneratedNameDrawCount, 1) {
		name, err := draw()
		if err != nil {
			return "", err
		}
		if ScreenNewNetworkName(name) == nil {
			return name, nil
		}
	}
	return "", fmt.Errorf("%w: %d generated names refused", ErrAbuseScreenDenied, settings.GeneratedNameDrawCount)
}

// A screen_allow's target as the action records it: the name, one lowercase label, or an error naming no value.
func AbuseScreenAllowTarget(name string) (string, error) {
	name = abuseName(name)
	if !isAbuseScreenName(name) || strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-") {
		return "", fmt.Errorf("%w: a screen allow names one lowercase label", ErrAbuseActionInvalid)
	}
	return name, nil
}
