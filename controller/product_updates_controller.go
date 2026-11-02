package controller

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/glog"

	"maps"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// The Authorization values a Brevo webhook may carry: each configured user as
// a Basic credential with an empty password, lower cased. A blank user would
// add "Basic Og==", which anyone can send, so it adds nothing. Without a usable
// user the error says why, and every webhook is refused.
func brevoWebhookExpectedAuths() (map[string]bool, error) {
	brevo, err := server.Vault.SimpleResource("brevo.yml")
	if err != nil {
		return nil, err
	}
	// String panics on a malformed resource
	if _, err := brevo.ParseE(); err != nil {
		return nil, err
	}
	// TODO these are users not bearer tokens
	bearers := brevo.String("brevo", "webhook_bearers")
	expectedAuths := map[string]bool{}
	for _, bearer := range bearers {
		if strings.TrimSpace(bearer) == "" {
			continue
		}
		expectedAuth := strings.ToLower(fmt.Sprintf(
			"Basic %s",
			base64.StdEncoding.EncodeToString([]byte(
				fmt.Sprintf("%s:", bearer),
			)),
		))
		expectedAuths[expectedAuth] = true
	}
	if len(expectedAuths) == 0 {
		return nil, errors.New("brevo.yml: brevo.webhook_bearers names no user")
	}
	return expectedAuths, nil
}

var brevoApiKey = sync.OnceValue(func() string {
	brevo := server.Vault.RequireSimpleResource("brevo.yml")
	return brevo.RequireString("brevo", "api_key")
})

var newNetworksListId = sync.OnceValue(func() int {
	brevo := server.Config.RequireSimpleResource("brevo.yml")
	return brevo.RequireInt("brevo", "list_ids", "new_networks")
})

var productUpdatesListId = sync.OnceValue(func() int {
	brevo := server.Config.RequireSimpleResource("brevo.yml")
	return brevo.RequireInt("brevo", "list_ids", "network_users")
})

// brevoApiBaseUrl is replaceable only by hermetic protocol tests.
var brevoApiBaseUrl = "https://api.brevo.com/v3"

// brevoApiUrl joins an endpoint to the configurable base used by hermetic
// protocol tests. Production never mutates brevoApiBaseUrl.
func brevoApiUrl(path string) string {
	return strings.TrimRight(brevoApiBaseUrl, "/") + "/" + strings.TrimLeft(path, "/")
}

func brevoHeader(header http.Header) {
	header.Add("api-key", brevoApiKey())
}

func maskEmail(userEmail string) string {
	user, host, ok := strings.Cut(userEmail, "@")
	if !ok {
		return server.MaskValue(userEmail)
	}
	return fmt.Sprintf("%s@%s", server.MaskValue(user), host)
}

/*
	{
		"event":"unsubscribed",
	  "email": "example@domain.com",
	  "id": xxxxx,
	  "date": "2020-10-09 00:00:00",
	  "ts":1604933619,
	  "message-id": "201798300811.5787683@relay.domain.com",
	  "ts_event": 1604933654,
	  "subject": "My first Transactional",
		"X-Mailin-custom": "some_custom_header",
	  "template_id": 22,
	  "tag":"[\"transactionalTag\"]",
	  "user_agent": "Mozilla/5.0 (Windows NT 5.1; rv:11.0) Gecko Firefox/11.0 (via ggpht.com GoogleImageProxy)",
	  "device_used": "MOBILE",
	  "mirror_link": "https://app-smtp.brevo.com/log/preview/1a2000f4-4e33-23aa-ab68-900dxxx9152c",
	  "contact_id": 8,
	  "ts_epoch": 1604933623,
	  "sending_ip": "xxx.xxx.xxx.xxx"
	}
*/
type BrevoWebhookArgs struct {
	Event     string `json:"event"`
	Email     string `json:"email"`
	MessageId string `json:"message-id"`
	// the send's tags: Brevo posts either `tags` (array) or the legacy `tag`
	// (a JSON-encoded array in a string); the onboarding campaign sends carry
	// [onboarding, <template>, <variant>]
	Tags       []string `json:"tags,omitempty"`
	Tag        string   `json:"tag,omitempty"`
	TemplateId int      `json:"template_id,omitempty"`
	// X-Mailin-custom is the custom header the campaign sets per send
	Custom  string `json:"X-Mailin-custom,omitempty"`
	Link    string `json:"link,omitempty"`
	TsEvent int64  `json:"ts_event,omitempty"`
}

