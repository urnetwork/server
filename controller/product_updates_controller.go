package controller

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	// "time"

	"github.com/golang/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

var brevoWebhookExpectedAuths = sync.OnceValue(func() map[string]bool {
	brevo := server.Vault.RequireSimpleResource("brevo.yml")
	bearers := brevo.String("brevo", "webhook_bearers")
	expectedAuths := map[string]bool{}
	for _, bearer := range bearers {
		expectedAuth := fmt.Sprintf("bearer %s", bearer)
		expectedAuths[expectedAuth] = true
	}
	return expectedAuths
})

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

func brevoHeader(header http.Header) {
	header.Add("api-key", brevoApiKey())
}

func maskEmail(userEmail string) string {
	parts := strings.SplitN(userEmail, "@", 2)
	user := parts[0]
	host := parts[1]

	maskedUser := func() string {
		if len(user) < 6 {
			return "***"
		} else {
			return fmt.Sprintf("%s***%s", user[:2], user[len(user)-2:])
		}
	}()

	return fmt.Sprintf("%s@%s", maskedUser, host)
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
}

type BrevoWebhookResult struct {
}

// https://developers.brevo.com/docs/transactional-webhooks
func BrevoWebhook(
	webhookArgs *BrevoWebhookArgs,
	clientSession *session.ClientSession,
) (*BrevoWebhookResult, error) {

	auth := func() bool {
		auths, ok := clientSession.Header["Authorization"]
		if !ok {
			return false
		}
		expectedAuths := brevoWebhookExpectedAuths()
		for _, auth := range auths {
			if expectedAuths[strings.ToLower(auth)] {
				return true
			}
		}
		return false
	}()
	if !auth {
		return nil, fmt.Errorf("%d Not authorized.", http.StatusUnauthorized)
	}

	if webhookArgs.Event == "unsubscribe" {
		glog.Info("[product_updates]unsubscribe %s\n", maskEmail(webhookArgs.Email))
		model.AccountProductUpdatesSetForEmail(
			clientSession.Ctx,
			webhookArgs.Email,
			false,
		)
	}

	return &BrevoWebhookResult{}, nil
}

