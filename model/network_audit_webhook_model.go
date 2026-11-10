package model

// §7.11's audit reads and SIEM webhooks (R-TEAM-10, R-TEAM-12, D-TEAM-11; §2.8
// api-8; TEAM-M6-09).
//
//	read      GET /network/admin-events answers a team's admin events in seq
//	          order, a page at a time, to Auditors and above: every role that
//	          holds RolePermissionRead, so a Member is refused. Each event
//	          carries its predecessor's chain value and its own, so a reader
//	          verifies the chain itself (VerifyNetworkAdminEventResults)
//	webhook   POST /network/audit-webhook adds, lists or removes the team's
//	          webhooks, the Owner's. A webhook's target is an https URL whose
//	          host is public
//	delivery  DeliverNetworkAuditWebhooks posts each webhook the events after
//	          its delivered_seq, oldest first, as JSON signed with HMAC-SHA256
//	          (SignNetworkAuditWebhook). An answer that is not 2xx starts or
//	          continues a failing run, retried with a growing wait for 24
//	          hours; then the events the run held are skipped, an admin event
//	          records the skip, and the webhook stays. A delivery may repeat:
//	          a receiver drops a seq it has
//	target    a webhook's URL is its network's to choose, so every delivery
//	          goes through api's outbound guard: https only, no redirect
//	          followed, no proxy, and no dial of an address that is not public
//	          (NetworkIdpDialControl), whatever the name resolves to at the
//	          time
//	key       a webhook's signing key is derived from a 32-byte key only vault
//	          holds and the webhook's id, so no key is stored, and a removed
//	          webhook's key is never issued again. While vault holds no key,
//	          no webhook is added and none is delivered: it fails closed
//
// A webhook starts at its network's newest event, so it is sent what happens
// from its creation on, its own creation first. Events are never changed by
// delivery, and the log read is the same whether or not a webhook took them.
// A new webhook owes Owners and Admins the network authority notice (§7.8).
//
// Nothing here reads or writes network_audit_webhook unless the binary links
// MG6, nor network_admin_event unless it links MG1; until then each route
// answers 503 and delivery does nothing.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// Tunables of audit webhooks.
type NetworkAuditWebhookSettings struct {
	// The most webhooks one network holds: 5. Non-positive holds none.
	WebhookLimit int
	// How long a failing run is retried before its events are skipped: 24
	// hours (§7.11). Non-positive skips at the first failure.
	RetryWindow time.Duration
	// The wait after a run's first failure; each later wait is the run's age,
	// so waits double: 1 min.
	RetryBaseDelay time.Duration
	// The longest wait between two tries: 1 h, so a receiver back after an
	// outage is caught up within the hour.
	RetryMaxDelay time.Duration
	// The most events one delivery carries: 100.
	BatchEventCount int
	// The most deliveries one pass makes to one webhook, so a backlog drains
	// without one webhook holding the pass: 10.
	PassBatchCount int
	// The most webhooks one pass examines; the next pass takes the rest from
	// this one's cursor: 200.
	PassWebhookCount int
}

func DefaultNetworkAuditWebhookSettings() *NetworkAuditWebhookSettings {
	return &NetworkAuditWebhookSettings{
		WebhookLimit:     5,
		RetryWindow:      24 * time.Hour,
		RetryBaseDelay:   time.Minute,
		RetryMaxDelay:    time.Hour,
		BatchEventCount:  100,
		PassBatchCount:   10,
		PassWebhookCount: 200,
	}
}

// network_audit_webhook.url's width.
const networkAuditWebhookUrlMaxLength = 2048

// The vault resource holding the 32-byte key webhook signing keys derive
// from. Only vault holds it.
const NetworkAuditWebhookKeyVaultResource = "network_audit_webhook_key"

// The domain of a webhook's key derivation, so no other use of the vault key
// derives the same bytes.
const networkAuditWebhookKeyDomain = "urnetwork network_audit_webhook key v1"

// The header a delivery's signature rides in, and the header naming its
// webhook.
const (
	NetworkAuditWebhookSignatureHeader = "Urnetwork-Signature"
	NetworkAuditWebhookIdHeader        = "Urnetwork-Webhook-Id"
)

// The key webhook signing keys derive from, nil while vault holds none. A
// test replaces it.
var networkAuditWebhookRootKey = func() []byte {
	resource, err := server.Vault.SimpleResource(NetworkAuditWebhookKeyVaultResource)
	if err != nil {
		return nil
	}
	key, err := resource.BytesE()
	if err != nil || len(key) != 32 {
		return nil
	}
	return key
}

