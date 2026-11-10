package model

// §7.6's verified domains (R-TEAM-4, R-TEAM-12; §2.8 api-3; TEAM-M6-06): a team
// proves a DNS domain by publishing a TXT record, and POST /auth/sso-discover
// then routes that domain's sign-ins to the team's IdP.
//
//	add       POST /network/domain-add makes the row and answers the record to
//	          publish: TXT _urnetwork-challenge.<domain>, its value the row's
//	          challenge, 256 random bits. Adding a domain again answers the
//	          same record
//	verify    POST /network/domain-verify looks the record up through the
//	          process's resolver and, when a value is the challenge, marks the
//	          row verified. One network per verified domain: the first to
//	          verify holds it, and another network's proof is refused while it
//	          does
//	recheck   every verified domain is looked up again once a day
//	          (RecheckNetworkDomains). A check that does not find the challenge,
//	          a lookup error included, starts or continues a failing run, a
//	          check that finds it ends the run, and a domain whose run reaches
//	          7 days is dropped: its row goes, and discovery stops routing it
//	pending   a domain added and never verified goes 7 days after it was added,
//	          so no row outlasts §2.8 api-3's "while verified, dropped after 7
//	          failed days"
//	discover  NetworkSsoIdpOfEmail answers the IdP of the verified domain an
//	          address is in. It reads nothing of the address but its domain and
//	          no user, so it cannot say whether an address exists, and an
//	          unknown domain, an unverified one and a verified one whose team
//	          has no IdP answer alike: nothing
//
// The routes are the team's Owner's (the role gate asks RolePermissionOwner,
// and each checks the caller's membership itself), Team only (§7.5), and each
// change writes an admin event in its own transaction (§7.11). A new domain,
// and its verification, owe Owners and Admins the network authority notice
// (§7.8). An admin event names no domain: network_domain has no id, and an
// event holds ids and fixed words only (I-13).
//
// Nothing here reads or writes network_domain unless the binary links MG6
// (server.SidecarMg6Linked); until then each route answers 503 and discovery
// finds nothing.

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// Tunables of verified domains.
type NetworkDomainSettings struct {
	// Between two checks of one verified domain: a day (§7.6). Non-positive
	// checks every domain on every pass.
	RecheckInterval time.Duration
	// How long a failing run lasts before its domain is dropped, and how long
	// a domain may stay added and unverified: 7 days (§2.8 api-3's ttl).
	DropAfter time.Duration
	// The most domains one network holds, verified or not: 20, a company's
	// brands. Non-positive holds none.
	DomainLimit int
	// The least time between two lookups one row asks for through
	// domain-verify: 10 s, so a caller cannot turn the route into a stream of
	// lookups. Non-positive bounds nothing.
	VerifyInterval time.Duration
	// One TXT lookup's bound: 10 s.
	LookupTimeout time.Duration
	// The most rows one recheck pass examines; the next pass takes the rest
	// from this one's cursor. Non-positive examines one.
	RecheckBatchCount int
}

func DefaultNetworkDomainSettings() *NetworkDomainSettings {
	return &NetworkDomainSettings{
		RecheckInterval:   24 * time.Hour,
		DropAfter:         7 * 24 * time.Hour,
		DomainLimit:       20,
		VerifyInterval:    10 * time.Second,
		LookupTimeout:     10 * time.Second,
		RecheckBatchCount: 500,
	}
}

// The label a domain's challenge is published under (§7.6).
const NetworkDomainChallengeLabel = "_urnetwork-challenge"

// The longest domain a challenge name still fits a DNS name with, and
// network_domain.domain's width.
const networkDomainMaxLength = 253 - len(NetworkDomainChallengeLabel) - 1

// Looks TXT records up for domain verification. The process's resolver is
// injected (SetNetworkDomainResolver), so tests and a deployment with its own
// validating resolver replace it.
type NetworkDomainResolver interface {
	// The TXT values at name, each record's strings joined. A name with no
	// TXT record is an empty list or an error; both are a check that did not
	// find the challenge.
	LookupTxt(ctx context.Context, name string) ([]string, error)
}

// The system resolver.
type systemNetworkDomainResolver struct {
	resolver *net.Resolver
}

func (self *systemNetworkDomainResolver) LookupTxt(ctx context.Context, name string) ([]string, error) {
	return self.resolver.LookupTXT(ctx, name)
}

