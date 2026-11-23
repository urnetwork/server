// Security notices (owner decision 2026-10-29): an email to the account's
// sign-in addresses whenever its payout wallet or its sign-in methods change,
// or an API key is created or deleted (security_notice_api_key.go), so an
// owner notices a change they did not make. In May 2026 an attacker's wallet
// replaced the payout wallet of 1,459 networks and no owner noticed.
//
// Each notice is owed in the transaction of the change it reports, through the
// account message outbox (account_message_outbox_model.go), so it exists exactly
// when the change commits, and the outbox's delivery task sends it after the
// commit and retries a failed send. Owing a notice never fails the change: it
// runs under a savepoint (guardSecurityNoticeInTx), and a failure there rolls
// back the notice alone and is counted. Only a failure that reruns or ends the
// whole transaction anyway (a serialization failure, a deadlock, the caller's
// stop) is left to the change's own transaction handling.
//
// Recipients are sign-in email addresses. A personal network's notices go to
// its owner's, the admin's (securityNoticeRecipientsInTx). A team's owner of
// record never signs in (D-TEAM-2), so a team's notices go to members by role:
// a payout or subnet wallet change to each member whose role holds billing, its
// Owners and Billing admins (securityNoticeBillingRecipientsInTx, QD-1217), and
// the key and authority notices to its Owners and Admins
// (securityNoticeAuthorityRecipientsInTx). A phone number or a wallet is no
// email: an account that signs in only with those gets no notice, and no notice
// goes by SMS (AWS approved SMS for verification codes, password resets and the
// password-changed notice only).
//
// Coalescing: a change owes its notice due the settings' CoalesceWindow later.
// Another change of the same kind for the same recipient while that notice is
// still waiting (not yet due, never attempted) joins it, so a burst of changes is
// one email that lists them in order. Joining never moves the due time, so the
// first email goes out at most a window after the first change (plus the
// delivery task's one-minute interval) however long the burst lasts, and a
// change after it is due owes a new one.
//
// Wallets, email addresses and phone numbers are masked before they are stored,
// so the outbox never holds a full address, and no notice carries an IP address.
package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// The security notice templates. The controller's templates of the same names
// render them; the password-changed notice is AuthPasswordSetTemplateName.
const (
	SecurityPayoutWalletTemplateName = "security_payout_wallet_changed"
	SecuritySignInTemplateName       = "security_sign_in_changed"
	SecurityApiKeyTemplateName       = "security_api_key_changed"
)

// Tunables of the account security notices (QD-702). The notices read them
// where they apply, from DefaultSecurityNoticeSettings.
type SecurityNoticeSettings struct {
	// How long a new notice waits before it is due, for more changes of its
	// kind to join it: 1 minute, so a burst of changes is one email that still
	// goes out about a minute after the burst's first change. Non-positive
	// makes each notice due at once, so no change joins another's.
	CoalesceWindow time.Duration
	// The most changes one notice lists: 10, a list still read at a glance. A
	// later change takes the last slot and the one it replaces is counted as
	// not listed, so the list always ends with the latest state. Below 1, a
	// notice lists the latest change alone.
	ChangeMaxCount int
	// The most sign-in email addresses one change notifies: 8, room for every
	// address one account signs in with. The addresses kept are the first in
	// sort order. Non-positive bounds nothing.
	RecipientMaxCount int
	// The shortest time between two default-level reports of notices that
	// could not be owed: 1 minute, so a failing outbox shows in the log
	// without flooding it; each report counts the ones suppressed since the
	// last. Non-positive reports every one.
	FailureReportInterval time.Duration
	// The characters of a wallet address a notice shows at each end of its
	// mask: 4, enough for a person to tell its own wallet from another.
	// Non-positive shows none of any address.
	WalletMaskEndLength int
	// The shortest value a wallet mask shows any of: 12 characters, under
	// which a value is no wallet address. A value no longer than both ends
	// together shows none either, so a mask never shows a whole address.
	WalletMaskMinLength int
	// The digits of a phone number a notice shows at its end: 2. Non-positive
	// shows none.
	PhoneMaskEndDigitCount int
	// The fewest digits a phone number has for a notice to show any: 6. A
	// number no longer than the end shown shows none either.
	PhoneMaskMinDigitCount int
}

// The defaults, a fresh value each call.
func DefaultSecurityNoticeSettings() *SecurityNoticeSettings {
	return &SecurityNoticeSettings{
		CoalesceWindow:         1 * time.Minute,
		ChangeMaxCount:         10,
		RecipientMaxCount:      8,
		FailureReportInterval:  1 * time.Minute,
		WalletMaskEndLength:    4,
		WalletMaskMinLength:    12,
		PhoneMaskEndDigitCount: 2,
		PhoneMaskMinDigitCount: 6,
	}
}