// Replaces the vault key for a test, returning a restore function.
func Testing_SetNetworkAuditWebhookRootKey(key []byte) func() {
	previous := networkAuditWebhookRootKey
	networkAuditWebhookRootKey = func() []byte { return key }
	return func() {
		networkAuditWebhookRootKey = previous
	}
}

// The webhook's signing key under rootKey: HMAC-SHA256(rootKey, domain ||
// webhook_id), 32 bytes.
func deriveNetworkAuditWebhookKey(rootKey []byte, webhookId server.Id) []byte {
	h := hmac.New(sha256.New, rootKey)
	h.Write([]byte(networkAuditWebhookKeyDomain))
	h.Write(webhookId.Bytes())
	return h.Sum(nil)
}

// The webhook's signing key, nil while vault holds no key to derive it from.
func networkAuditWebhookKey(webhookId server.Id) []byte {
	rootKey := networkAuditWebhookRootKey()
	if rootKey == nil {
		return nil
	}
	return deriveNetworkAuditWebhookKey(rootKey, webhookId)
}

// The value of a delivery's signature header: t=<unix seconds>,v1=<hex>, where
// the hex is HMAC-SHA256 under the webhook's key of the decimal t, a period
// and the body. A receiver recomputes it over the bytes it received and
// refuses a t far from its own clock.
func SignNetworkAuditWebhook(key []byte, signTime time.Time, body []byte) string {
	t := strconv.FormatInt(signTime.Unix(), 10)
	h := hmac.New(sha256.New, key)
	h.Write([]byte(t))
	h.Write([]byte("."))
	h.Write(body)
	return "t=" + t + ",v1=" + hex.EncodeToString(h.Sum(nil))
}

// Whether signature is SignNetworkAuditWebhook's of body under key, its t
// within tolerance of now. What a receiver runs.
func VerifyNetworkAuditWebhookSignature(key []byte, signature string, body []byte, now time.Time, tolerance time.Duration) bool {
	t, v1 := "", ""
	for _, part := range strings.Split(signature, ",") {
		if value, ok := strings.CutPrefix(part, "t="); ok {
			t = value
		} else if value, ok := strings.CutPrefix(part, "v1="); ok {
			v1 = value
		}
	}
	seconds, err := strconv.ParseInt(t, 10, 64)
	if err != nil {
		return false
	}
	signTime := time.Unix(seconds, 0)
	if signTime.Before(now.Add(-tolerance)) || signTime.After(now.Add(tolerance)) {
		return false
	}
	return hmac.Equal([]byte(SignNetworkAuditWebhook(key, signTime, body)), []byte("t="+t+",v1="+v1))
}

// A target api may post to: an https URL with no user or fragment, at most
// the column's width, whose host is a name or a public address. A name that
// resolves to an address that is not public is refused where api dials it.
func ValidNetworkAuditWebhookUrl(webhookUrl string) error {
	if webhookUrl == "" || networkAuditWebhookUrlMaxLength < len(webhookUrl) {
		return errors.New("The webhook URL is missing or too long.")
	}
	parsedUrl, err := url.Parse(webhookUrl)
	if err != nil || parsedUrl.Scheme != "https" || parsedUrl.Hostname() == "" || parsedUrl.User != nil || parsedUrl.Fragment != "" {
		return errors.New("The webhook URL must be an https URL with no user or fragment.")
	}
	host := strings.ToLower(strings.TrimSuffix(parsedUrl.Hostname(), "."))
	if address, err := netip.ParseAddr(host); err == nil {
		if !NetworkIdpPublicAddress(address) {
			return errors.New("The webhook URL's host must be public.")
		}
	} else if host == "localhost" || strings.HasSuffix(host, ".localhost") || !strings.Contains(host, ".") {
		// a single-label name resolves only inside api's own network
		return errors.New("The webhook URL's host must be public.")
	}
	return nil
}

// An admin event as the log's readers and webhooks' receivers see it. The
// chain values are lowercase hex.
type NetworkAdminEventResult struct {
	Seq       int64     `json:"seq"`
	EventTime time.Time `json:"event_time"`
	// user, client, api_key, system or sidecar
	ActorKind  string     `json:"actor_kind"`
	ActorId    *server.Id `json:"actor_id,omitempty"`
	Action     string     `json:"action"`
	TargetKind string     `json:"target_kind"`
	TargetId   *server.Id `json:"target_id,omitempty"`
	Result     string     `json:"result"`
	RequestId  *server.Id `json:"request_id,omitempty"`
	// the previous event's chain_sha256, 64 zeros before the first
	PrevSha256 string `json:"prev_sha256"`
	// SHA-256(prev_sha256 bytes || the event's canonical form)
	ChainSha256 string `json:"chain_sha256"`
}