var networkDomainResolver atomic.Pointer[NetworkDomainResolver]

// Installs the process's resolver; nil restores the system's.
func SetNetworkDomainResolver(resolver NetworkDomainResolver) {
	if resolver == nil {
		networkDomainResolver.Store(nil)
		return
	}
	networkDomainResolver.Store(&resolver)
}

func currentNetworkDomainResolver() NetworkDomainResolver {
	if resolver := networkDomainResolver.Load(); resolver != nil {
		return *resolver
	}
	return &systemNetworkDomainResolver{resolver: net.DefaultResolver}
}

// What the domain routes depend on apart from the database, so tests can
// replace it.
type networkDomains struct {
	settings        *NetworkDomainSettings
	now             func() time.Time
	requireTeamTier func(ctx context.Context, networkId server.Id) error
	resolver        func() NetworkDomainResolver
}

func newNetworkDomains() *networkDomains {
	return &networkDomains{
		settings:        DefaultNetworkDomainSettings(),
		now:             server.NowUtc,
		requireTeamTier: RequireTeamTier,
		resolver:        currentNetworkDomainResolver,
	}
}

// The domain as network_domain holds it: lowercase ASCII with no trailing dot,
// at least two labels of letters, digits and inner hyphens, and a last label
// that is not a number, so no address passes for one. An internationalized
// domain goes in its xn-- form.
func NormalizeNetworkDomain(domain string) (string, error) {
	invalid := errors.New("The domain is not a DNS domain name.")
	domain = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(domain)), ".")
	if domain == "" || networkDomainMaxLength < len(domain) {
		return "", invalid
	}
	labels := strings.Split(domain, ".")
	if len(labels) < 2 {
		return "", invalid
	}
	for _, label := range labels {
		if label == "" || 63 < len(label) || label[0] == '-' || label[len(label)-1] == '-' {
			return "", invalid
		}
		for i := 0; i < len(label); i += 1 {
			c := label[i]
			if !('a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '-') {
				return "", invalid
			}
		}
	}
	lastLabel := labels[len(labels)-1]
	numeric := true
	for i := 0; i < len(lastLabel); i += 1 {
		if lastLabel[i] < '0' || '9' < lastLabel[i] {
			numeric = false
		}
	}
	if numeric {
		return "", invalid
	}
	return domain, nil
}

// The name a domain's challenge is published at.
func NetworkDomainChallengeName(domain string) string {
	return NetworkDomainChallengeLabel + "." + domain
}

// A new challenge: 256 random bits, base64url with no padding, 43 characters.
func newNetworkDomainChallenge() (string, error) {
	b := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// Whether one of a lookup's TXT values is the challenge, compared in constant
// time. Surrounding space is not part of a value.
func networkDomainChallengeFound(txtValues []string, challenge string) bool {
	found := false
	for _, txtValue := range txtValues {
		if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(txtValue)), []byte(challenge)) == 1 {
			found = true
		}
	}
	return found
}

// A domain as network_domain holds it.
type NetworkDomain struct {
	NetworkId server.Id
	Domain    string
	Challenge string
	// when the network proved it; nil while unverified
	VerifyTime *time.Time
	// the newest lookup
	CheckTime *time.Time
	// the start of the failing run, or of an unverified domain's 7 days
	FailTime *time.Time
}

const networkDomainColumns = `network_id, domain, challenge, verify_time, check_time, fail_time`

func scanNetworkDomain(result server.PgResult) *NetworkDomain {
	domain := &NetworkDomain{}
	server.Raise(result.Scan(
		&domain.NetworkId,
		&domain.Domain,
		&domain.Challenge,
		&domain.VerifyTime,
		&domain.CheckTime,
		&domain.FailTime,
	))
	return domain
}

// The network's row for domain, nil for none.
func getNetworkDomainWithQuery(ctx context.Context, query server.PgCanQuery, networkId server.Id, domain string, forUpdate bool) (networkDomain *NetworkDomain) {
	sql := `SELECT ` + networkDomainColumns + ` FROM network_domain WHERE network_id = $1 AND domain = $2`
	if forUpdate {
		sql += ` FOR UPDATE`
	}
	result, err := query.Query(ctx, sql, networkId, domain)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			networkDomain = scanNetworkDomain(result)
		}
	})
	return
}

