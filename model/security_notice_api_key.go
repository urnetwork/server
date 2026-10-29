package model

// The API key security notice (security_notice_model.go; owner decision
// 2026-10-29, "Yes, API keys too"): creating or deleting an API key owes an
// email to the admin's sign-in email addresses, in the transaction of the
// change, so an owner notices a key they did not make. An API key is a full
// network credential (AUTHZ1.md G3) that outlives every sign-in session.
//
// Both ways to create or delete a key owe it: POST /account/api-key
// (CreateApiKey) and POST /account/api-key/remove (DeleteApiKey). The only
// other writer of account_api_key is apikey.Testing_CreateApiKey, a test
// fixture. Like the other notices it is owed under the notice's savepoint, so
// it never fails or rolls back the change, and repeats inside the coalescing
// window join one email.
//
// The notice names a key by its name and its creation time. It never holds the
// key: the secret is shown once, in the create answer, and only its hash is
// stored. Nor does it hold the key's id, whose leading characters are only its
// creation time again (ids are ULIDs).

import (
	"context"
	"strings"
	"time"
	"unicode"

	"github.com/urnetwork/server"
)

// What happened to an API key.
const (
	SecurityApiKeyCreated = "created"
	SecurityApiKeyDeleted = "deleted"
)

// The most characters of an API key's name a notice shows.
const securityNoticeMaxApiKeyNameLength = 64

// One change of an API key.
type SecurityApiKeyChange struct {
	ChangeTime time.Time `json:"change_time"`
	// one of SecurityApiKey{Created,Deleted}
	Action string `json:"action"`
	// the key's name as a notice shows it (SecurityNoticeApiKeyName); empty for
	// a key without one
	Name string `json:"name,omitempty"`
	// when a deleted key was created; nil for a key created, which was created
	// at the change time
	KeyCreateTime *time.Time `json:"key_create_time,omitempty"`
	// one of SecurityNoticeVia*, or "" when unknown
	Via string `json:"via,omitempty"`
}

// The API key notice: the changes it lists, oldest first.
type SecurityApiKeyNotice struct {
	Changes []*SecurityApiKeyChange `json:"changes"`
	// changes made between the listed ones and not listed
	MoreCount int `json:"more_count,omitempty"`
}

// Adds a change, keeping the latest change last when the list is full.
func (self *SecurityApiKeyNotice) add(change *SecurityApiKeyChange) {
	self.Changes, self.MoreCount = addSecurityNoticeChange(self.Changes, self.MoreCount, change)
}

// An API key's name as a notice shows it. The name is free text that whoever
// created the key chose, so a notice keeps it to one line in reading order:
// control and format characters (line breaks, tabs, direction overrides,
// zero-width marks) count as space, each run of space is one space, and the
// ends are trimmed. A name longer than securityNoticeMaxApiKeyNameLength
// characters is cut short of it and ends in an ellipsis. Applying it again
// changes nothing.
func SecurityNoticeApiKeyName(name string) string {
	builder := strings.Builder{}
	pendingSpace := false
	for _, r := range name {
		if unicode.IsSpace(r) || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			pendingSpace = true
			continue
		}
		if pendingSpace && 0 < builder.Len() {
			builder.WriteRune(' ')
		}
		pendingSpace = false
		builder.WriteRune(r)
	}
	runes := []rune(builder.String())
	if len(runes) <= securityNoticeMaxApiKeyNameLength {
		return string(runes)
	}
	return strings.TrimRight(string(runes[:securityNoticeMaxApiKeyNameLength-1]), " ") + "…"
}

// A change of an API key, made by the request of ctx now. keyCreateTime is the
// creation time of a deleted key.
func newSecurityApiKeyChange(ctx context.Context, action string, name string, keyCreateTime *time.Time) *SecurityApiKeyChange {
	change := &SecurityApiKeyChange{
		ChangeTime: server.NowUtc(),
		Action:     action,
		Name:       SecurityNoticeApiKeyName(name),
		Via:        securityNoticeVia(ctx),
	}
	if keyCreateTime != nil {
		createTime := keyCreateTime.UTC()
		change.KeyCreateTime = &createTime
	}
	return change
}

// Owes the API key notice for a key the caller's transaction created or deleted,
// to the network admin's sign-in email addresses. It never fails the change
// (guardSecurityNoticeInTx).
func oweSecurityApiKeyNoticeInTx(ctx context.Context, tx server.PgTx, networkId server.Id, change *SecurityApiKeyChange) {
	guardSecurityNoticeInTx(ctx, tx, SecurityNoticeApiKey, func() string {
		recipients := securityNoticeRecipientsInTx(ctx, tx, networkId)
		return oweSecurityNoticeInTx(ctx, tx, networkId, recipients, SecurityApiKeyTemplateName, func(templateJson string) (string, error) {
			return applySecurityNoticeChanges(templateJson, (*SecurityApiKeyNotice).add, []*SecurityApiKeyChange{change})
		})
	})
}

// Watches an API key across its delete in the caller's transaction: the key's
// name and creation time are read before its row is deleted.
type apiKeyDeleteWatch struct {
	networkId  server.Id
	name       string
	createTime time.Time
	// the network has the key, and the read before the delete succeeded
	watching bool
}

// Reads the key the network is about to delete. A key the network does not
// have, or a failed read, leaves the delete unwatched; the delete goes on.
func watchApiKeyDeleteInTx(ctx context.Context, tx server.PgTx, networkId server.Id, apiKeyId *server.Id) *apiKeyDeleteWatch {
	watch := &apiKeyDeleteWatch{networkId: networkId}
	if apiKeyId == nil {
		return watch
	}
	found := false
	var name string
	var createTime time.Time
	if guardSecurityNoticeInTx(ctx, tx, SecurityNoticeApiKey, func() string {
		result, err := tx.Query(
			ctx,
			`
				SELECT
					COALESCE(name, ''),
					create_time
				FROM account_api_key
				WHERE api_key_id = $1 AND network_id = $2
			`,
			*apiKeyId,
			networkId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&name, &createTime))
				found = true
			}
		})
		return ""
	}) && found {
		watch.name = name
		watch.createTime = createTime
		watch.watching = true
	}
	return watch
}

// Owes the API key notice for the key, once the delete deleted it.
func (self *apiKeyDeleteWatch) noticeInTx(ctx context.Context, tx server.PgTx) {
	if !self.watching {
		return
	}
	createTime := self.createTime
	oweSecurityApiKeyNoticeInTx(ctx, tx, self.networkId, newSecurityApiKeyChange(ctx, SecurityApiKeyDeleted, self.name, &createTime))
}