func networkAdminEventResultOf(event *NetworkAdminEvent) *NetworkAdminEventResult {
	return &NetworkAdminEventResult{
		Seq:         event.Seq,
		EventTime:   event.EventTime.UTC(),
		ActorKind:   event.ActorKind,
		ActorId:     event.ActorId,
		Action:      event.Action,
		TargetKind:  event.TargetKind,
		TargetId:    event.TargetId,
		Result:      event.Result,
		RequestId:   event.RequestId,
		PrevSha256:  hex.EncodeToString(event.PrevSha256),
		ChainSha256: hex.EncodeToString(event.ChainSha256),
	}
}

// Verifies a run of networkId's events as a reader received them, in seq
// order: consecutive seqs, each event chained to the one before it, and each
// chain value recomputed from its event's fields. The run's first event
// anchors on its own prev_sha256, the 32 zero bytes when its seq is 1; a
// reader that keeps the last chain value it verified checks the next page's
// first event against it.
func VerifyNetworkAdminEventResults(networkId server.Id, results []*NetworkAdminEventResult) error {
	events := make([]*NetworkAdminEvent, 0, len(results))
	for _, result := range results {
		prevSha256, err := hex.DecodeString(result.PrevSha256)
		if err != nil || len(prevSha256) != sha256.Size {
			return fmt.Errorf("%w: event %d has no predecessor hash", errAdminEventChainBroken, result.Seq)
		}
		chainSha256, err := hex.DecodeString(result.ChainSha256)
		if err != nil || len(chainSha256) != sha256.Size {
			return fmt.Errorf("%w: event %d has no chain value", errAdminEventChainBroken, result.Seq)
		}
		events = append(events, &NetworkAdminEvent{
			AdminEvent: AdminEvent{
				NetworkId:  networkId,
				ActorKind:  result.ActorKind,
				ActorId:    result.ActorId,
				Action:     result.Action,
				TargetKind: result.TargetKind,
				TargetId:   result.TargetId,
				Result:     result.Result,
				RequestId:  result.RequestId,
			},
			Seq:         result.Seq,
			EventTime:   result.EventTime,
			PrevSha256:  prevSha256,
			ChainSha256: chainSha256,
		})
	}
	return verifyAdminEventChain(events)
}

// GET /network/admin-events's query.
type GetNetworkAdminEventsArgs struct {
	// read the events after this seq; 0 reads from the oldest kept
	After int64 `json:"after"`
	// the most events to answer, at most the log's read limit; non-positive
	// asks for that limit
	Limit int `json:"limit"`
}

type GetNetworkAdminEventsResult struct {
	Events []*NetworkAdminEventResult `json:"events"`
	// events follow the last one answered
	More bool `json:"more"`
}

const networkAdminEventsRefusedMessage = "Only the team's Auditors and above may read its admin events."

// What the audit routes and delivery depend on apart from the database, so
// tests can replace it.
type networkAudits struct {
	settings        *NetworkAuditWebhookSettings
	eventSettings   *NetworkAdminEventSettings
	now             func() time.Time
	requireTeamTier func(ctx context.Context, networkId server.Id) error
}

func newNetworkAudits() *networkAudits {
	return &networkAudits{
		settings:        DefaultNetworkAuditWebhookSettings(),
		eventSettings:   DefaultNetworkAdminEventSettings(),
		now:             server.NowUtc,
		requireTeamTier: RequireTeamTier,
	}
}

// The checks both audit routes make before their own work: MG1 and MG6, a
// network credential, and the team tier.
func (self *networkAudits) admit(clientSession *session.ClientSession, refusedMessage string) error {
	if !server.SidecarMg1Linked() || !server.SidecarMg6Linked() {
		return fmt.Errorf("%d %s", http.StatusServiceUnavailable, networkTeamsUnavailableMessage)
	}
	byJwt := clientSession.ByJwt
	if byJwt == nil || byJwt.ClientId != nil {
		return fmt.Errorf("%d %s", http.StatusForbidden, refusedMessage)
	}
	return self.requireTeamTier(clientSession.Ctx, byJwt.NetworkId)
}

// GET /network/admin-events?after=<seq>&limit=<n> (§7.11): the caller's
// team's admin events after a seq, in seq order. Auditors and above read
// them; reading writes no event.
func GetNetworkAdminEventsForSession(args *GetNetworkAdminEventsArgs, clientSession *session.ClientSession) (*GetNetworkAdminEventsResult, error) {
	return newNetworkAudits().getEvents(args, clientSession)
}