// The network's row for domain, nil for none or in a binary without MG6.
func GetNetworkDomain(ctx context.Context, networkId server.Id, domain string) (networkDomain *NetworkDomain) {
	if !server.SidecarMg6Linked() {
		return nil
	}
	server.Db(ctx, func(conn server.PgConn) {
		networkDomain = getNetworkDomainWithQuery(ctx, conn, networkId, domain, false)
	})
	return
}

// The verified domains of the network, in name order. Empty in a binary
// without MG6.
func GetNetworkVerifiedDomains(ctx context.Context, networkId server.Id) []string {
	domains := []string{}
	if !server.SidecarMg6Linked() {
		return domains
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT domain
				FROM network_domain
				WHERE network_id = $1 AND verify_time IS NOT NULL
				ORDER BY domain
			`,
			networkId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var domain string
				server.Raise(result.Scan(&domain))
				domains = append(domains, domain)
			}
		})
	})
	return domains
}

// Whether the session's caller stands in its network with a role that holds
// permission: a member that is not pending, by the credential's user, which
// every team credential names (TEAM-M6-02). A personal network has no members,
// so its owner is refused: the routes that ask are Team routes.
func teamCallerPermitsWithQuery(ctx context.Context, query server.PgCanQuery, clientSession *session.ClientSession, permission RolePermission) bool {
	byJwt := clientSession.ByJwt
	if byJwt == nil || byJwt.ClientId != nil {
		return false
	}
	caller := getNetworkMemberOfUserWithQuery(ctx, query, byJwt.NetworkId, byJwt.UserId)
	return caller != nil && !caller.Pending && caller.Role.Permits(permission)
}

// The actions and target of a domain's admin events. The target carries no id.
const (
	AdminEventActionDomainAdd    = "domain.add"
	AdminEventActionDomainVerify = "domain.verify"
	AdminEventActionDomainRemove = "domain.remove"
	// the daily recheck dropped the domain, by the system
	AdminEventActionDomainDrop = "domain.drop"
	AdminEventTargetDomain     = "domain"
)

// The body of the three domain routes.
type NetworkDomainArgs struct {
	Domain string `json:"domain"`
}

type NetworkDomainError struct {
	Message string `json:"message"`
}

// A domain as its network's Owner sees it.
type NetworkDomainState struct {
	Domain string `json:"domain"`
	// the TXT record to publish
	TxtName  string `json:"txt_name"`
	TxtValue string `json:"txt_value"`
	Verified bool   `json:"verified"`
	// when the network proved the domain
	VerifyTime *time.Time `json:"verify_time,omitempty"`
	// the newest lookup
	CheckTime *time.Time `json:"check_time,omitempty"`
	// a verified domain's failing run began here, and it is dropped 7 days
	// on unless a check finds the record; an unverified domain is dropped 7
	// days after this
	FailTime *time.Time `json:"fail_time,omitempty"`
}

type NetworkDomainResult struct {
	Domain *NetworkDomainState `json:"domain,omitempty"`
	// domain-remove: whether a row went
	Removed bool                `json:"removed,omitempty"`
	Error   *NetworkDomainError `json:"error,omitempty"`
}

func networkDomainStateOf(networkDomain *NetworkDomain) *NetworkDomainState {
	return &NetworkDomainState{
		Domain:     networkDomain.Domain,
		TxtName:    NetworkDomainChallengeName(networkDomain.Domain),
		TxtValue:   networkDomain.Challenge,
		Verified:   networkDomain.VerifyTime != nil,
		VerifyTime: networkDomain.VerifyTime,
		CheckTime:  networkDomain.CheckTime,
		FailTime:   networkDomain.FailTime,
	}
}

const networkDomainsRefusedMessage = "Only the team's Owners may change its domains."

// The checks every domain route makes before its own work: MG6, a network
// credential, the team tier, and a domain that parses. A nil result and error
// pass.
func (self *networkDomains) admit(args *NetworkDomainArgs, clientSession *session.ClientSession) (domain string, result *NetworkDomainResult, returnErr error) {
	if !server.SidecarMg6Linked() {
		return "", nil, fmt.Errorf("%d %s", http.StatusServiceUnavailable, networkTeamsUnavailableMessage)
	}
	byJwt := clientSession.ByJwt
	if byJwt == nil || byJwt.ClientId != nil {
		return "", nil, fmt.Errorf("%d %s", http.StatusForbidden, networkDomainsRefusedMessage)
	}
	if err := self.requireTeamTier(clientSession.Ctx, byJwt.NetworkId); err != nil {
		return "", nil, err
	}
	domain, err := NormalizeNetworkDomain(args.Domain)
	if err != nil {
		return "", &NetworkDomainResult{Error: &NetworkDomainError{Message: err.Error()}}, nil
	}
	return domain, nil, nil
}

// The lock that orders every claim of one domain across networks, held to the
// end of the caller's transaction.
func lockNetworkDomainInTx(ctx context.Context, tx server.PgTx, domain string) {
	server.RaisePgResult(tx.Exec(
		ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended('network_domain:' || $1::text, 0))`,
		domain,
	))
}