// The notice label of the metric.
const (
	SecurityNoticePayoutWallet = "payout_wallet"
	SecurityNoticeSubnetWallet = "subnet_wallet"
	SecurityNoticeSignIn       = "sign_in"
	SecurityNoticePassword     = "password"
	SecurityNoticeApiKey       = "api_key"
)

// The result label of the metric.
const (
	// a new message is owed to at least one recipient
	securityNoticeQueued = "queued"
	// every recipient's waiting notice took the change
	securityNoticeCoalesced = "coalesced"
	// the account has no sign-in email, so nothing is owed
	securityNoticeNoEmail = "no_email"
	// the notice could not be owed; the change went through without it
	securityNoticeFailed = "failed"
)

// Counts security notices by notice and result, once per change.
var securityNoticeCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "account_message",
		Name:      "security_notices_total",
		Help:      "Security notices owed for payout wallet, sign-in method, password and API key changes, by notice and result (queued, coalesced, no_email, failed)",
	},
	[]string{"notice", "result"},
)

// Registers the notice metric with the default registry.
func init() {
	prometheus.MustRegister(securityNoticeCounter)
}

// How a change was made, as a notice says it: the X-UR-ClientInfo device types
// of the apps, the website, or an API key.
const (
	SecurityNoticeViaAndroid = "android"
	SecurityNoticeViaIos     = "ios"
	SecurityNoticeViaMacos   = "macos"
	SecurityNoticeViaWindows = "windows"
	SecurityNoticeViaLinux   = "linux"
	SecurityNoticeViaCli     = "cli"
	SecurityNoticeViaWeb     = "web"
	SecurityNoticeViaApiKey  = "api_key"
)

// Where payouts go, as a wallet notice names it.
const (
	// the network's USDC payout wallet (payout_wallet)
	SecurityWalletDestinationUsdc = "usdc_payout"
	// the Bittensor wallet the network's subnet pool payouts are claimable by
	// (st_wallet)
	SecurityWalletDestinationSubnetNetwork = "subnet_network"
	// the Bittensor wallet of one of the network's providers
	// (st_provider_wallet_history), with the epochs when it is a consent
	SecurityWalletDestinationSubnetProvider = "subnet_provider"
	// the Bittensor wallet a network wallet mapping consent gives the network's
	// subnet earnings to, for its epochs
	SecurityWalletDestinationSubnetNetworkConsent = "subnet_network_consent"
	// the Bittensor hotkey a hotkey network delegation gives the network's
	// subnet earnings to, for its epochs
	SecurityWalletDestinationSubnetHotkey = "subnet_hotkey"

	// the hotkey a network's coldkey authorized to claim its Top 200 spot, whose
	// devices sign bindings to it with no person looking
	SecurityWalletDestinationFleetHotkey = "fleet_hotkey"
)

// What happened to a sign-in method.
const (
	SecuritySignInAdded   = "added"
	SecuritySignInRemoved = "removed"
	// replaced in place: a regenerated seedphrase
	SecuritySignInReplaced = "replaced"
)

// The sign-in methods, as auth_types in GET /network/user names them.
const (
	SecuritySignInMethodEmail      = "email"
	SecuritySignInMethodPhone      = "phone"
	SecuritySignInMethodApple      = "apple"
	SecuritySignInMethodGoogle     = "google"
	SecuritySignInMethodSolana     = "solana"
	SecuritySignInMethodBittensor  = "bittensor"
	SecuritySignInMethodSeedphrase = "seedphrase"
)

// One change of where payouts go. Wallets are masked (MaskWalletAddress).
type SecurityWalletChange struct {
	ChangeTime time.Time `json:"change_time"`
	// one of SecurityWalletDestination*
	Destination string `json:"destination"`
	// the chain (SOL, MATIC, TAO, ...) and masked wallet before and after the
	// change; empty for none
	PreviousChain  string `json:"previous_chain,omitempty"`
	PreviousWallet string `json:"previous_wallet,omitempty"`
	NewChain       string `json:"new_chain,omitempty"`
	NewWallet      string `json:"new_wallet,omitempty"`
	// the first and last epochs a consent covers; zero for none
	FromEpoch    uint64 `json:"from_epoch,omitempty"`
	ThroughEpoch uint64 `json:"through_epoch,omitempty"`
	// one of SecurityNoticeVia*, or "" when unknown
	Via string `json:"via,omitempty"`
}

// The payout wallet notice: the changes it lists, oldest first.
type SecurityWalletNotice struct {
	Changes []*SecurityWalletChange `json:"changes"`
	// changes made between the listed ones and not listed
	MoreCount int `json:"more_count,omitempty"`
}

// Adds a change, keeping the latest change last when the list is full.
func (self *SecurityWalletNotice) add(change *SecurityWalletChange) {
	self.Changes, self.MoreCount = addSecurityNoticeChange(self.Changes, self.MoreCount, change)
}