func (self *networkAudits) getEvents(args *GetNetworkAdminEventsArgs, clientSession *session.ClientSession) (*GetNetworkAdminEventsResult, error) {
	if err := self.admit(clientSession, networkAdminEventsRefusedMessage); err != nil {
		return nil, err
	}
	ctx := clientSession.Ctx
	networkId := clientSession.ByJwt.NetworkId
	permitted := false
	server.Db(ctx, func(conn server.PgConn) {
		permitted = teamCallerPermitsWithQuery(ctx, conn, clientSession, RolePermissionRead)
	})
	if !permitted {
		return nil, fmt.Errorf("%d %s", http.StatusForbidden, networkAdminEventsRefusedMessage)
	}
	limit := max(self.eventSettings.ReadLimit, 1)
	if 0 < args.Limit && args.Limit < limit {
		limit = args.Limit
	}
	// one past the page says whether more follow
	events := GetNetworkAdminEvents(ctx, networkId, max(args.After, 0), limit+1)
	result := &GetNetworkAdminEventsResult{
		Events: make([]*NetworkAdminEventResult, 0, min(len(events), limit)),
	}
	for i, event := range events {
		if limit <= i {
			result.More = true
			break
		}
		result.Events = append(result.Events, networkAdminEventResultOf(event))
	}
	return result, nil
}

// A webhook as network_audit_webhook holds it.
type NetworkAuditWebhook struct {
	WebhookId server.Id
	NetworkId server.Id
	Url       string
	// the newest admin event seq its receiver took, or was skipped past
	DeliveredSeq int64
	// the earliest next try of a failing run; nil when none waits
	NextTime *time.Time
	// the start of the failing run
	FailTime *time.Time
}

const networkAuditWebhookColumns = `webhook_id, network_id, url, delivered_seq, next_time, fail_time`

func scanNetworkAuditWebhook(result server.PgResult) *NetworkAuditWebhook {
	webhook := &NetworkAuditWebhook{}
	server.Raise(result.Scan(
		&webhook.WebhookId,
		&webhook.NetworkId,
		&webhook.Url,
		&webhook.DeliveredSeq,
		&webhook.NextTime,
		&webhook.FailTime,
	))
	return webhook
}

// The network's webhooks, oldest first.
func getNetworkAuditWebhooksWithQuery(ctx context.Context, query server.PgCanQuery, networkId server.Id) []*NetworkAuditWebhook {
	webhooks := []*NetworkAuditWebhook{}
	result, err := query.Query(
		ctx,
		`SELECT `+networkAuditWebhookColumns+` FROM network_audit_webhook WHERE network_id = $1 ORDER BY webhook_id`,
		networkId,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			webhooks = append(webhooks, scanNetworkAuditWebhook(result))
		}
	})
	return webhooks
}

