package controller

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	// "time"
	"encoding/base64"

	"github.com/urnetwork/glog/v2026"

	"golang.org/x/exp/maps"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

var brevoWebhookExpectedAuths = sync.OnceValue(func() map[string]bool {
	brevo := server.Vault.RequireSimpleResource("brevo.yml")
	// TODO these are users not bearer tokens
	bearers := brevo.String("brevo", "webhook_bearers")
	expectedAuths := map[string]bool{}
	for _, bearer := range bearers {
		expectedAuth := strings.ToLower(fmt.Sprintf(
			"Basic %s",
			base64.StdEncoding.EncodeToString([]byte(
				fmt.Sprintf("%s:", bearer),
			)),
		))
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

	maskedUser := server.MaskValue(user)

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
	// glog.Infof("[product_updates]brevo webhook: (%v) %v\n", clientSession.Header, webhookArgs)

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
		glog.Infof("[product_updates]unsubscribe %s\n", maskEmail(webhookArgs.Email))
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
	// this is *2 for both lists
	// this seems to be the highest brevo will let us go
	parallelCount := 5

	var wg sync.WaitGroup

	// new network sync
	wg.Add(1)
	go server.HandleError(func() {
		defer wg.Done()

		userEmailNetworkIds := model.GetNetworkUserEmailsForProductUpdatesSync(ctx)
		userEmails := maps.Keys(userEmailNetworkIds)

		var subWg sync.WaitGroup

		n := max(16, len(userEmails)/parallelCount)
		for j := 0; j < len(userEmails); j += n {
			i0 := j
			i1 := min(j+n, len(userEmails))
			subWg.Add(1)
			go server.HandleError(func() {
				defer subWg.Done()

				networkIdProductUpdatesSync := map[server.Id]bool{}

				for i := i0; i < i1; i += 1 {
					userEmail := userEmails[i]
					networkId := userEmailNetworkIds[userEmail]
					glog.Infof("[product_updates][%d+%d/%d]add to new networks %s\n", i0, i-i0+1, i1-i0, maskEmail(userEmail))
					if err := BrevoAddToList(ctx, userEmail, newNetworksListId()); err == nil {
						networkIdProductUpdatesSync[networkId] = true
					} else {
						glog.Infof("[product_updates][%d+%d/%d]could not add to new networks %s. err = %s\n", i0, i-i0+1, i1-i0, maskEmail(userEmail), err)
					}
				}

				model.SetNetworkProductUpdatesSyncForUsers(ctx, networkIdProductUpdatesSync)
			})
		}

		subWg.Wait()
	})

	// product updates sync
	wg.Add(1)
	go server.HandleError(func() {
		defer wg.Done()

		userEmailUserIds := model.GetUserEmailsForProductUpdatesSync(ctx)
		userEmails := maps.Keys(userEmailUserIds)

		var subWg sync.WaitGroup

		n := max(16, len(userEmails)/parallelCount)
		for j := 0; j < len(userEmails); j += n {
			i0 := j
			i1 := min(j+n, len(userEmails))
			subWg.Add(1)
			go server.HandleError(func() {
				defer subWg.Done()

				userIdProductUpdatesSync := map[server.Id]bool{}

				for i := i0; i < i1; i += 1 {
					userEmail := userEmails[i]
					userId := userEmailUserIds[userEmail]
					glog.Infof("[product_updates][%d+%d/%d]add to product updates %s\n", i0, i-i0+1, i1-i0, maskEmail(userEmail))
					if err := BrevoAddToList(ctx, userEmail, productUpdatesListId()); err == nil {
						userIdProductUpdatesSync[userId] = true
					} else {
						glog.Infof("[product_updates][%d+%d/%d]could not add to product updates %s. err = %s\n", i0, i-i0+1, i1-i0, maskEmail(userEmail), err)
					}
				}

				model.SetProductUpdatesSyncForUsers(ctx, userIdProductUpdatesSync)
			})
		}

		subWg.Wait()
	})

	wg.Wait()

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
		for i, userEmail := range userEmails {
			glog.Infof("[product_updates][%d/%d]add to product updates %s\n", i+1, len(userEmails), maskEmail(userEmail))
			err := BrevoAddToList(clientSession.Ctx, userEmail, productUpdatesListId())
			if err != nil {
				return nil, err
			}
		}
	} else {
		for i, userEmail := range userEmails {
			glog.Infof("[product_updates][%d/%d]remove from new networks %s\n", i+1, len(userEmails), maskEmail(userEmail))
			err := BrevoRemoveFromList(clientSession.Ctx, userEmail, newNetworksListId())
			if err != nil {
				return nil, err
			}
			glog.Infof("[product_updates][%d/%d]remove from product updates %s\n", i+1, len(userEmails), maskEmail(userEmail))
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