// One change of a sign-in method. The identity is masked.
type SecuritySignInChange struct {
	ChangeTime time.Time `json:"change_time"`
	// one of SecuritySignIn{Added,Removed,Replaced}
	Action string `json:"action"`
	// one of SecuritySignInMethod*
	Method string `json:"method"`
	// the masked email address, phone number or wallet of the method; empty
	// for a method without one (a seedphrase)
	Identity string `json:"identity,omitempty"`
	// one of SecurityNoticeVia*, or "" when unknown
	Via string `json:"via,omitempty"`
}

// The sign-in method notice: the changes it lists, oldest first.
type SecuritySignInNotice struct {
	Changes []*SecuritySignInChange `json:"changes"`
	// changes made between the listed ones and not listed
	MoreCount int `json:"more_count,omitempty"`
}

// Adds a change, keeping the latest change last when the list is full.
func (self *SecuritySignInNotice) add(change *SecuritySignInChange) {
	self.Changes, self.MoreCount = addSecurityNoticeChange(self.Changes, self.MoreCount, change)
}

// The password-changed notice's fields (AuthPasswordSetTemplateName). A message
// owed before they existed decodes with none.
type PasswordSetNotice struct {
	ChangeTime *time.Time `json:"change_time,omitempty"`
	// one of SecurityNoticeVia*, or "" when unknown
	Via string `json:"via,omitempty"`
}

// Appends the change while the list has room, the settings' ChangeMaxCount. A
// full list keeps its first changes and its last slot takes the latest change,
// so the notice always ends with the current state; the change it replaces is
// counted as not listed.
func addSecurityNoticeChange[C any](changes []C, moreCount int, change C) ([]C, int) {
	changeMaxCount := max(DefaultSecurityNoticeSettings().ChangeMaxCount, 1)
	if len(changes) < changeMaxCount {
		return append(changes, change), moreCount
	}
	changes[len(changes)-1] = change
	return changes, moreCount + 1
}

// The wallet address as a notice shows it: its first and last
// WalletMaskEndLength characters (4) around an ellipsis ("9WzD…AWWM"). A value
// under WalletMaskMinLength characters (12) is no wallet address, and none of it
// is shown.
func MaskWalletAddress(address string) string {
	return maskWalletAddress(address, DefaultSecurityNoticeSettings())
}

// MaskWalletAddress under the given settings. A value no longer than both ends
// together shows none, so no settings show a whole address.
func maskWalletAddress(address string, settings *SecurityNoticeSettings) string {
	runes := []rune(strings.TrimSpace(address))
	if len(runes) == 0 {
		return ""
	}
	endLength := max(settings.WalletMaskEndLength, 0)
	if len(runes) < settings.WalletMaskMinLength || len(runes) <= 2*endLength {
		return "…"
	}
	return string(runes[:endLength]) + "…" + string(runes[len(runes)-endLength:])
}

// An email address as a notice shows it: the first character of the mailbox, an
// ellipsis and the domain ("j…@example.com").
func maskEmailAddress(emailAddress string) string {
	mailbox, domain, ok := strings.Cut(strings.TrimSpace(emailAddress), "@")
	if !ok || mailbox == "" || domain == "" {
		return "…"
	}
	first, _ := firstRune(mailbox)
	return first + "…@" + domain
}

// A phone number as a notice shows it: an ellipsis and its last
// PhoneMaskEndDigitCount digits (2, "…01"), none for a number of fewer than
// PhoneMaskMinDigitCount digits (6), nor for one no longer than the end it
// would show, so no settings show a whole number.
func maskPhoneNumber(phoneNumber string, settings *SecurityNoticeSettings) string {
	digits := []rune{}
	for _, r := range phoneNumber {
		if unicode.IsDigit(r) {
			digits = append(digits, r)
		}
	}
	endDigitCount := max(settings.PhoneMaskEndDigitCount, 0)
	if len(digits) < settings.PhoneMaskMinDigitCount || len(digits) <= endDigitCount {
		return "…"
	}
	return "…" + string(digits[len(digits)-endDigitCount:])
}

func firstRune(value string) (string, bool) {
	for _, r := range value {
		return string(r), true
	}
	return "", false
}

// The masked identity of a sign-in method's user auth: an email address or a
// phone number, whichever it normalizes to.
func maskUserAuth(userAuth string) string {
	normalUserAuth, userAuthType := NormalUserAuth(userAuth)
	switch userAuthType {
	case UserAuthTypeEmail:
		return maskEmailAddress(normalUserAuth)
	case UserAuthTypePhone:
		return maskPhoneNumber(normalUserAuth, DefaultSecurityNoticeSettings())
	default:
		return ""
	}
}

// How the request of ctx made its change, or "" when unknown or not a request.
func securityNoticeVia(ctx context.Context) string {
	channel, ok := session.RequestChannelFromContext(ctx)
	if !ok {
		return ""
	}
	return securityNoticeViaOf(channel)
}