type BrevoListArgs struct {
	Emails []string `json:"emails",omitempty`
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

func BrevoAddContact(ctx context.Context, userEmail string) error {
	url := "https://api.brevo.com/v3/contacts"
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
		server.ResponseJsonObject,
	)
	if status != nil {
		if 200 <= status.Code && status.Code < 300 {
			return nil
		}
		if r.Code == "duplicate_parameter" {
			// "Unable to create contact, email is already associated with another Contact"
			return nil
		}
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("Could not add contact (%s)", status.Status)

}

func BrevoRemoveContact(ctx context.Context, userEmail string) error {
	url := fmt.Sprintf("https://api.brevo.com/v3/contacts/%s?identifierType=email_id", userEmail)

	status, r, err := server.HttpDeleteWithStatus[BrevoContactResult](
		ctx,
		url,
		brevoHeader,
		server.ResponseJsonObject,
	)
	if status != nil {
		if 200 <= status.Code && status.Code < 300 {
			return nil
		}
		if r.Code == "document_not_found" {
			// contact already gone
			return nil
		}
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("Could not remove contact (%s)", status.Status)
}

func BrevoAddToList(ctx context.Context, userEmail string, listId int) error {
	err := BrevoAddContact(ctx, userEmail)
	if err != nil {
		return err
	}

	url := fmt.Sprintf("https://api.brevo.com/v3/contacts/lists/%d/contacts/add", listId)

	args := &BrevoListArgs{
		Emails: []string{userEmail},
	}
	status, r, err := server.HttpPostWithStatus[BrevoListResult](
		ctx,
		url,
		args,
		brevoHeader,
		server.ResponseJsonObject,
	)
	if status != nil {
		if 200 <= status.Code && status.Code < 300 {
			if slices.Contains(r.Contacts.Success, userEmail) {
				return nil
			}
			return fmt.Errorf("Success list did not contain user email.")
		}
		if r.Code == "invalid_parameter" {
			// "Contact already in list and/or does not exist"
			// given the contact is already created, it must be already in the list
			return nil
		}
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("Could not add contact to list (%s)", status.Status)
}

func BrevoRemoveFromList(ctx context.Context, userEmail string, listId int) error {
	url := fmt.Sprintf("https://api.brevo.com/v3/contacts/lists/%d/contacts/remove", listId)

	args := &BrevoListArgs{
		Emails: []string{userEmail},
	}
	status, r, err := server.HttpPostWithStatus[BrevoListResult](
		ctx,
		url,
		args,
		brevoHeader,
		server.ResponseJsonObject,
	)
	if status != nil {
		if 200 <= status.Code && status.Code < 300 {
			if slices.Contains(r.Contacts.Success, userEmail) {
				return nil
			}
			return fmt.Errorf("Success list did not contain user email.")
		}
		if r.Code == "invalid_parameter" {
			// "Contact already removed from list and/or does not exist"
			// either the contact doesn't exist or doesn't exist in the list is success
			return nil
		}
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("Could not remove contact from list (%s)", status.Status)
}

// these set the initial product updates for new networks and users
func SyncInitialProductUpdates(ctx context.Context) error {

	// new network sync
	userEmailNetworkIds := model.GetNetworkUserEmailsForProductUpdatesSync(ctx)
	networkIdProductUpdatesSync := map[server.Id]bool{}

	for userEmail, networkId := range userEmailNetworkIds {
		glog.Info("[product_updates]add to new networks %s\n", maskEmail(userEmail))
		if err := BrevoAddToList(ctx, userEmail, newNetworksListId()); err == nil {
			networkIdProductUpdatesSync[networkId] = true
		} else {
			glog.Info("[product_updates]could not add to new networks %s. err = %s\n", maskEmail(userEmail), err)
		}
	}

	model.SetNetworkProductUpdatesSyncForUsers(ctx, networkIdProductUpdatesSync)

	// product updates sync
	userEmailUserIds := model.GetUserEmailsForProductUpdatesSync(ctx)
	userIdProductUpdatesSync := map[server.Id]bool{}

	for userEmail, userId := range userEmailUserIds {
		glog.Info("[product_updates]add to product updates %s\n", maskEmail(userEmail))
		if err := BrevoAddToList(ctx, userEmail, productUpdatesListId()); err == nil {
			userIdProductUpdatesSync[userId] = true
		} else {
			glog.Info("[product_updates]could not add to product updates %s. err = %s\n", maskEmail(userEmail), err)
		}
	}

	model.SetProductUpdatesSyncForUsers(ctx, userIdProductUpdatesSync)

	return nil
}

type SyncProductUpdatesForUserArgs struct {
	UserId server.Id `json:"user_id"`
}

type SyncProductUpdatesForUserResult struct {
}

func ScheduleSyncProductUpdatesForUser(clientSession *session.ClientSession, userId server.Id, tx server.PgTx) {
	task.ScheduleTaskInTx(
		tx,
		SyncProductUpdatesForUser,
		&SyncProductUpdatesForUserArgs{},
		clientSession,
		task.RunOnce("sync_product_updates_for_user", userId),
	)
}

func SyncProductUpdatesForUser(
	syncProductUpdatesForUser *SyncProductUpdatesForUserArgs,
	clientSession *session.ClientSession,
) (*SyncProductUpdatesForUserResult, error) {

	productUpdates, userEmails := model.GetProductUpdateUserEmailsForUser(
		clientSession.Ctx,
		clientSession.ByJwt.UserId,
	)
	if productUpdates {
		for userEmail, _ := range userEmails {
			glog.Info("[product_updates]add to product updates %s\n", maskEmail(userEmail))
			err := BrevoAddToList(clientSession.Ctx, userEmail, productUpdatesListId())
			if err != nil {
				return nil, err
			}
		}
	} else {
		for userEmail, _ := range userEmails {
			glog.Info("[product_updates]remove from new networks %s\n", maskEmail(userEmail))
			err := BrevoRemoveFromList(clientSession.Ctx, userEmail, newNetworksListId())
			if err != nil {
				return nil, err
			}
			glog.Info("[product_updates]remove from product updates %s\n", maskEmail(userEmail))
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
		glog.Info("[product_updates]remove email %s\n", maskEmail(removeProductUpdates.UserAuth))
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