// POST /network/domain-add (§7.6): adds the domain to the caller's team,
// unverified, and answers the TXT record that proves it. A domain the team
// already holds answers its record again.
func AddNetworkDomain(args *NetworkDomainArgs, clientSession *session.ClientSession) (*NetworkDomainResult, error) {
	return newNetworkDomains().add(args, clientSession)
}

func (self *networkDomains) add(args *NetworkDomainArgs, clientSession *session.ClientSession) (*NetworkDomainResult, error) {
	domain, result, err := self.admit(args, clientSession)
	if result != nil || err != nil {
		return result, err
	}
	ctx := clientSession.Ctx
	networkId := clientSession.ByJwt.NetworkId
	challenge, err := newNetworkDomainChallenge()
	if err != nil {
		return nil, err
	}
	now := self.now()
	refused := false
	var networkDomain *NetworkDomain
	var message string
	server.Tx(ctx, func(tx server.PgTx) {
		refused, networkDomain, message = false, nil, ""
		if !teamCallerPermitsWithQuery(ctx, tx, clientSession, RolePermissionOwner) {
			refused = true
			return
		}
		// the network's rows change one at a time, so the limit holds
		server.RaisePgResult(tx.Exec(
			ctx,
			`SELECT pg_advisory_xact_lock(hashtextextended('network_domain_network:' || $1::uuid::text, 0))`,
			networkId,
		))
		if networkDomain = getNetworkDomainWithQuery(ctx, tx, networkId, domain, false); networkDomain != nil {
			return
		}
		var count int
		server.Raise(tx.QueryRow(ctx, `SELECT COUNT(*) FROM network_domain WHERE network_id = $1`, networkId).Scan(&count))
		if self.settings.DomainLimit <= count {
			message = fmt.Sprintf("A network holds at most %d domains.", max(self.settings.DomainLimit, 0))
			return
		}
		// fail_time starts the unverified row's 7 days
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO network_domain (network_id, domain, challenge, fail_time)
				VALUES ($1, $2, $3, $4)
			`,
			networkId,
			domain,
			challenge,
			now,
		))
		networkDomain = getNetworkDomainWithQuery(ctx, tx, networkId, domain, false)
		AddAdminEventInTx(ctx, tx, NewSessionAdminEvent(clientSession, AdminEventActionDomainAdd, AdminEventTargetDomain, nil))
		// a new domain owes Owners and Admins a notice (§7.8)
		oweSecurityNetworkAuthorityNoticeInTx(ctx, tx, networkId, &SecurityNetworkAuthorityChange{
			ChangeTime: now,
			Action:     SecurityNetworkAuthorityDomain,
			Via:        securityNoticeVia(ctx),
		})
	}, server.TxReadCommitted)
	switch {
	case refused:
		return nil, fmt.Errorf("%d %s", http.StatusForbidden, networkDomainsRefusedMessage)
	case message != "":
		return &NetworkDomainResult{Error: &NetworkDomainError{Message: message}}, nil
	}
	return &NetworkDomainResult{Domain: networkDomainStateOf(networkDomain)}, nil
}

// What a claim of a verified domain answers when another network holds it. It
// is said only to a caller whose TXT record was found, which controls the
// domain's DNS.
const networkDomainHeldMessage = "Another network has verified this domain."

// POST /network/domain-verify (§7.6): looks the domain's TXT record up and,
// when it holds the challenge, marks the domain verified, unless another
// network holds it. A record not found answers the domain unverified with
// error.
func VerifyNetworkDomain(args *NetworkDomainArgs, clientSession *session.ClientSession) (*NetworkDomainResult, error) {
	return newNetworkDomains().verify(args, clientSession)
}

func (self *networkDomains) verify(args *NetworkDomainArgs, clientSession *session.ClientSession) (*NetworkDomainResult, error) {
	domain, result, err := self.admit(args, clientSession)
	if result != nil || err != nil {
		return result, err
	}
	refuse := func(message string, networkDomain *NetworkDomain) (*NetworkDomainResult, error) {
		result := &NetworkDomainResult{Error: &NetworkDomainError{Message: message}}
		if networkDomain != nil {
			result.Domain = networkDomainStateOf(networkDomain)
		}
		return result, nil
	}
	ctx := clientSession.Ctx
	networkId := clientSession.ByJwt.NetworkId
	now := self.now()

	// the row, and this lookup's turn taken on it, before any lookup
	refused, throttled := false, false
	var networkDomain *NetworkDomain
	server.Tx(ctx, func(tx server.PgTx) {
		refused, throttled, networkDomain = false, false, nil
		if !teamCallerPermitsWithQuery(ctx, tx, clientSession, RolePermissionOwner) {
			refused = true
			return
		}
		networkDomain = getNetworkDomainWithQuery(ctx, tx, networkId, domain, true)
		if networkDomain == nil || networkDomain.VerifyTime != nil {
			return
		}
		if networkDomain.CheckTime != nil && now.Sub(*networkDomain.CheckTime) < self.settings.VerifyInterval {
			throttled = true
			return
		}
		server.RaisePgResult(tx.Exec(
			ctx,
			`UPDATE network_domain SET check_time = $3 WHERE network_id = $1 AND domain = $2`,
			networkId,
			domain,
			now,
		))
		networkDomain.CheckTime = &now
	}, server.TxReadCommitted)
	switch {
	case refused:
		return nil, fmt.Errorf("%d %s", http.StatusForbidden, networkDomainsRefusedMessage)
	case networkDomain == nil:
		return refuse("Add the domain first.", nil)
	case networkDomain.VerifyTime != nil:
		return &NetworkDomainResult{Domain: networkDomainStateOf(networkDomain)}, nil
	case throttled:
		return refuse("The domain was checked a moment ago. Try again shortly.", networkDomain)
	}

	// the lookup, holding no connection
	found := self.lookup(ctx, networkDomain)
	if !found {
		return refuse("The TXT record was not found. DNS changes can take a while to appear.", networkDomain)
	}

	held, gone := false, false
	server.Tx(ctx, func(tx server.PgTx) {
		held, gone = false, false
		lockNetworkDomainInTx(ctx, tx, domain)
		current := getNetworkDomainWithQuery(ctx, tx, networkId, domain, true)
		if current == nil || current.Challenge != networkDomain.Challenge {
			// removed, or removed and added again, while the lookup ran
			gone = true
			return
		}
		if current.VerifyTime != nil {
			networkDomain = current
			return
		}
		server.Raise(tx.QueryRow(
			ctx,
			`
				SELECT EXISTS (
					SELECT 1 FROM network_domain
					WHERE domain = $1 AND verify_time IS NOT NULL AND network_id <> $2
				)
			`,
			domain,
			networkId,
		).Scan(&held))
		if held {
			return
		}
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE network_domain
				SET verify_time = $3, check_time = $3, fail_time = NULL
				WHERE network_id = $1 AND domain = $2
			`,
			networkId,
			domain,
			now,
		))
		networkDomain = getNetworkDomainWithQuery(ctx, tx, networkId, domain, false)
		AddAdminEventInTx(ctx, tx, NewSessionAdminEvent(clientSession, AdminEventActionDomainVerify, AdminEventTargetDomain, nil))
		// from here the domain's sign-ins route to the team's IdP (§7.8)
		oweSecurityNetworkAuthorityNoticeInTx(ctx, tx, networkId, &SecurityNetworkAuthorityChange{
			ChangeTime: now,
			Action:     SecurityNetworkAuthorityDomainVerified,
			Via:        securityNoticeVia(ctx),
		})
	}, server.TxReadCommitted)
	switch {
	case gone:
		return refuse("Add the domain first.", nil)
	case held:
		return refuse(networkDomainHeldMessage, networkDomain)
	}
	return &NetworkDomainResult{Domain: networkDomainStateOf(networkDomain)}, nil
}