// The webhook, nil for none or in a binary without MG6.
func GetNetworkAuditWebhook(ctx context.Context, webhookId server.Id) (webhook *NetworkAuditWebhook) {
	if !server.SidecarMg6Linked() {
		return nil
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT `+networkAuditWebhookColumns+` FROM network_audit_webhook WHERE webhook_id = $1`,
			webhookId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				webhook = scanNetworkAuditWebhook(result)
			}
		})
	})
	return
}

// The actions and target of a webhook's admin events.
const (
	AdminEventActionAuditWebhookAdd    = "audit_webhook.add"
	AdminEventActionAuditWebhookRemove = "audit_webhook.remove"
	// a failing run reached its 24 hours and its events were skipped, by the
	// system
	AdminEventActionAuditWebhookSkip = "audit_webhook.skip"
	AdminEventTargetAuditWebhook     = "audit_webhook"
)

// POST /network/audit-webhook's body: url adds a webhook, remove_webhook_id
// removes one, and neither lists the network's webhooks. Not both.
type NetworkAuditWebhookArgs struct {
	Url             string     `json:"url,omitempty"`
	RemoveWebhookId *server.Id `json:"remove_webhook_id,omitempty"`
}

type NetworkAuditWebhookError struct {
	Message string `json:"message"`
}

// A webhook as its network's Owner sees it. Never its key.
type NetworkAuditWebhookState struct {
	WebhookId    server.Id `json:"webhook_id"`
	Url          string    `json:"url"`
	DeliveredSeq int64     `json:"delivered_seq"`
	// a failing run began here; its events are skipped 24 hours on
	FailTime *time.Time `json:"fail_time,omitempty"`
	// the failing run's next try
	NextTime *time.Time `json:"next_time,omitempty"`
}

type NetworkAuditWebhookResult struct {
	// the webhook this call added
	WebhookId *server.Id `json:"webhook_id,omitempty"`
	// the added webhook's signing key, 32 bytes as base64url with no padding,
	// answered this once
	SigningKey string `json:"signing_key,omitempty"`
	// whether this call removed a webhook
	Removed bool `json:"removed,omitempty"`
	// the network's webhooks after the call, oldest first
	Webhooks []*NetworkAuditWebhookState `json:"webhooks"`
	Error    *NetworkAuditWebhookError   `json:"error,omitempty"`
}

// Never the key.
func (self *NetworkAuditWebhookResult) String() string {
	return "NetworkAuditWebhookResult{}"
}

// As String, for %#v.
func (self *NetworkAuditWebhookResult) GoString() string {
	return self.String()
}

const networkAuditWebhooksRefusedMessage = "Only the team's Owners may change its audit webhooks."

// POST /network/audit-webhook (§7.11): adds a webhook to the caller's team,
// removes one, or lists them. The Owner's.
func SetNetworkAuditWebhook(args *NetworkAuditWebhookArgs, clientSession *session.ClientSession) (*NetworkAuditWebhookResult, error) {
	return newNetworkAudits().setWebhook(args, clientSession)
}

func (self *networkAudits) setWebhook(args *NetworkAuditWebhookArgs, clientSession *session.ClientSession) (*NetworkAuditWebhookResult, error) {
	if err := self.admit(clientSession, networkAuditWebhooksRefusedMessage); err != nil {
		return nil, err
	}
	ctx := clientSession.Ctx
	networkId := clientSession.ByJwt.NetworkId
	add := args.Url != ""
	webhookId := server.NewId()
	var signingKey []byte
	message := ""
	switch {
	case add && args.RemoveWebhookId != nil:
		message = "Add a webhook or remove one, not both."
	case add:
		if err := ValidNetworkAuditWebhookUrl(args.Url); err != nil {
			message = err.Error()
		} else if signingKey = networkAuditWebhookKey(webhookId); signingKey == nil {
			message = "Audit webhooks are not available yet."
		}
	}
	now := self.now()
	refused, removed := false, false
	states := []*NetworkAuditWebhookState{}
	server.Tx(ctx, func(tx server.PgTx) {
		refused, removed = false, false
		states = []*NetworkAuditWebhookState{}
		if !teamCallerPermitsWithQuery(ctx, tx, clientSession, RolePermissionOwner) {
			refused = true
			return
		}
		// the network's webhooks change one at a time, so the limit holds
		server.RaisePgResult(tx.Exec(
			ctx,
			`SELECT pg_advisory_xact_lock(hashtextextended('network_audit_webhook:' || $1::uuid::text, 0))`,
			networkId,
		))
		webhooks := getNetworkAuditWebhooksWithQuery(ctx, tx, networkId)
		switch {
		case message != "":
		case add && self.settings.WebhookLimit <= len(webhooks):
			message = fmt.Sprintf("A network holds at most %d audit webhooks.", max(self.settings.WebhookLimit, 0))
		case add:
			// from the network's newest event on: the webhook's own creation
			// is the first event it is sent
			var headSeq int64
			server.Raise(tx.QueryRow(
				ctx,
				`SELECT COALESCE(MAX(seq), 0) FROM network_admin_event WHERE network_id = $1`,
				networkId,
			).Scan(&headSeq))
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					INSERT INTO network_audit_webhook (webhook_id, network_id, url, delivered_seq)
					VALUES ($1, $2, $3, $4)
				`,
				webhookId,
				networkId,
				args.Url,
				headSeq,
			))
			AddAdminEventInTx(ctx, tx, NewSessionAdminEvent(clientSession, AdminEventActionAuditWebhookAdd, AdminEventTargetAuditWebhook, &webhookId))
			// the network's admin events now leave for a new place (§7.8)
			oweSecurityNetworkAuthorityNoticeInTx(ctx, tx, networkId, &SecurityNetworkAuthorityChange{
				ChangeTime: now,
				Action:     SecurityNetworkAuthorityAuditWebhook,
				Via:        securityNoticeVia(ctx),
			})
			webhooks = getNetworkAuditWebhooksWithQuery(ctx, tx, networkId)
		case args.RemoveWebhookId != nil:
			tag := server.RaisePgResult(tx.Exec(
				ctx,
				`DELETE FROM network_audit_webhook WHERE network_id = $1 AND webhook_id = $2`,
				networkId,
				*args.RemoveWebhookId,
			))
			if tag.RowsAffected() != 0 {
				removed = true
				removedWebhookId := *args.RemoveWebhookId
				AddAdminEventInTx(ctx, tx, NewSessionAdminEvent(clientSession, AdminEventActionAuditWebhookRemove, AdminEventTargetAuditWebhook, &removedWebhookId))
				webhooks = getNetworkAuditWebhooksWithQuery(ctx, tx, networkId)
			}
		}
		for _, webhook := range webhooks {
			states = append(states, &NetworkAuditWebhookState{
				WebhookId:    webhook.WebhookId,
				Url:          webhook.Url,
				DeliveredSeq: webhook.DeliveredSeq,
				FailTime:     webhook.FailTime,
				NextTime:     webhook.NextTime,
			})
		}
	}, server.TxReadCommitted)
	if refused {
		return nil, fmt.Errorf("%d %s", http.StatusForbidden, networkAuditWebhooksRefusedMessage)
	}
	result := &NetworkAuditWebhookResult{Webhooks: states, Removed: removed}
	switch {
	case message != "":
		result.Error = &NetworkAuditWebhookError{Message: message}
	case add:
		result.WebhookId = &webhookId
		result.SigningKey = base64.RawURLEncoding.EncodeToString(signingKey)
	}
	return result, nil
}