// An API key first, then the device type the client declared, then a browser
// call from the operator's website.
func securityNoticeViaOf(channel session.RequestChannel) string {
	if channel.ApiKey {
		return SecurityNoticeViaApiKey
	}
	switch channel.DeviceType {
	case SecurityNoticeViaAndroid,
		SecurityNoticeViaIos,
		SecurityNoticeViaMacos,
		SecurityNoticeViaWindows,
		SecurityNoticeViaLinux,
		SecurityNoticeViaCli,
		SecurityNoticeViaWeb:
		return channel.DeviceType
	}
	if securityNoticeWebsiteOrigin(channel.BrowserOrigin) {
		return SecurityNoticeViaWeb
	}
	return ""
}

// Whether a browser origin is one of the operator's websites: https on ur.io or
// bringyour.com, or a subdomain of either.
func securityNoticeWebsiteOrigin(origin string) bool {
	if origin == "" {
		return false
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Scheme != "https" {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	for _, site := range []string{"ur.io", "bringyour.com"} {
		if host == site || strings.HasSuffix(host, "."+site) {
			return true
		}
	}
	return false
}

// The sign-in email addresses of a network's admin, read in the caller's
// transaction:
//   - the legacy network_user.user_auth, the address every other account email
//     goes to, when it is an email;
//   - each verified email sign-in (network_user_auth_password);
//   - the email of each Apple or Google sign-in.
const securityNoticeRecipientsSql = `
	SELECT network_user.user_auth
	FROM network
	INNER JOIN network_user ON network_user.user_id = network.admin_user_id
	WHERE
		network.network_id = $1 AND
		network_user.user_auth IS NOT NULL
	UNION ALL
	SELECT network_user_auth_password.user_auth
	FROM network
	INNER JOIN network_user_auth_password ON network_user_auth_password.user_id = network.admin_user_id
	WHERE
		network.network_id = $1 AND
		network_user_auth_password.auth_type = 'email' AND
		network_user_auth_password.verified
	UNION ALL
	SELECT network_user_auth_sso.user_auth
	FROM network
	INNER JOIN network_user_auth_sso ON network_user_auth_sso.user_id = network.admin_user_id
	WHERE network.network_id = $1
`

// The network's security notice recipients: its admin's sign-in email
// addresses (securityNoticeEmails). A phone number or a wallet is no email, so
// an admin who signs in only with those, or a network that is gone, has none.
// In a team the admin is the owner of record, which has no sign-in, so a notice
// that can reach a team takes its recipients from a rule that tells a team's
// members by role (securityNoticeBillingRecipientsInTx,
// securityNoticeAuthorityRecipientsInTx).
func securityNoticeRecipientsInTx(ctx context.Context, tx server.PgTx, networkId server.Id) []string {
	userAuths := []string{}
	result, err := tx.Query(ctx, securityNoticeRecipientsSql, networkId)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var userAuth string
			server.Raise(result.Scan(&userAuth))
			userAuths = append(userAuths, userAuth)
		}
	})
	return securityNoticeEmails(userAuths)
}

// The email addresses among user auths, normalized, distinct and sorted, at
// most the settings' RecipientMaxCount.
func securityNoticeEmails(userAuths []string) []string {
	emails := []string{}
	for _, userAuth := range userAuths {
		normalUserAuth, userAuthType := NormalUserAuth(userAuth)
		if userAuthType == UserAuthTypeEmail && normalUserAuth != "" && !slices.Contains(emails, normalUserAuth) {
			emails = append(emails, normalUserAuth)
		}
	}
	slices.Sort(emails)
	if recipientMaxCount := DefaultSecurityNoticeSettings().RecipientMaxCount; 0 < recipientMaxCount && recipientMaxCount < len(emails) {
		emails = emails[:recipientMaxCount]
	}
	return emails
}

// Whether the network is a team, read in tx: a network_team row, which only
// team-create writes. False in a binary that does not link MG6, which has no
// teams.
func securityNoticeTeamInTx(ctx context.Context, tx server.PgTx, networkId server.Id) (team bool) {
	if !server.SidecarMg6Linked() {
		return false
	}
	server.Raise(tx.QueryRow(
		ctx,
		`SELECT EXISTS (SELECT 1 FROM network_team WHERE network_id = $1)`,
		networkId,
	).Scan(&team))
	return
}

// The sign-in email addresses of a team's members whose role is one of $2,
// read in the caller's transaction as securityNoticeRecipientsSql reads an
// admin's, for each member's user. A removed member has no row, and a JIT
// pending one (§7.6) holds no role's reach, so neither is told.
const securityNoticeMemberRecipientsSql = `
	WITH members AS (
		SELECT user_id
		FROM network_member
		WHERE network_id = $1 AND NOT pending AND role = ANY($2)
	)
	SELECT network_user.user_auth
	FROM members
	INNER JOIN network_user ON network_user.user_id = members.user_id
	WHERE network_user.user_auth IS NOT NULL
	UNION ALL
	SELECT network_user_auth_password.user_auth
	FROM members
	INNER JOIN network_user_auth_password ON network_user_auth_password.user_id = members.user_id
	WHERE
		network_user_auth_password.auth_type = 'email' AND
		network_user_auth_password.verified
	UNION ALL
	SELECT network_user_auth_sso.user_auth
	FROM members
	INNER JOIN network_user_auth_sso ON network_user_auth_sso.user_id = members.user_id
`