// One lookup of the domain's challenge, bounded by LookupTimeout. A lookup
// error is a check that did not find it.
func (self *networkDomains) lookup(ctx context.Context, networkDomain *NetworkDomain) bool {
	lookupCtx, cancel := context.WithTimeout(ctx, self.settings.LookupTimeout)
	defer cancel()
	txtValues, err := self.resolver().LookupTxt(lookupCtx, NetworkDomainChallengeName(networkDomain.Domain))
	if err != nil {
		return false
	}
	return networkDomainChallengeFound(txtValues, networkDomain.Challenge)
}

// POST /network/domain-remove (§7.6): removes the domain from the caller's
// team, verified or not, so discovery stops routing it at once.
func RemoveNetworkDomain(args *NetworkDomainArgs, clientSession *session.ClientSession) (*NetworkDomainResult, error) {
	return newNetworkDomains().remove(args, clientSession)
}

func (self *networkDomains) remove(args *NetworkDomainArgs, clientSession *session.ClientSession) (*NetworkDomainResult, error) {
	domain, result, err := self.admit(args, clientSession)
	if result != nil || err != nil {
		return result, err
	}
	ctx := clientSession.Ctx
	networkId := clientSession.ByJwt.NetworkId
	refused, removed := false, false
	server.Tx(ctx, func(tx server.PgTx) {
		refused, removed = false, false
		if !teamCallerPermitsWithQuery(ctx, tx, clientSession, RolePermissionOwner) {
			refused = true
			return
		}
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`DELETE FROM network_domain WHERE network_id = $1 AND domain = $2`,
			networkId,
			domain,
		))
		if tag.RowsAffected() == 0 {
			return
		}
		removed = true
		AddAdminEventInTx(ctx, tx, NewSessionAdminEvent(clientSession, AdminEventActionDomainRemove, AdminEventTargetDomain, nil))
	}, server.TxReadCommitted)
	if refused {
		return nil, fmt.Errorf("%d %s", http.StatusForbidden, networkDomainsRefusedMessage)
	}
	return &NetworkDomainResult{Removed: removed}, nil
}