// One delivery's body.
type NetworkAuditWebhookDelivery struct {
	WebhookId server.Id                  `json:"webhook_id"`
	NetworkId server.Id                  `json:"network_id"`
	Events    []*NetworkAdminEventResult `json:"events"`
}

// Posts deliveries to webhook targets.
type NetworkAuditWebhookSender interface {
	// Posts body to targetUrl with header. Nil only for a 2xx answer.
	Send(ctx context.Context, targetUrl string, header http.Header, body []byte) error
}

var networkAuditWebhookSender atomic.Pointer[NetworkAuditWebhookSender]

// Installs the process's sender; nil restores the HTTP one.
func SetNetworkAuditWebhookSender(sender NetworkAuditWebhookSender) {
	if sender == nil {
		networkAuditWebhookSender.Store(nil)
		return
	}
	networkAuditWebhookSender.Store(&sender)
}

func currentNetworkAuditWebhookSender() NetworkAuditWebhookSender {
	if sender := networkAuditWebhookSender.Load(); sender != nil {
		return *sender
	}
	return defaultNetworkAuditWebhookSender()
}

// The process's HTTP sender, made on first use.
var defaultNetworkAuditWebhookSender = sync.OnceValue(func() NetworkAuditWebhookSender {
	return NewHttpNetworkAuditWebhookSender(DefaultHttpNetworkAuditWebhookSenderSettings())
})

// Tunables of the HTTP sender.
type HttpNetworkAuditWebhookSenderSettings struct {
	// One delivery's bound, dial to answer: 10 s.
	RequestTimeout time.Duration
	// The most bytes read of a receiver's answer, which is then dropped: 4
	// KiB, enough to reuse the connection after a short body.
	MaxAnswerByteCount int64
	// Refuses a target before any dial: by default ValidNetworkAuditWebhookUrl,
	// which a stored URL passed when it was added and passes again here. A
	// test's receiver on loopback sets its own.
	TargetCheck func(targetUrl string) error
	// Refuses a dial: by default NetworkIdpDialControl's, every address that
	// is not public. A test's receiver on loopback sets its own.
	DialControl func(network string, address string, conn syscall.RawConn) error
	// The transport's TLS roots, nil for the system's. A test's receiver sets
	// its own.
	Transport *http.Transport
}

func DefaultHttpNetworkAuditWebhookSenderSettings() *HttpNetworkAuditWebhookSenderSettings {
	return &HttpNetworkAuditWebhookSenderSettings{
		RequestTimeout:     10 * time.Second,
		MaxAnswerByteCount: 4 * 1024,
		TargetCheck:        ValidNetworkAuditWebhookUrl,
		DialControl: func(network string, address string, conn syscall.RawConn) error {
			return NetworkIdpDialControl(network, address, conn)
		},
	}
}

// The outbound guard of a user-supplied target (the file header): https only,
// no redirect followed, no proxy, and every dial through DialControl.
type httpNetworkAuditWebhookSender struct {
	settings   *HttpNetworkAuditWebhookSenderSettings
	httpClient *http.Client
}