// The security notice recipients among a team's standing, not pending members
// whose role is one of roles: their member users' sign-in email addresses
// (securityNoticeEmails).
func securityNoticeMemberRecipientsInTx(ctx context.Context, tx server.PgTx, networkId server.Id, roles []NetworkMemberRole) []string {
	roleNames := []string{}
	for _, role := range roles {
		roleNames = append(roleNames, string(role))
	}
	userAuths := []string{}
	result, err := tx.Query(ctx, securityNoticeMemberRecipientsSql, networkId, roleNames)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var userAuth string
			server.Raise(result.Scan(&userAuth))
			userAuths = append(userAuths, userAuth)
		}
	})
	return securityNoticeEmails(userAuths)
}

// The recipients of a notice about where the network's payouts go, read in tx
// (QD-1217): in a personal network its owner's sign-in email addresses
// (securityNoticeRecipientsInTx); in a team each standing, not pending
// member's whose role holds RolePermissionBilling, its Owners and Billing
// admins, at its member user's sign-in email addresses. Those are the members
// who can change where the team's payouts go and the only ones the role table
// lets read it (§7.5), so an Admin, which holds no billing, is not told. A team
// with no such member that has an email has none, and its notice is owed to
// nobody, counted as no_email.
func securityNoticeBillingRecipientsInTx(ctx context.Context, tx server.PgTx, networkId server.Id) []string {
	if !securityNoticeTeamInTx(ctx, tx, networkId) {
		return securityNoticeRecipientsInTx(ctx, tx, networkId)
	}
	billingRoles := []NetworkMemberRole{}
	for role := range networkMemberRolePermissions {
		if role.Permits(RolePermissionBilling) {
			billingRoles = append(billingRoles, role)
		}
	}
	slices.Sort(billingRoles)
	return securityNoticeMemberRecipientsInTx(ctx, tx, networkId, billingRoles)
}

// The network the user administers, or nil.
func securityNoticeNetworkInTx(ctx context.Context, tx server.PgTx, userId server.Id) (networkId *server.Id) {
	result, err := tx.Query(
		ctx,
		`SELECT network_id FROM network WHERE admin_user_id = $1 ORDER BY network_id LIMIT 1`,
		userId,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			var id server.Id
			server.Raise(result.Scan(&id))
			networkId = &id
		}
	})
	return
}

// Runs owe, which owes one change's notices and returns the result to count (""
// counts nothing), under a savepoint, so owing a notice never fails the change
// it reports: a failure in owe rolls back the notice's own writes, counts as
// failed, and the change goes on and commits without it. A failure that reruns
// or ends the whole transaction anyway (a serialization failure or deadlock, or
// the caller's stop) is raised to the change's transaction handling, which
// treats it as it treats any of the change's own statements. Returns whether owe
// completed; a caller keeps what owe read only then.
func guardSecurityNoticeInTx(ctx context.Context, tx server.PgTx, notice string, owe func() string) (completed bool) {
	server.RaisePgResult(tx.Exec(ctx, `SAVEPOINT security_notice`))
	result, failure := func() (result string, failure any) {
		defer func() {
			if recovered := recover(); recovered != nil {
				failure = recovered
			}
		}()
		result = owe()
		if fault, ok := ctx.Value(securityNoticeFaultKey{}).(func(notice string, result string)); ok {
			fault(notice, result)
		}
		return
	}()
	if failure == nil {
		server.RaisePgResult(tx.Exec(ctx, `RELEASE SAVEPOINT security_notice`))
		if result != "" {
			securityNoticeCounter.WithLabelValues(notice, result).Inc()
		}
		return true
	}
	if securityNoticeFailureReruns(ctx, failure) {
		panic(failure)
	}
	server.RaisePgResult(tx.Exec(ctx, `ROLLBACK TO SAVEPOINT security_notice`))
	securityNoticeCounter.WithLabelValues(notice, securityNoticeFailed).Inc()
	reportSecurityNoticeFailure(notice, failure)
	return false
}

// A test fault: when ctx carries a func(notice string, result string), the
// guard calls it after each owe with what owe returned, so a test can fail a
// notice after its writes ran.
type securityNoticeFaultKey struct{}