// The IdP that sign-ins of the address's domain route to (§7.6): the oldest
// IdP of the team that holds the domain verified. Nil for anything else, with
// nothing to tell the cases apart: an address that does not parse, a domain no
// network added, one added and unverified, one dropped, and a verified one
// whose team has no IdP. Only the domain is read, never a user, so the answer
// says nothing of whether the address exists.
func NetworkSsoIdpOfEmail(ctx context.Context, email string) (idpId *server.Id) {
	if !server.SidecarMg6Linked() {
		return nil
	}
	// a domain that does not parse still asks, so it costs what any other does
	domain := ""
	if at := strings.LastIndexByte(email, '@'); 0 < at {
		if normalDomain, err := NormalizeNetworkDomain(email[at+1:]); err == nil {
			domain = normalDomain
		}
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT network_idp.idp_id
				FROM network_domain
				INNER JOIN network_idp ON network_idp.network_id = network_domain.network_id
				WHERE network_domain.domain = $1 AND network_domain.verify_time IS NOT NULL
				ORDER BY network_idp.idp_id
				LIMIT 1
			`,
			domain,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				var id server.Id
				server.Raise(result.Scan(&id))
				idpId = &id
			}
		})
	})
	return
}

// Where a recheck pass stopped: the last row it examined.
type NetworkDomainCursor struct {
	NetworkId server.Id `json:"network_id"`
	Domain    string    `json:"domain"`
}

type NetworkDomainRecheckResult struct {
	// verified domains looked up
	CheckedCount int `json:"checked_count"`
	// of those, lookups that did not find the challenge
	FailedCount int `json:"failed_count"`
	// rows dropped: failing runs that reached DropAfter, and unverified
	// domains past it
	DroppedCount int `json:"dropped_count"`
	// set when the pass stopped at its batch bound, for the next pass
	After *NetworkDomainCursor `json:"after,omitempty"`
}

// One pass of the daily recheck at now, from after (nil for the start), over
// at most RecheckBatchCount rows: each verified domain whose newest check is a
// RecheckInterval old is looked up again, and each unverified domain past its
// DropAfter goes. Empty in a binary without MG6.
func RecheckNetworkDomains(ctx context.Context, now time.Time, after *NetworkDomainCursor, settings *NetworkDomainSettings) *NetworkDomainRecheckResult {
	domains := newNetworkDomains()
	domains.settings = settings
	return domains.recheck(ctx, now, after)
}

func (self *networkDomains) recheck(ctx context.Context, now time.Time, after *NetworkDomainCursor) *NetworkDomainRecheckResult {
	recheckResult := &NetworkDomainRecheckResult{}
	if !server.SidecarMg6Linked() {
		return recheckResult
	}
	limit := max(self.settings.RecheckBatchCount, 1)
	var afterNetworkId *server.Id
	afterDomain := ""
	if after != nil {
		afterNetworkId, afterDomain = &after.NetworkId, after.Domain
	}
	dueDomains := []*NetworkDomain{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT `+networkDomainColumns+`
				FROM network_domain
				WHERE
					($1::uuid IS NULL OR (network_id, domain) > ($1::uuid, $2::varchar)) AND
					(
						(verify_time IS NOT NULL AND (check_time IS NULL OR check_time <= $3)) OR
						(verify_time IS NULL AND (fail_time IS NULL OR fail_time <= $4))
					)
				ORDER BY network_id, domain
				LIMIT $5
			`,
			afterNetworkId,
			afterDomain,
			now.Add(-self.settings.RecheckInterval),
			now.Add(-self.settings.DropAfter),
			limit,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				dueDomains = append(dueDomains, scanNetworkDomain(result))
			}
		})
	})
	for _, dueDomain := range dueDomains {
		if ctx.Err() != nil {
			// the pass is ending: what is left is the next pass's, unjudged
			return recheckResult
		}
		cursor := &NetworkDomainCursor{NetworkId: dueDomain.NetworkId, Domain: dueDomain.Domain}
		if dueDomain.VerifyTime == nil {
			if self.settle(ctx, dueDomain, now, false) {
				recheckResult.DroppedCount += 1
			}
			recheckResult.After = cursor
			continue
		}
		found := self.lookup(ctx, dueDomain)
		if ctx.Err() != nil {
			// a lookup the pass's own end cut short judges nothing
			return recheckResult
		}
		recheckResult.CheckedCount += 1
		if !found {
			recheckResult.FailedCount += 1
		}
		if self.settle(ctx, dueDomain, now, found) {
			recheckResult.DroppedCount += 1
		}
		recheckResult.After = cursor
	}
	if len(dueDomains) < limit {
		recheckResult.After = nil
	}
	return recheckResult
}

