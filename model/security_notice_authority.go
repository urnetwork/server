package model

// The network authority notice (R-TEAM-12, §7.8's notices): a change to who may
// admit devices or sign people in to a network owes its Owners and Admins an
// email, so an owner notices a change they did not make:
//
//	delegation   a new Delegation landed in the network's log, whose key
//	             admits agents until it ends (TEAM-M2-16's effects)
//	admin_set    the AdminSet changed: its members' roles or its threshold
//	             (TEAM-M2-16's effects)
//	idp          a team gained an OIDC IdP, which signs members in
//	             (TEAM-M6-05)
//	domain       a team added a domain to verify, and domain_verified when it
//	             proved one, from when the domain's sign-ins route to the
//	             team's IdP (TEAM-M6-06)
//	audit_webhook  a team gained a webhook its admin events are sent to
//	             (TEAM-M6-09)
//
// The recipients are the sign-in email addresses of the network's Owners and
// Admins: in a personal network its owner's, the account security notices
// already go to; in a team each Owner's and Admin's member user's, since the
// team's owner of record never signs in (D-TEAM-2). Like the other notices it
// is owed under the notice's savepoint in the change's own transaction, so it
// never fails the change, and repeats within the coalescing window join one
// email. It names no key, delegation, member or IdP, only what kind of change
// it was and when.

import (
	"context"
	"time"

	"github.com/urnetwork/server"
)

// The notice's template; the controller's template of the same name renders it.
const SecurityNetworkAuthorityTemplateName = "security_network_authority_changed"

// The notice label of the metric.
const SecurityNoticeNetworkAuthority = "network_authority"

// What changed.
const (
	SecurityNetworkAuthorityDelegation = "delegation"
	SecurityNetworkAuthorityAdminSet   = "admin_set"
	SecurityNetworkAuthorityIdp        = "idp"
	SecurityNetworkAuthorityDomain     = "domain"
	// the domain's TXT record was found
	SecurityNetworkAuthorityDomainVerified = "domain_verified"
	SecurityNetworkAuthorityAuditWebhook   = "audit_webhook"
)

// One change of a network's authority.
type SecurityNetworkAuthorityChange struct {
	ChangeTime time.Time `json:"change_time"`
	// one of SecurityNetworkAuthority*
	Action string `json:"action"`
	// one of SecurityNoticeVia*, or "" when unknown, as for a change the
	// network's log carried
	Via string `json:"via,omitempty"`
}

// The network authority notice: the changes it lists, oldest first.
type SecurityNetworkAuthorityNotice struct {
	Changes []*SecurityNetworkAuthorityChange `json:"changes"`
	// changes made between the listed ones and not listed
	MoreCount int `json:"more_count,omitempty"`
}

// Adds a change, keeping the latest change last when the list is full.
func (self *SecurityNetworkAuthorityNotice) add(change *SecurityNetworkAuthorityChange) {
	self.Changes, self.MoreCount = addSecurityNoticeChange(self.Changes, self.MoreCount, change)
}

// The sign-in email addresses of the network's Owners and Admins, read in tx:
// its owner's in a personal network, each Owner's and Admin's member user's in
// a team.
func securityNoticeAuthorityRecipientsInTx(ctx context.Context, tx server.PgTx, networkId server.Id) []string {
	team := false
	if server.SidecarMg6Linked() {
		server.Raise(tx.QueryRow(
			ctx,
			`SELECT EXISTS (SELECT 1 FROM network_team WHERE network_id = $1)`,
			networkId,
		).Scan(&team))
	}
	if !team {
		return securityNoticeRecipientsInTx(ctx, tx, networkId)
	}
	userAuths := []string{}
	result, err := tx.Query(
		ctx,
		`
			WITH admins AS (
				SELECT user_id
				FROM network_member
				WHERE network_id = $1 AND NOT pending AND role IN ($2, $3)
			)
			SELECT network_user.user_auth
			FROM admins
			INNER JOIN network_user ON network_user.user_id = admins.user_id
			WHERE network_user.user_auth IS NOT NULL
			UNION ALL
			SELECT network_user_auth_password.user_auth
			FROM admins
			INNER JOIN network_user_auth_password ON network_user_auth_password.user_id = admins.user_id
			WHERE
				network_user_auth_password.auth_type = 'email' AND
				network_user_auth_password.verified
			UNION ALL
			SELECT network_user_auth_sso.user_auth
			FROM admins
			INNER JOIN network_user_auth_sso ON network_user_auth_sso.user_id = admins.user_id
		`,
		networkId,
		string(NetworkMemberRoleOwner),
		string(NetworkMemberRoleAdmin),
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var userAuth string
			server.Raise(result.Scan(&userAuth))
			userAuths = append(userAuths, userAuth)
		}
	})
	return securityNoticeEmails(userAuths)
}

// Owes the network authority notice for a change the caller's transaction
// made or carried out, to the network's Owners and Admins. It never fails the
// change (guardSecurityNoticeInTx).
func oweSecurityNetworkAuthorityNoticeInTx(ctx context.Context, tx server.PgTx, networkId server.Id, change *SecurityNetworkAuthorityChange) {
	guardSecurityNoticeInTx(ctx, tx, SecurityNoticeNetworkAuthority, func() string {
		recipients := securityNoticeAuthorityRecipientsInTx(ctx, tx, networkId)
		return oweSecurityNoticeInTx(ctx, tx, networkId, recipients, SecurityNetworkAuthorityTemplateName, func(templateJson string) (string, error) {
			return applySecurityNoticeChanges(templateJson, (*SecurityNetworkAuthorityNotice).add, []*SecurityNetworkAuthorityChange{change})
		})
	})
}