func NewHttpNetworkAuditWebhookSender(settings *HttpNetworkAuditWebhookSenderSettings) NetworkAuditWebhookSender {
	transport := settings.Transport
	if transport == nil {
		transport = http.DefaultTransport.(*http.Transport).Clone()
	} else {
		transport = transport.Clone()
	}
	dialer := &net.Dialer{Timeout: settings.RequestTimeout, Control: settings.DialControl}
	transport.DialContext = dialer.DialContext
	transport.Proxy = nil
	return &httpNetworkAuditWebhookSender{
		settings: settings,
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   settings.RequestTimeout,
			// a receiver answers where it is; a redirect could lead anywhere
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

func (self *httpNetworkAuditWebhookSender) Send(ctx context.Context, targetUrl string, header http.Header, body []byte) error {
	if err := self.settings.TargetCheck(targetUrl); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetUrl, bytes.NewReader(body))
	if err != nil {
		return err
	}
	for name, values := range header {
		req.Header[name] = values
	}
	res, err := self.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	io.Copy(io.Discard, io.LimitReader(res.Body, self.settings.MaxAnswerByteCount))
	if res.StatusCode < 200 || 300 <= res.StatusCode {
		return fmt.Errorf("the receiver answered %d", res.StatusCode)
	}
	return nil
}

type NetworkAuditWebhookDeliveryResult struct {
	// deliveries a receiver took
	DeliveredCount int `json:"delivered_count"`
	// deliveries that failed and wait for their next try
	FailedCount int `json:"failed_count"`
	// failing runs that reached RetryWindow, their events skipped
	SkippedCount int `json:"skipped_count"`
	// set when the pass stopped at its webhook bound, for the next pass
	AfterWebhookId *server.Id `json:"after_webhook_id,omitempty"`
}

// One delivery pass at now, from afterWebhookId (nil for the start), over at
// most PassWebhookCount webhooks that hold undelivered events and wait on no
// later try. Does nothing in a binary without MG1 and MG6, or while vault
// holds no key to sign with.
func DeliverNetworkAuditWebhooks(ctx context.Context, now time.Time, afterWebhookId *server.Id, settings *NetworkAuditWebhookSettings) *NetworkAuditWebhookDeliveryResult {
	audits := newNetworkAudits()
	audits.settings = settings
	return audits.deliver(ctx, now, afterWebhookId, currentNetworkAuditWebhookSender())
}

func (self *networkAudits) deliver(ctx context.Context, now time.Time, afterWebhookId *server.Id, sender NetworkAuditWebhookSender) *NetworkAuditWebhookDeliveryResult {
	deliveryResult := &NetworkAuditWebhookDeliveryResult{}
	if !server.SidecarMg1Linked() || !server.SidecarMg6Linked() {
		return deliveryResult
	}
	rootKey := networkAuditWebhookRootKey()
	if rootKey == nil {
		// nothing can be signed, so nothing is sent
		return deliveryResult
	}
	limit := max(self.settings.PassWebhookCount, 1)
	dueWebhooks := []*NetworkAuditWebhook{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT `+networkAuditWebhookColumns+`
				FROM network_audit_webhook
				WHERE
					($1::uuid IS NULL OR $1::uuid < webhook_id) AND
					(next_time IS NULL OR next_time <= $2) AND
					EXISTS (
						SELECT 1 FROM network_admin_event
						WHERE
							network_admin_event.network_id = network_audit_webhook.network_id AND
							network_audit_webhook.delivered_seq < network_admin_event.seq
					)
				ORDER BY webhook_id
				LIMIT $3
			`,
			afterWebhookId,
			now,
			limit,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				dueWebhooks = append(dueWebhooks, scanNetworkAuditWebhook(result))
			}
		})
	})
	for _, webhook := range dueWebhooks {
		if ctx.Err() != nil {
			return deliveryResult
		}
		self.deliverWebhook(ctx, now, webhook, deriveNetworkAuditWebhookKey(rootKey, webhook.WebhookId), sender, deliveryResult)
		webhookId := webhook.WebhookId
		deliveryResult.AfterWebhookId = &webhookId
	}
	if len(dueWebhooks) < limit {
		deliveryResult.AfterWebhookId = nil
	}
	return deliveryResult
}

// Sends one webhook its backlog, a batch at a time, until it is caught up, a
// delivery fails or the pass's bound for one webhook is reached.
func (self *networkAudits) deliverWebhook(
	ctx context.Context,
	now time.Time,
	webhook *NetworkAuditWebhook,
	key []byte,
	sender NetworkAuditWebhookSender,
	deliveryResult *NetworkAuditWebhookDeliveryResult,
) {
	deliveredSeq := webhook.DeliveredSeq
	for batch := 0; batch < max(self.settings.PassBatchCount, 1); batch += 1 {
		events := GetNetworkAdminEvents(ctx, webhook.NetworkId, deliveredSeq, max(self.settings.BatchEventCount, 1))
		if len(events) == 0 {
			return
		}
		delivery := &NetworkAuditWebhookDelivery{
			WebhookId: webhook.WebhookId,
			NetworkId: webhook.NetworkId,
			Events:    make([]*NetworkAdminEventResult, 0, len(events)),
		}
		for _, event := range events {
			delivery.Events = append(delivery.Events, networkAdminEventResultOf(event))
		}
		body, err := json.Marshal(delivery)
		if err == nil {
			header := http.Header{}
			header.Set("Content-Type", "application/json")
			header.Set("User-Agent", "urnetwork-audit-webhook/1")
			header.Set(NetworkAuditWebhookIdHeader, webhook.WebhookId.String())
			header.Set(NetworkAuditWebhookSignatureHeader, SignNetworkAuditWebhook(key, now, body))
			err = sender.Send(ctx, webhook.Url, header, body)
		}
		if ctx.Err() != nil {
			// a try the pass's own end cut short judges nothing
			return
		}
		lastSeq := events[len(events)-1].Seq
		if err == nil {
			if !self.settleDelivered(ctx, webhook.WebhookId, deliveredSeq, lastSeq) {
				// removed, or moved by another pass: this pass is done with it
				return
			}
			deliveredSeq = lastSeq
			deliveryResult.DeliveredCount += 1
			continue
		}
		if glog.V(1) {
			// the error may name the target, which a log must not (I-13)
			glog.Infof("[audit webhook][%s]delivery to %s failed\n", webhook.NetworkId, webhook.WebhookId)
		}
		if self.settleFailed(ctx, now, webhook.WebhookId, deliveredSeq) {
			deliveryResult.SkippedCount += 1
		} else {
			deliveryResult.FailedCount += 1
		}
		return
	}
}

// Records a delivery the receiver took: the cursor moves from fromSeq to
// toSeq and the failing run, if any, ends. False when the webhook is gone or
// its cursor is no longer fromSeq.
func (self *networkAudits) settleDelivered(ctx context.Context, webhookId server.Id, fromSeq int64, toSeq int64) (settled bool) {
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE network_audit_webhook
				SET delivered_seq = $3, next_time = NULL, fail_time = NULL
				WHERE webhook_id = $1 AND delivered_seq = $2
			`,
			webhookId,
			fromSeq,
			toSeq,
		))
		settled = tag.RowsAffected() == 1
	}, server.TxReadCommitted)
	return
}

