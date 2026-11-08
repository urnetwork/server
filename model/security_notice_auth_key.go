package model

// The auth key security notice (R-TEAM-12, §7.8's notices): creating or
// revoking an auth key owes an email to the admin's sign-in email addresses, in
// the transaction of the change, as the API key notice does
// (security_notice_api_key.go), so an owner notices a key they did not make. A
// key admits devices to the network until it expires or its uses run out.
//
// Both ways to change a key owe it: POST /network/auth-keys (CreateAuthKey) and
// POST /network/auth-key-revoke (RevokeAuthKey). Expiry is no change anyone
// made, and owes none. Like the other notices it is owed under the notice's
// savepoint, so it never fails or rolls back the change, and repeats inside the
// coalescing window join one email.
//
// A key has no name, so the notice describes it: its expiry, its uses, whether
// it is ephemeral or pre-approved and how many tags it carries. It never holds
// the key, its hash, its id or its tag ids.

import (
	"context"
	"time"

	"github.com/urnetwork/server"
)

// The notice's template; the controller's template of the same name renders it.
const SecurityAuthKeyTemplateName = "security_auth_key_changed"

// The notice label of the metric.
const SecurityNoticeAuthKey = "auth_key"

// What happened to an auth key.
const (
	SecurityAuthKeyCreated = "created"
	SecurityAuthKeyRevoked = "revoked"
)

// One change of an auth key.
type SecurityAuthKeyChange struct {
	ChangeTime time.Time `json:"change_time"`
	// one of SecurityAuthKey{Created,Revoked}
	Action     string    `json:"action"`
	ExpireTime time.Time `json:"expire_time"`
	MaxUses    int       `json:"max_uses"`
	// the key's uses so far, for a revoked key
	UseCount    int  `json:"use_count,omitempty"`
	Ephemeral   bool `json:"ephemeral,omitempty"`
	PreApproved bool `json:"pre_approved,omitempty"`
	// how many tags the key carries, never which
	TagCount int `json:"tag_count,omitempty"`
	// one of SecurityNoticeVia*, or "" when unknown
	Via string `json:"via,omitempty"`
}

// The auth key notice: the changes it lists, oldest first.
type SecurityAuthKeyNotice struct {
	Changes []*SecurityAuthKeyChange `json:"changes"`
	// changes made between the listed ones and not listed
	MoreCount int `json:"more_count,omitempty"`
}

// Adds a change, keeping the latest change last when the list is full.
func (self *SecurityAuthKeyNotice) add(change *SecurityAuthKeyChange) {
	self.Changes, self.MoreCount = addSecurityNoticeChange(self.Changes, self.MoreCount, change)
}

// A change of the key, made by the request of ctx now.
func newSecurityAuthKeyChange(ctx context.Context, action string, authKey *NetworkAuthKey, now time.Time) *SecurityAuthKeyChange {
	change := &SecurityAuthKeyChange{
		ChangeTime:  now.UTC(),
		Action:      action,
		ExpireTime:  authKey.ExpireTime.UTC(),
		MaxUses:     authKey.MaxUses,
		Ephemeral:   authKey.Ephemeral,
		PreApproved: authKey.PreApproved,
		TagCount:    len(authKey.TagIds),
		Via:         securityNoticeVia(ctx),
	}
	if action == SecurityAuthKeyRevoked {
		change.UseCount = authKey.UseCount
	}
	return change
}

// Owes the auth key notice for a key the caller's transaction created or
// revoked, to the network admin's sign-in email addresses. It never fails the
// change (guardSecurityNoticeInTx).
func oweSecurityAuthKeyNoticeInTx(ctx context.Context, tx server.PgTx, networkId server.Id, change *SecurityAuthKeyChange) {
	guardSecurityNoticeInTx(ctx, tx, SecurityNoticeAuthKey, func() string {
		recipients := securityNoticeRecipientsInTx(ctx, tx, networkId)
		return oweSecurityNoticeInTx(ctx, tx, networkId, recipients, SecurityAuthKeyTemplateName, func(templateJson string) (string, error) {
			return applySecurityNoticeChanges(templateJson, (*SecurityAuthKeyNotice).add, []*SecurityAuthKeyChange{change})
		})
	})
}