// Whether a failure while owing a notice reruns or ends the whole transaction
// anyway: the caller stopped, or postgres rolled the transaction back (class
// 40: a serialization failure or a deadlock), which server.Tx reruns.
func securityNoticeFailureReruns(ctx context.Context, failure any) bool {
	if ctx.Err() != nil {
		return true
	}
	err, ok := failure.(error)
	if !ok {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && strings.HasPrefix(pgErr.Code, "40")
}

// The default-level report of notices that could not be owed, at most once each
// FailureReportInterval of the settings. It names the notice and the failure's
// kind only: a postgres detail can carry an address.
var securityNoticeFailureReport struct {
	stateLock       sync.Mutex
	lastReportTime  time.Time
	suppressedCount int
}

func reportSecurityNoticeFailure(notice string, failure any) {
	kind := fmt.Sprintf("%T", failure)
	if err, ok := failure.(error); ok {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			kind = "postgres " + pgErr.Code
		}
	}
	failureReportInterval := DefaultSecurityNoticeSettings().FailureReportInterval
	report := &securityNoticeFailureReport
	report.stateLock.Lock()
	now := time.Now()
	if now.Sub(report.lastReportTime) < failureReportInterval {
		report.suppressedCount += 1
		report.stateLock.Unlock()
		return
	}
	suppressedCount := report.suppressedCount
	report.lastReportTime = now
	report.suppressedCount = 0
	report.stateLock.Unlock()
	glog.Infof(
		"[security]%s notice was not owed (%s); the change went through without it; %d more since the last report\n",
		notice,
		kind,
		suppressedCount,
	)
}

