// Security notices (owner decision 2026-10-29): an email to the account's
// sign-in addresses whenever its payout wallet or its sign-in methods change,
// so an owner notices a change they did not make. In May 2026 an attacker's
// wallet replaced the payout wallet of 1,459 networks and no owner noticed.
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
// Recipients are the admin's sign-in email addresses
// (securityNoticeRecipientsInTx). A phone number or a wallet is no email: an
// account that signs in only with those gets no notice, and no notice goes by
// SMS (AWS approved SMS for verification codes, password resets and the
// password-changed notice only).
//
// Coalescing: a change owes its notice due SecurityNoticeCoalesceWindow later.
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
)

// How long a notice waits for more changes of its kind to join it before it is
// due: the coalescing window for repeats.
const SecurityNoticeCoalesceWindow = 1 * time.Minute

// The most changes one notice lists. A later change replaces the last listed one
// and is counted, so the list always ends with the latest state.
const securityNoticeMaxChanges = 10

// The most sign-in email addresses one change notifies.
const securityNoticeMaxRecipients = 8

// The notice label of the metric.
const (
	SecurityNoticePayoutWallet = "payout_wallet"
	SecurityNoticeSubnetWallet = "subnet_wallet"
	SecurityNoticeSignIn       = "sign_in"
	SecurityNoticePassword     = "password"
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
		Help:      "Security notices owed for payout wallet, sign-in method and password changes, by notice and result (queued, coalesced, no_email, failed)",
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

// Appends the change while the list has room. A full list keeps its first
// changes and its last slot takes the latest change, so the notice always ends
// with the current state; the change it replaces is counted as not listed.
func addSecurityNoticeChange[C any](changes []C, moreCount int, change C) ([]C, int) {
	if len(changes) < securityNoticeMaxChanges {
		return append(changes, change), moreCount
	}
	changes[len(changes)-1] = change
	return changes, moreCount + 1
}

// The wallet address as a notice shows it: its first four and last four
// characters around an ellipsis ("9WzD…AWWM"). A value under twelve characters
// is no wallet address, and none of it is shown.
func MaskWalletAddress(address string) string {
	runes := []rune(strings.TrimSpace(address))
	if len(runes) == 0 {
		return ""
	}
	if len(runes) < 12 {
		return "…"
	}
	return string(runes[:4]) + "…" + string(runes[len(runes)-4:])
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

// A phone number as a notice shows it: an ellipsis and its last two digits
// ("…01").
func maskPhoneNumber(phoneNumber string) string {
	digits := []rune{}
	for _, r := range phoneNumber {
		if unicode.IsDigit(r) {
			digits = append(digits, r)
		}
	}
	if len(digits) < 6 {
		return "…"
	}
	return "…" + string(digits[len(digits)-2:])
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
		return maskPhoneNumber(normalUserAuth)
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
// addresses, normalized, distinct and sorted, at most
// securityNoticeMaxRecipients. A phone number or a wallet is no email, so an
// admin who signs in only with those, or a network that is gone, has none.
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
// most securityNoticeMaxRecipients.
func securityNoticeEmails(userAuths []string) []string {
	emails := []string{}
	for _, userAuth := range userAuths {
		normalUserAuth, userAuthType := NormalUserAuth(userAuth)
		if userAuthType == UserAuthTypeEmail && normalUserAuth != "" && !slices.Contains(emails, normalUserAuth) {
			emails = append(emails, normalUserAuth)
		}
	}
	slices.Sort(emails)
	if securityNoticeMaxRecipients < len(emails) {
		emails = emails[:securityNoticeMaxRecipients]
	}
	return emails
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

// The default-level report of notices that could not be owed, at most once a
// minute. It names the notice and the failure's kind only: a postgres detail
// can carry an address.
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
	report := &securityNoticeFailureReport
	report.stateLock.Lock()
	now := time.Now()
	if now.Sub(report.lastReportTime) < time.Minute {
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
// or adds a notice due SecurityNoticeCoalesceWindow from now. `apply` takes the
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
			DeliverAfter: SecurityNoticeCoalesceWindow,
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

// Owes the payout wallet notice for the changes `read` returns. `read` runs
// under the notice's savepoint, after the change, and returns nothing when the
// wallets did not change.
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
		recipients := securityNoticeRecipientsInTx(ctx, tx, networkId)
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