// Records one check of the row as the pass read it, and answers whether the
// row was dropped. A row that changed since, removed or added again, is left
// as it is.
func (self *networkDomains) settle(ctx context.Context, examined *NetworkDomain, now time.Time, found bool) (dropped bool) {
	server.Tx(ctx, func(tx server.PgTx) {
		dropped = false
		current := getNetworkDomainWithQuery(ctx, tx, examined.NetworkId, examined.Domain, true)
		if current == nil || current.Challenge != examined.Challenge || (current.VerifyTime == nil) != (examined.VerifyTime == nil) {
			return
		}
		drop := func() {
			server.RaisePgResult(tx.Exec(
				ctx,
				`DELETE FROM network_domain WHERE network_id = $1 AND domain = $2`,
				current.NetworkId,
				current.Domain,
			))
			AddAdminEventInTx(ctx, tx, &AdminEvent{
				NetworkId:  current.NetworkId,
				ActorKind:  AdminEventActorSystem,
				Action:     AdminEventActionDomainDrop,
				TargetKind: AdminEventTargetDomain,
				Result:     AdminEventResultOk,
			})
			dropped = true
		}
		switch {
		case current.VerifyTime == nil:
			// never proven: gone once its 7 days are
			if current.FailTime == nil || self.settings.DropAfter <= now.Sub(*current.FailTime) {
				drop()
			}
		case found:
			server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE network_domain SET check_time = $3, fail_time = NULL WHERE network_id = $1 AND domain = $2`,
				current.NetworkId,
				current.Domain,
				now,
			))
		case current.FailTime != nil && self.settings.DropAfter <= now.Sub(*current.FailTime):
			drop()
		default:
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					UPDATE network_domain
					SET check_time = $3, fail_time = COALESCE(fail_time, $3)
					WHERE network_id = $1 AND domain = $2
				`,
				current.NetworkId,
				current.Domain,
				now,
			))
		}
	}, server.TxReadCommitted)
	return
}