// Owes one change's notice of the template to each recipient: it joins the
// recipient's notice that is still waiting (not yet due and never attempted),
// or adds a notice due the settings' CoalesceWindow from now. `apply` takes the
// waiting notice's fields ("" for a new notice) and returns them with the
// change. Returns the result to count.
func oweSecurityNoticeInTx(
	ctx context.Context,
	tx server.PgTx,
	networkId server.Id,
	recipients []string,
	templateName string,
	apply func(templateJson string) (string, error),
) string {
	if len(recipients) == 0 {
		return securityNoticeNoEmail
	}
	settings := DefaultSecurityNoticeSettings()
	result := securityNoticeCoalesced
	now := server.NowUtc()
	for _, recipient := range recipients {
		var waitingMessageId *server.Id
		var waitingJson string
		// a notice whose lock another change holds is passed over, so a change
		// never waits on another's notice; it owes its own instead
		rows, err := tx.Query(
			ctx,
			`
				SELECT
					message_id,
					template_json::text
				FROM account_message_outbox
				WHERE
					template_name = $1 AND
					network_id = $2 AND
					user_auth = $3 AND
					$4 < deliver_time AND
					attempt_count = 0 AND
					claim_id IS NULL AND
					sent_time IS NULL AND
					abandon_time IS NULL
				ORDER BY deliver_time, message_id
				LIMIT 1
				FOR UPDATE SKIP LOCKED
			`,
			templateName,
			networkId,
			recipient,
			now,
		)
		server.WithPgResult(rows, err, func() {
			if rows.Next() {
				var messageId server.Id
				server.Raise(rows.Scan(&messageId, &waitingJson))
				waitingMessageId = &messageId
			}
		})
		if waitingMessageId != nil {
			templateJson, err := apply(waitingJson)
			server.Raise(err)
			server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE account_message_outbox SET template_json = $2 WHERE message_id = $1`,
				*waitingMessageId,
				templateJson,
			))
			continue
		}
		templateJson, err := apply("")
		server.Raise(err)
		AddAccountMessageInTx(ctx, tx, &AccountMessageArgs{
			// one notice per window: each new notice has its own key, and a
			// rerun transaction rolls back the one it added
			Key:          "security_notice:" + server.NewId().String(),
			NetworkId:    &networkId,
			UserAuth:     recipient,
			TemplateName: templateName,
			TemplateJson: templateJson,
			DeliverAfter: settings.CoalesceWindow,
		})
		result = securityNoticeQueued
	}
	return result
}

// The fields of a notice with the changes added: decoded from templateJson, or
// a new notice when it is "".
func applySecurityNoticeChanges[N any, C any](templateJson string, add func(*N, C), changes []C) (string, error) {
	notice := new(N)
	if templateJson != "" {
		if err := json.Unmarshal([]byte(templateJson), notice); err != nil {
			return "", err
		}
	}
	for _, change := range changes {
		add(notice, change)
	}
	encoded, err := json.Marshal(notice)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

// A wallet before masking: its chain and address.
type securityWallet struct {
	chain   string
	address string
}

// Whether two wallets (nil for none) are the same address.
func sameSecurityWallet(a *securityWallet, b *securityWallet) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.address == b.address
}

// The change from previous to next (nil for none) at the destination, masked;
// nil when they are the same wallet.
func newSecurityWalletChange(
	ctx context.Context,
	destination string,
	previous *securityWallet,
	next *securityWallet,
	fromEpoch uint64,
	throughEpoch uint64,
) *SecurityWalletChange {
	if sameSecurityWallet(previous, next) {
		return nil
	}
	change := &SecurityWalletChange{
		ChangeTime:   server.NowUtc(),
		Destination:  destination,
		FromEpoch:    fromEpoch,
		ThroughEpoch: throughEpoch,
		Via:          securityNoticeVia(ctx),
	}
	if previous != nil {
		change.PreviousChain = securityNoticeChain(previous.chain)
		change.PreviousWallet = MaskWalletAddress(previous.address)
	}
	if next != nil {
		change.NewChain = securityNoticeChain(next.chain)
		change.NewWallet = MaskWalletAddress(next.address)
	}
	return change
}

// A stored chain name as a notice keeps it: the canonical name when it parses
// (stored names vary in case and spelling), else the stored name.
func securityNoticeChain(chain string) string {
	if parsed, err := ParseBlockchain(chain); err == nil {
		return parsed.String()
	}
	return strings.ToUpper(strings.TrimSpace(chain))
}

// Owes the payout wallet notice for the changes `read` returns, to the people
// who hold the network's billing (securityNoticeBillingRecipientsInTx): a
// personal network's owner, a team's Owners and Billing admins. `read` runs
// under the notice's savepoint, after the change, and returns nothing when the
// wallets did not change. Every payout and subnet wallet notice is owed here.
func oweSecurityWalletNoticeInTx(
	ctx context.Context,
	tx server.PgTx,
	networkId server.Id,
	notice string,
	read func() []*SecurityWalletChange,
) {
	guardSecurityNoticeInTx(ctx, tx, notice, func() string {
		changes := []*SecurityWalletChange{}
		for _, change := range read() {
			if change != nil {
				changes = append(changes, change)
			}
		}
		if len(changes) == 0 {
			return ""
		}
		recipients := securityNoticeBillingRecipientsInTx(ctx, tx, networkId)
		return oweSecurityNoticeInTx(ctx, tx, networkId, recipients, SecurityPayoutWalletTemplateName, func(templateJson string) (string, error) {
			return applySecurityNoticeChanges(templateJson, (*SecurityWalletNotice).add, changes)
		})
	})
}

// The network's USDC payout wallet, matching the wallet the payout planner uses
// (GetPayoutWalletId): an active wallet the network owns, or nil.
func securityPayoutWalletInTx(ctx context.Context, tx server.PgTx, networkId server.Id) (wallet *securityWallet) {
	result, err := tx.Query(
		ctx,
		`
			SELECT
				account_wallet.blockchain,
				account_wallet.wallet_address
			FROM payout_wallet
			INNER JOIN account_wallet ON
				account_wallet.wallet_id = payout_wallet.wallet_id AND
				account_wallet.network_id = payout_wallet.network_id AND
				account_wallet.active = true
			WHERE payout_wallet.network_id = $1
		`,
		networkId,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			wallet = &securityWallet{}
			server.Raise(result.Scan(&wallet.chain, &wallet.address))
		}
	})
	return
}

// Watches a network's USDC payout wallet across a change in the caller's
// transaction: the wallet is read before the change and compared after it.
type payoutWalletWatch struct {
	networkId server.Id
	previous  *securityWallet
	// the read before the change succeeded
	watching bool
}

// Reads the payout wallet before a change. A failed read leaves the change
// unwatched; the change goes on.
func watchPayoutWalletInTx(ctx context.Context, tx server.PgTx, networkId server.Id) *payoutWalletWatch {
	watch := &payoutWalletWatch{networkId: networkId}
	var previous *securityWallet
	if guardSecurityNoticeInTx(ctx, tx, SecurityNoticePayoutWallet, func() string {
		previous = securityPayoutWalletInTx(ctx, tx, networkId)
		return ""
	}) {
		watch.previous = previous
		watch.watching = true
	}
	return watch
}

// Owes the payout wallet notice when the change set, replaced or removed it.
func (self *payoutWalletWatch) noticeInTx(ctx context.Context, tx server.PgTx) {
	if !self.watching {
		return
	}
	oweSecurityWalletNoticeInTx(ctx, tx, self.networkId, SecurityNoticePayoutWallet, func() []*SecurityWalletChange {
		next := securityPayoutWalletInTx(ctx, tx, self.networkId)
		return []*SecurityWalletChange{
			newSecurityWalletChange(ctx, SecurityWalletDestinationUsdc, self.previous, next, 0, 0),
		}
	})
}

// Watches a network's subnet wallets across a wallet set in the caller's
// transaction: the network's claim wallet and, for a provider's set, that
// provider's wallet.
type subnetWalletWatch struct {
	networkId       server.Id
	clientId        *server.Id
	previousNetwork *securityWallet
	previousClient  *securityWallet
	watching        bool
}

// Reads the subnet wallets before a set. A failed read leaves the set
// unwatched; the set goes on.
func watchSubnetWalletInTx(ctx context.Context, tx server.PgTx, networkId server.Id, clientId *server.Id) *subnetWalletWatch {
	watch := &subnetWalletWatch{networkId: networkId, clientId: clientId}
	var previousNetwork, previousClient *securityWallet
	if guardSecurityNoticeInTx(ctx, tx, SecurityNoticeSubnetWallet, func() string {
		previousNetwork = securitySubnetNetworkWalletInTx(ctx, tx, networkId)
		if clientId != nil {
			previousClient = securitySubnetProviderWalletInTx(ctx, tx, *clientId)
		}
		return ""
	}) {
		watch.previousNetwork = previousNetwork
		watch.previousClient = previousClient
		watch.watching = true
	}
	return watch
}

// Owes the payout wallet notice for the subnet wallets the set changed: the
// provider's wallet (with a consent's epochs) and the network's claim wallet.
func (self *subnetWalletWatch) noticeInTx(ctx context.Context, tx server.PgTx, fromEpoch uint64, throughEpoch uint64) {
	if !self.watching {
		return
	}
	oweSecurityWalletNoticeInTx(ctx, tx, self.networkId, SecurityNoticeSubnetWallet, func() []*SecurityWalletChange {
		changes := []*SecurityWalletChange{}
		if self.clientId != nil {
			next := securitySubnetProviderWalletInTx(ctx, tx, *self.clientId)
			changes = append(changes, newSecurityWalletChange(ctx, SecurityWalletDestinationSubnetProvider, self.previousClient, next, fromEpoch, throughEpoch))
		}
		next := securitySubnetNetworkWalletInTx(ctx, tx, self.networkId)
		changes = append(changes, newSecurityWalletChange(ctx, SecurityWalletDestinationSubnetNetwork, self.previousNetwork, next, 0, 0))
		return changes
	})
}

// Watches the sign-in methods of the network a user administers across a change
// in the caller's transaction. The recipients are read before the change as
// well as after it, so an email address the change removes still hears of it.
type signInMethodWatch struct {
	userId    server.Id
	networkId *server.Id
	before    []string
	watching  bool
}

// Reads the recipients before a sign-in change. A failed read leaves the change
// unwatched; the change goes on.
func watchSignInMethodsInTx(ctx context.Context, tx server.PgTx, userId server.Id) *signInMethodWatch {
	watch := &signInMethodWatch{userId: userId}
	var networkId *server.Id
	var before []string
	if guardSecurityNoticeInTx(ctx, tx, SecurityNoticeSignIn, func() string {
		networkId = securityNoticeNetworkInTx(ctx, tx, userId)
		if networkId != nil {
			before = securityNoticeRecipientsInTx(ctx, tx, *networkId)
		}
		return ""
	}) {
		watch.networkId = networkId
		watch.before = before
		watch.watching = true
	}
	return watch
}

// Owes the sign-in method notice for the changes, to the recipients before and
// after them.
func (self *signInMethodWatch) noticeInTx(ctx context.Context, tx server.PgTx, changes ...*SecuritySignInChange) {
	if !self.watching || len(changes) == 0 {
		return
	}
	guardSecurityNoticeInTx(ctx, tx, SecurityNoticeSignIn, func() string {
		networkId := self.networkId
		if networkId == nil {
			// a user that administers no network has no one to tell
			return securityNoticeNoEmail
		}
		after := securityNoticeRecipientsInTx(ctx, tx, *networkId)
		recipients := securityNoticeEmails(append(slices.Clone(self.before), after...))
		return oweSecurityNoticeInTx(ctx, tx, *networkId, recipients, SecuritySignInTemplateName, func(templateJson string) (string, error) {
			return applySecurityNoticeChanges(templateJson, (*SecuritySignInNotice).add, changes)
		})
	})
}

// A change of a sign-in method, made by the request of ctx now.
func newSecuritySignInChange(ctx context.Context, action string, method string, identity string) *SecuritySignInChange {
	return &SecuritySignInChange{
		ChangeTime: server.NowUtc(),
		Action:     action,
		Method:     method,
		Identity:   identity,
		Via:        securityNoticeVia(ctx),
	}
}

// The method of a wallet sign-in's chain.
func securityWalletSignInMethod(blockchain string) string {
	if walletAuthType(blockchain) == AuthTypeBittensor {
		return SecuritySignInMethodBittensor
	}
	return SecuritySignInMethodSolana
}

// The admin's own sign-in (network_user.user_auth) when it is a phone number,
// normalized, or "". It is the one phone a security notice may reach, by the
// SMS AWS approved for the password-changed notice.
func securityNoticeLegacyPhoneInTx(ctx context.Context, tx server.PgTx, networkId server.Id) string {
	userAuth, err := getUserAuth(ctx, tx, networkId)
	if err != nil || userAuth == "" {
		return ""
	}
	normalUserAuth, userAuthType := NormalUserAuth(userAuth)
	if userAuthType != UserAuthTypePhone {
		return ""
	}
	return normalUserAuth
}