type BrevoWebhookResult struct {
}

// https://developers.brevo.com/docs/transactional-webhooks
func BrevoWebhook(
	webhookArgs *BrevoWebhookArgs,
	clientSession *session.ClientSession,
) (*BrevoWebhookResult, error) {
	// glog.Infof("[product_updates]brevo webhook: (%v) %v\n", clientSession.Header, webhookArgs)

	// fails closed: without a usable configured user nothing is accepted. A
	// refusal is counted by reason (urnetwork_brevo_webhook_requests_total) and
	// logged at most once a minute (brevo_webhook_auth.go).
	if err := verifyBrevoWebhookAuthorization(clientSession.Header); err != nil {
		return nil, err
	}

	if webhookArgs.Event == "unsubscribe" || webhookArgs.Event == "unsubscribed" {
		glog.Infof("[product_updates]unsubscribe %s\n", maskEmail(webhookArgs.Email))
		model.AccountProductUpdatesUnsubscribeForEmail(
			clientSession.Ctx,
			webhookArgs.Email,
			server.NowUtc(),
		)
	}

	// the campaigns' delivery events (attributed by message id)
	handleOnboardingBrevoWebhook(clientSession.Ctx, webhookArgs)
	handleReengagementBrevoWebhook(clientSession.Ctx, webhookArgs)

	return &BrevoWebhookResult{}, nil
}

type BrevoListArgs struct {
	Emails []string `json:"emails,omitempty"`
}

// {"contacts":{"success":["brien@ur.io"],"failure":[]}}%
type BrevoListResult struct {
	Contacts *BrevoListResultContacts `json:"contacts"`
	Code     string                   `json:"code"`
	Message  string                   `json:"message"`
}

type BrevoListResultContacts struct {
	Success []string `json:"success"`
	Failure []string `json:"failure"`
}

type BrevoContactArgs struct {
	Email         string `json:"email"`
	UpdateEnabled bool   `json:"updateEnabled"`
}