// Records a failed delivery at now. Within RetryWindow of the run's start the
// next try waits the run's age, at least RetryBaseDelay and at most
// RetryMaxDelay, and never past the window's end, where the last try lands. A
// failure at or past the window's end skips the run's events: the cursor moves
// to the network's newest event, the run ends, and an admin event records the
// skip, which is the next event the webhook is sent. Answers whether it
// skipped.
func (self *networkAudits) settleFailed(ctx context.Context, now time.Time, webhookId server.Id, fromSeq int64) (skipped bool) {
	server.Tx(ctx, func(tx server.PgTx) {
		skipped = false
		var webhook *NetworkAuditWebhook
		result, err := tx.Query(
			ctx,
			`SELECT `+networkAuditWebhookColumns+` FROM network_audit_webhook WHERE webhook_id = $1 AND delivered_seq = $2 FOR UPDATE`,
			webhookId,
			fromSeq,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				webhook = scanNetworkAuditWebhook(result)
			}
		})
		if webhook == nil {
			return
		}
		failTime := now
		if webhook.FailTime != nil {
			failTime = *webhook.FailTime
		}
		windowEnd := failTime.Add(self.settings.RetryWindow)
		if !now.Before(windowEnd) {
			var headSeq int64
			server.Raise(tx.QueryRow(
				ctx,
				`SELECT COALESCE(MAX(seq), 0) FROM network_admin_event WHERE network_id = $1`,
				webhook.NetworkId,
			).Scan(&headSeq))
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					UPDATE network_audit_webhook
					SET delivered_seq = $2, next_time = NULL, fail_time = NULL
					WHERE webhook_id = $1
				`,
				webhookId,
				max(headSeq, fromSeq),
			))
			skippedWebhookId := webhookId
			AddAdminEventInTx(ctx, tx, &AdminEvent{
				NetworkId:  webhook.NetworkId,
				ActorKind:  AdminEventActorSystem,
				Action:     AdminEventActionAuditWebhookSkip,
				TargetKind: AdminEventTargetAuditWebhook,
				TargetId:   &skippedWebhookId,
				Result:     AdminEventResultOk,
			})
			skipped = true
			return
		}
		delay := min(max(now.Sub(failTime), self.settings.RetryBaseDelay), self.settings.RetryMaxDelay)
		nextTime := now.Add(delay)
		if windowEnd.Before(nextTime) {
			nextTime = windowEnd
		}
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE network_audit_webhook
				SET fail_time = $2, next_time = $3
				WHERE webhook_id = $1
			`,
			webhookId,
			failTime,
			nextTime,
		))
	}, server.TxReadCommitted)
	return
}