type BrevoContactResult struct {
	Id      uint64 `json:"id"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

type brevoProductUpdatesOperation string

const (
	brevoOperationAddContact     brevoProductUpdatesOperation = "add-contact"
	brevoOperationRemoveContact  brevoProductUpdatesOperation = "remove-contact"
	brevoOperationAddToList      brevoProductUpdatesOperation = "add-to-list"
	brevoOperationRemoveFromList brevoProductUpdatesOperation = "remove-from-list"

	brevoProviderCodeUnclassified = "unclassified"
)

type brevoProductUpdatesFailureClass string

const (
	brevoFailureProviderRejection brevoProductUpdatesFailureClass = "provider-rejection"
	brevoFailureResponseContract  brevoProductUpdatesFailureClass = "response-contract"
	brevoFailureResponseDecode    brevoProductUpdatesFailureClass = "response-decode"
	brevoFailureRequestCanceled   brevoProductUpdatesFailureClass = "request-canceled"
	brevoFailureRequestDeadline   brevoProductUpdatesFailureClass = "request-deadline"
	brevoFailureTransport         brevoProductUpdatesFailureClass = "transport"
	brevoFailureProtocol          brevoProductUpdatesFailureClass = "protocol"
)

// brevoSafeProviderCode returns only codes whose exact spelling is part of
// this client's reviewed protocol contract. Provider-controlled code and
// message fields must never become durable task errors through normalization
// or truncation: an unknown value is represented by one fixed token instead.
func brevoSafeProviderCode(code string) string {
	switch code {
	case "document_not_found", "duplicate_parameter", "invalid_json", "invalid_parameter", "not_found":
		return code
	default:
		return brevoProviderCodeUnclassified
	}
}

// brevoProductUpdatesError renders only local finite labels, the numeric HTTP
// status, and net/http's canonical status text. server.Status.Status is the
// remote status line and is deliberately not used.
func brevoProductUpdatesError(
	operation brevoProductUpdatesOperation,
	status *server.Status,
	providerCode string,
	failureClass brevoProductUpdatesFailureClass,
) error {
	statusLabel := "unavailable"
	if status != nil {
		statusLabel = strconv.Itoa(status.Code)
		if statusText := http.StatusText(status.Code); statusText != "" {
			statusLabel += " " + statusText
		}
	}
	return fmt.Errorf(
		"Brevo %s failed (status=%s; provider_code=%s; failure_class=%s)",
		operation,
		statusLabel,
		brevoSafeProviderCode(providerCode),
		failureClass,
	)
}

// brevoProductUpdatesRequestError intentionally does not wrap the underlying
// error. Transport errors can contain the request URL, while decoder errors
// can contain provider-controlled response fragments.
func brevoProductUpdatesRequestError(
	operation brevoProductUpdatesOperation,
	status *server.Status,
	err error,
) error {
	failureClass := brevoFailureTransport
	switch {
	case errors.Is(err, context.Canceled):
		failureClass = brevoFailureRequestCanceled
	case errors.Is(err, context.DeadlineExceeded):
		failureClass = brevoFailureRequestDeadline
	case status != nil:
		failureClass = brevoFailureResponseDecode
	}
	return brevoProductUpdatesError(operation, status, "", failureClass)
}

// brevoResponseJsonObject accepts the empty success body used by delete
// endpoints while still surfacing malformed non-empty JSON.
func brevoResponseJsonObject[R any](response *http.Response, responseBodyBytes []byte) (R, error) {
	if len(responseBodyBytes) == 0 && 200 <= response.StatusCode && response.StatusCode < 300 {
		var empty R
		return empty, nil
	}
	return server.ResponseJsonObject[R](response, responseBodyBytes)
}

func BrevoAddContact(ctx context.Context, userEmail string) error {
	url := brevoApiUrl("contacts")
	/*
			{
		  "updateEnabled": false,
		  "email": "hey@ur.io"
		}*/

	args := &BrevoContactArgs{
		Email: userEmail,
	}
	status, r, err := server.HttpPostWithStatus[BrevoContactResult](
		ctx,
		url,
		args,
		brevoHeader,
		brevoResponseJsonObject[BrevoContactResult],
	)
	if err != nil {
		return brevoProductUpdatesRequestError(brevoOperationAddContact, status, err)
	}
	if status == nil {
		return brevoProductUpdatesError(brevoOperationAddContact, nil, "", brevoFailureProtocol)
	}
	if 200 <= status.Code && status.Code < 300 {
		return nil
	}
	if r.Code == "duplicate_parameter" {
		// "Unable to create contact, email is already associated with another Contact"
		return nil
	}
	return brevoProductUpdatesError(brevoOperationAddContact, status, r.Code, brevoFailureProviderRejection)
}

func BrevoRemoveContact(ctx context.Context, userEmail string) error {
	url := fmt.Sprintf("%s?identifierType=email_id", brevoApiUrl("contacts/"+userEmail))

	status, r, err := server.HttpDeleteWithStatus[BrevoContactResult](
		ctx,
		url,
		brevoHeader,
		brevoResponseJsonObject[BrevoContactResult],
	)
	if err != nil {
		return brevoProductUpdatesRequestError(brevoOperationRemoveContact, status, err)
	}
	if status == nil {
		return brevoProductUpdatesError(brevoOperationRemoveContact, nil, "", brevoFailureProtocol)
	}
	if 200 <= status.Code && status.Code < 300 {
		return nil
	}
	if r.Code == "document_not_found" {
		// contact already gone
		return nil
	}
	return brevoProductUpdatesError(brevoOperationRemoveContact, status, r.Code, brevoFailureProviderRejection)
}

func BrevoAddToList(ctx context.Context, userEmail string, listId int) error {
	err := BrevoAddContact(ctx, userEmail)
	if err != nil {
		return err
	}

	url := brevoApiUrl(fmt.Sprintf("contacts/lists/%d/contacts/add", listId))

	args := &BrevoListArgs{
		Emails: []string{userEmail},
	}
	status, r, err := server.HttpPostWithStatus[BrevoListResult](
		ctx,
		url,
		args,
		brevoHeader,
		brevoResponseJsonObject[BrevoListResult],
	)
	if err != nil {
		return brevoProductUpdatesRequestError(brevoOperationAddToList, status, err)
	}
	if status == nil {
		return brevoProductUpdatesError(brevoOperationAddToList, nil, "", brevoFailureProtocol)
	}
	if 200 <= status.Code && status.Code < 300 {
		if r.Contacts == nil {
			return brevoProductUpdatesError(brevoOperationAddToList, status, "", brevoFailureResponseContract)
		}
		if slices.Contains(r.Contacts.Success, userEmail) {
			return nil
		}
		return brevoProductUpdatesError(brevoOperationAddToList, status, "", brevoFailureResponseContract)
	}
	if r.Code == "invalid_parameter" {
		// "Contact already in list and/or does not exist"
		// given the contact is already created, it must be already in the list
		return nil
	}
	return brevoProductUpdatesError(brevoOperationAddToList, status, r.Code, brevoFailureProviderRejection)
}

func BrevoRemoveFromList(ctx context.Context, userEmail string, listId int) error {
	url := brevoApiUrl(fmt.Sprintf("contacts/lists/%d/contacts/remove", listId))

	args := &BrevoListArgs{
		Emails: []string{userEmail},
	}
	status, r, err := server.HttpPostWithStatus[BrevoListResult](
		ctx,
		url,
		args,
		brevoHeader,
		brevoResponseJsonObject[BrevoListResult],
	)
	if err != nil {
		return brevoProductUpdatesRequestError(brevoOperationRemoveFromList, status, err)
	}
	if status == nil {
		return brevoProductUpdatesError(brevoOperationRemoveFromList, nil, "", brevoFailureProtocol)
	}
	if 200 <= status.Code && status.Code < 300 {
		if r.Contacts == nil {
			return brevoProductUpdatesError(brevoOperationRemoveFromList, status, "", brevoFailureResponseContract)
		}
		if slices.Contains(r.Contacts.Success, userEmail) {
			return nil
		}
		return brevoProductUpdatesError(brevoOperationRemoveFromList, status, "", brevoFailureResponseContract)
	}
	if r.Code == "invalid_parameter" {
		// "Contact already removed from list and/or does not exist"
		// either the contact doesn't exist or doesn't exist in the list is success
		return nil
	}
	return brevoProductUpdatesError(brevoOperationRemoveFromList, status, r.Code, brevoFailureProviderRejection)
}

// Mark only acknowledged subscriptions, join both lists, and retain partial
// failures for task retry. Successful markers make a replay idempotent.
func SyncInitialProductUpdates(ctx context.Context) error {
	// this is *2 for both lists
	// this seems to be the highest brevo will let us go
	parallelCount := 5

	var wg sync.WaitGroup
	var stateLock sync.Mutex
	var result error
	failed := func(err error) {
		stateLock.Lock()
		defer stateLock.Unlock()
		result = errors.Join(result, err)
	}

	// new network sync
	wg.Add(1)
	go func() {
		defer wg.Done()
		server.HandleError(func() {
			userEmailNetworkIds := model.GetNetworkUserEmailsForProductUpdatesSync(ctx)
			userEmails := slices.Collect(maps.Keys(userEmailNetworkIds))

			var subWg sync.WaitGroup

			n := max(16, len(userEmails)/parallelCount)
			for j := 0; j < len(userEmails); j += n {
				i0 := j
				i1 := min(j+n, len(userEmails))
				subWg.Add(1)
				go func() {
					defer subWg.Done()
					server.HandleError(func() {
						networkIdProductUpdatesSync := map[server.Id]bool{}

						for i := i0; i < i1; i += 1 {
							if err := ctx.Err(); err != nil {
								failed(err)
								break
							}
							userEmail := userEmails[i]
							networkId := userEmailNetworkIds[userEmail]
							glog.Infof("[product_updates][%d+%d/%d]add to new networks %s\n", i0, i-i0+1, i1-i0, maskEmail(userEmail))
							if err := BrevoAddToList(ctx, userEmail, newNetworksListId()); err == nil {
								networkIdProductUpdatesSync[networkId] = true
							} else {
								failed(err)
								glog.Infof("[product_updates][%d+%d/%d]could not add to new networks %s. err = %s\n", i0, i-i0+1, i1-i0, maskEmail(userEmail), err)
							}
						}

						model.SetNetworkProductUpdatesSyncForUsers(ctx, networkIdProductUpdatesSync)
					}, failed)
				}()
			}

			subWg.Wait()
		}, failed)
	}()

	// product updates sync
	wg.Add(1)
	go func() {
		defer wg.Done()
		server.HandleError(func() {
			userEmailUserIds := model.GetUserEmailsForProductUpdatesSync(ctx)
			userEmails := slices.Collect(maps.Keys(userEmailUserIds))

			var subWg sync.WaitGroup

			n := max(16, len(userEmails)/parallelCount)
			for j := 0; j < len(userEmails); j += n {
				i0 := j
				i1 := min(j+n, len(userEmails))
				subWg.Add(1)
				go func() {
					defer subWg.Done()
					server.HandleError(func() {
						userIdProductUpdatesSync := map[server.Id]bool{}

						for i := i0; i < i1; i += 1 {
							if err := ctx.Err(); err != nil {
								failed(err)
								break
							}
							userEmail := userEmails[i]
							userId := userEmailUserIds[userEmail]
							glog.Infof("[product_updates][%d+%d/%d]add to product updates %s\n", i0, i-i0+1, i1-i0, maskEmail(userEmail))
							if err := BrevoAddToList(ctx, userEmail, productUpdatesListId()); err == nil {
								userIdProductUpdatesSync[userId] = true
							} else {
								failed(err)
								glog.Infof("[product_updates][%d+%d/%d]could not add to product updates %s. err = %s\n", i0, i-i0+1, i1-i0, maskEmail(userEmail), err)
							}
						}

						model.SetProductUpdatesSyncForUsers(ctx, userIdProductUpdatesSync)
					}, failed)
				}()
			}

			subWg.Wait()
		}, failed)
	}()

	wg.Wait()

	return errors.Join(result, ctx.Err())
}

type SyncProductUpdatesForUserArgs struct {
	// the user whose logins are synced. Rows queued before the args carried
	// it hold the zero id and sync the scheduling session's user.
	UserId server.Id `json:"user_id"`
}

type SyncProductUpdatesForUserResult struct {
}

// ScheduleSyncProductUpdatesForUser queues the sync of the user's logins with
// Brevo's lists. Requests for the same user coalesce, and one made while the
// sync runs gets a successor, so the last change is always read by a later run.
func ScheduleSyncProductUpdatesForUser(clientSession *session.ClientSession, userId server.Id, tx server.PgTx) {
	task.ScheduleTaskInTx(
		tx,
		SyncProductUpdatesForUser,
		&SyncProductUpdatesForUserArgs{
			UserId: userId,
		},
		clientSession,
		task.RunOnce("sync_product_updates_for_user", userId),
	)
}

// SyncProductUpdatesForUser makes Brevo's lists match the state at run time:
// the user's email logins are on network_users while the network's product
// updates are on and its marketing is not suppressed, and on neither list
// otherwise. Every Brevo call is idempotent, so a replay or a successor
// converges on the same lists; a provider failure fails the task, which
// retries.
func SyncProductUpdatesForUser(
	syncProductUpdatesForUser *SyncProductUpdatesForUserArgs,
	clientSession *session.ClientSession,
) (*SyncProductUpdatesForUserResult, error) {
	userId := syncProductUpdatesForUser.UserId
	if userId == (server.Id{}) {
		if clientSession.ByJwt == nil {
			glog.Warningf("[product_updates]sync names no user\n")
			return &SyncProductUpdatesForUserResult{}, nil
		}
		userId = clientSession.ByJwt.UserId
	}

	productUpdates, marketingSuppressed, userEmails := model.GetProductUpdateUserEmailsForUser(
		clientSession.Ctx,
		userId,
	)
	if productUpdates && !marketingSuppressed {
		for i, userEmail := range userEmails {
			glog.Infof("[product_updates][%d/%d]add to product updates %s\n", i+1, len(userEmails), maskEmail(userEmail))
			err := BrevoAddToList(clientSession.Ctx, userEmail, productUpdatesListId())
			if err != nil {
				return nil, err
			}
		}
	} else {
		// an opt-out, or a marketing suppression the lists mirror
		reason := "opted out"
		if productUpdates {
			reason = "marketing suppressed"
		}
		for i, userEmail := range userEmails {
			glog.Infof("[product_updates][%d/%d]remove from new networks %s (%s)\n", i+1, len(userEmails), maskEmail(userEmail), reason)
			err := BrevoRemoveFromList(clientSession.Ctx, userEmail, newNetworksListId())
			if err != nil {
				return nil, err
			}
			glog.Infof("[product_updates][%d/%d]remove from product updates %s (%s)\n", i+1, len(userEmails), maskEmail(userEmail), reason)
			err = BrevoRemoveFromList(clientSession.Ctx, userEmail, productUpdatesListId())
			if err != nil {
				return nil, err
			}
		}
	}

	return &SyncProductUpdatesForUserResult{}, nil
}

func SyncProductUpdatesForUserPost(
	syncProductUpdatesForUser *SyncProductUpdatesForUserArgs,
	syncProductUpdatesForUserResult *SyncProductUpdatesForUserResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	return nil
}

// ----- the marketing suppression's Brevo mirror -----

// SuppressNetworkMarketing stops every campaign's mail to the network (the
// first suppression's time and reason stay) and mirrors it to Brevo: the
// product-updates sync, queued in the same transaction, takes the admin's
// email logins off the lists. Every call queues the sync, so a replayed bounce
// or complaint also mirrors a suppression recorded before the mirror existed.
func SuppressNetworkMarketing(ctx context.Context, networkId server.Id, reason string, now time.Time) {
	server.Tx(ctx, func(tx server.PgTx) {
		model.SuppressNetworkMarketingInTx(tx, ctx, networkId, reason, now)
		scheduleSyncProductUpdatesForNetworkInTx(tx, ctx, networkId)
	})
}

// LiftNetworkMarketingSuppression clears a suppression whose reason is in
// `liftable` (the scan's: a sunset or a "switched" answer, never a bounce or
// complaint) and, when it lifted one, queues the product-updates sync in the
// same transaction. The sync puts the admin's email logins back on
// network_users only if product updates are still on when it runs. Returns
// whether it lifted.
func LiftNetworkMarketingSuppression(ctx context.Context, networkId server.Id, liftable []string) (lifted bool) {
	server.Tx(ctx, func(tx server.PgTx) {
		lifted = model.LiftNetworkMarketingSuppressionInTx(tx, ctx, networkId, liftable)
		if lifted {
			scheduleSyncProductUpdatesForNetworkInTx(tx, ctx, networkId)
		}
	})
	return
}

// scheduleSyncProductUpdatesForNetworkInTx queues the sync of the network
// admin's logins. A network that no longer exists has nothing to sync: its
// removal queued RemoveProductUpdates, which deletes the contacts.
func scheduleSyncProductUpdatesForNetworkInTx(tx server.PgTx, ctx context.Context, networkId server.Id) {
	userId, ok := model.GetNetworkAdminUserIdInTx(tx, ctx, networkId)
	if !ok {
		return
	}
	// an in-process owner of no user and no address: the args name the user.
	// A worker built before they did fails such a task, and it retries until
	// a current worker runs it, rather than syncing the old way (which never
	// removes for a suppression).
	clientSession := session.NewLocalClientSession(ctx, "", nil)
	defer clientSession.Cancel()
	ScheduleSyncProductUpdatesForUser(clientSession, userId, tx)
}

// MirrorNetworkMarketingSuppressions queues the product-updates sync for the
// admin of every network whose marketing is suppressed, for the suppressions
// recorded before the mirror existed (bringyourctl reengagement
// mirror-suppressions). Safe to repeat: queued syncs coalesce per user, and
// the sync of a mirrored suppression changes nothing. Returns how many
// networks it queued.
func MirrorNetworkMarketingSuppressions(ctx context.Context) (queued int) {
	return mirrorNetworkMarketingSuppressions(ctx, 1000)
}

func mirrorNetworkMarketingSuppressions(ctx context.Context, pageSize int) (queued int) {
	var after *server.Id
	for {
		admins := model.ListMarketingSuppressedNetworkAdmins(ctx, after, pageSize)
		if len(admins) == 0 {
			return
		}
		server.Tx(ctx, func(tx server.PgTx) {
			// the owner scheduleSyncProductUpdatesForNetworkInTx uses
			clientSession := session.NewLocalClientSession(ctx, "", nil)
			defer clientSession.Cancel()
			for _, admin := range admins {
				ScheduleSyncProductUpdatesForUser(clientSession, admin.AdminUserId, tx)
			}
		})
		queued += len(admins)
		after = &admins[len(admins)-1].NetworkId
	}
}

type RemoveProductUpdatesArgs struct {
	UserAuth string `json:"user_auth"`
}

type RemoveProductUpdatesResult struct {
}

func ScheduleRemoveProductUpdates(clientSession *session.ClientSession, userAuth string, tx server.PgTx) {
	task.ScheduleTaskInTx(
		tx,
		RemoveProductUpdates,
		&RemoveProductUpdatesArgs{
			UserAuth: userAuth,
		},
		clientSession,
		task.RunOnce("remove_product_updates", userAuth),
	)
}

func RemoveProductUpdates(
	removeProductUpdates *RemoveProductUpdatesArgs,
	clientSession *session.ClientSession,
) (*RemoveProductUpdatesResult, error) {
	_, authType := model.NormalUserAuthV1(&removeProductUpdates.UserAuth)
	if authType == model.UserAuthTypeEmail {
		glog.Infof("[product_updates]remove email %s\n", maskEmail(removeProductUpdates.UserAuth))
		err := BrevoRemoveContact(clientSession.Ctx, removeProductUpdates.UserAuth)
		if err != nil {
			return nil, err
		}
	}
	return &RemoveProductUpdatesResult{}, nil
}

func RemoveProductUpdatesPost(
	removeProductUpdates *RemoveProductUpdatesArgs,
	removeProductUpdatesResult *RemoveProductUpdatesResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	return nil
}
