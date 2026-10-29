package controller

// The security notices (model/security_notice_model.go, owner decision
// 2026-10-29): the payout wallet notice, the sign-in method notice and the API
// key notice, owed by the model in the transaction of the change and delivered
// by the account message outbox. They are email only: none is in
// smsTemplateNames, and the model owes them to email addresses only. The
// password-changed notice is AuthPasswordSetTemplate.

import (
	"strconv"
	"strings"
	"time"

	"github.com/urnetwork/server/model"
)

// The line every security notice carries, word for word.
const securityNoticeContactLine = "If you didn't make this change, contact security@ur.io right away."

// Where a reader reviews what changed, as the templates link it.
const (
	securityNoticePayoutWalletsUrl = "https://ur.io/app/account/earnings"
	securityNoticeSignInMethodsUrl = "https://ur.io/app/account/profile"
	securityNoticeOtherSessionsUrl = "https://ur.io/app/account/sessions"
	// the ur.io app's API keys screen, where the owner sees and deletes them
	// (owner decision 2026-10-29); the notice ships once the screen is live
	securityNoticeApiKeysUrl = "https://ur.io/app/account/api-keys"
)

const (
	securityNoticeTimeLayout        = "January 2, 2006, 15:04:05 UTC"
	securityNoticeNoWallet          = "None"
	securityNoticeUnknownWalletName = "Payout wallet"
)

// The line for changes made between the listed ones and not listed, or "".
func securityNoticeMoreChanges(moreCount int) string {
	switch {
	case moreCount <= 0:
		return ""
	case moreCount == 1:
		return "1 more change was made between these and is not listed. The last change listed is the latest."
	default:
		return strconv.Itoa(moreCount) + " more changes were made between these and are not listed. The last change listed is the latest."
	}
}

// The time of a change as a notice shows it, in UTC.
func securityNoticeTime(changeTime time.Time) string {
	if changeTime.IsZero() {
		return ""
	}
	return changeTime.UTC().Format(securityNoticeTimeLayout)
}

// How a change was made, as a notice says it, or "" when unknown.
func securityNoticeVia(via string) string {
	switch via {
	case model.SecurityNoticeViaAndroid:
		return "The URnetwork app for Android"
	case model.SecurityNoticeViaIos:
		return "The URnetwork app for iOS"
	case model.SecurityNoticeViaMacos:
		return "The URnetwork app for macOS"
	case model.SecurityNoticeViaWindows:
		return "The URnetwork app for Windows"
	case model.SecurityNoticeViaLinux:
		return "The URnetwork app for Linux"
	case model.SecurityNoticeViaCli:
		return "The URnetwork command-line tool"
	case model.SecurityNoticeViaWeb:
		return "The ur.io website"
	case model.SecurityNoticeViaApiKey:
		return "An API key"
	default:
		return ""
	}
}

// A chain as a notice names it.
func securityNoticeChainName(chain string) string {
	switch strings.ToUpper(chain) {
	case "SOL":
		return "Solana"
	case "MATIC":
		return "Polygon"
	case "TAO":
		return "Bittensor"
	case "ETH":
		return "Ethereum"
	default:
		return chain
	}
}

// A masked wallet with its chain ("Solana 9WzD…AWWM"), or "None".
func securityNoticeWallet(chain string, wallet string) string {
	if wallet == "" {
		return securityNoticeNoWallet
	}
	if name := securityNoticeChainName(chain); name != "" {
		return name + " " + wallet
	}
	return wallet
}

// Where payouts go, as a notice names it.
func securityNoticeDestinationName(destination string) string {
	switch destination {
	case model.SecurityWalletDestinationUsdc:
		return "USDC payout wallet"
	case model.SecurityWalletDestinationSubnetNetwork:
		return "Bittensor wallet for your network's subnet earnings"
	case model.SecurityWalletDestinationSubnetProvider:
		return "Bittensor wallet for one of your providers"
	case model.SecurityWalletDestinationSubnetNetworkConsent:
		return "Bittensor wallet approved for your network's subnet earnings"
	case model.SecurityWalletDestinationSubnetHotkey:
		return "Bittensor hotkey for your network's subnet earnings"
	default:
		return securityNoticeUnknownWalletName
	}
}

// The epochs a consent covers ("12 to 20"), or "".
func securityNoticeEpochs(fromEpoch uint64, throughEpoch uint64) string {
	switch {
	case fromEpoch == 0 && throughEpoch == 0:
		return ""
	case throughEpoch == 0 || throughEpoch == fromEpoch:
		return strconv.FormatUint(fromEpoch, 10)
	default:
		return strconv.FormatUint(fromEpoch, 10) + " to " + strconv.FormatUint(throughEpoch, 10)
	}
}

// One wallet change as the payout wallet notice shows it.
type SecurityWalletChangeRow struct {
	When   string
	Wallet string
	Before string
	After  string
	// the epochs of a consent, or ""
	Epochs string
	// how it was made, or ""
	Via string
}

// The payout wallet notice: the wallet payouts go to was set, replaced or
// removed, on any chain: the USDC payout wallet and the subnet's Bittensor
// wallets.
type SecurityPayoutWalletChangedTemplate struct {
	model.SecurityWalletNotice
	BaseTemplate
}

func (self *SecurityPayoutWalletChangedTemplate) Name() string {
	return model.SecurityPayoutWalletTemplateName
}

// The changes, oldest first.
func (self *SecurityPayoutWalletChangedTemplate) Rows() []*SecurityWalletChangeRow {
	rows := []*SecurityWalletChangeRow{}
	for _, change := range self.Changes {
		if change == nil {
			continue
		}
		rows = append(rows, &SecurityWalletChangeRow{
			When:   securityNoticeTime(change.ChangeTime),
			Wallet: securityNoticeDestinationName(change.Destination),
			Before: securityNoticeWallet(change.PreviousChain, change.PreviousWallet),
			After:  securityNoticeWallet(change.NewChain, change.NewWallet),
			Epochs: securityNoticeEpochs(change.FromEpoch, change.ThroughEpoch),
			Via:    securityNoticeVia(change.Via),
		})
	}
	return rows
}

// Whether the notice lists more than one change.
func (self *SecurityPayoutWalletChangedTemplate) Several() bool {
	return 1 < len(self.Changes) || 0 < self.MoreCount
}

func (self *SecurityPayoutWalletChangedTemplate) MoreChangesText() string {
	return securityNoticeMoreChanges(self.MoreCount)
}

// One sign-in method change as the sign-in notice shows it.
type SecuritySignInChangeRow struct {
	When string
	// the method, with its masked email, phone number or wallet
	Method string
	Action string
	// how it was made, or ""
	Via string
}

// The sign-in method notice: a way to sign in to the account was added,
// removed or replaced.
type SecuritySignInChangedTemplate struct {
	model.SecuritySignInNotice
	BaseTemplate
}

func (self *SecuritySignInChangedTemplate) Name() string {
	return model.SecuritySignInTemplateName
}

// A sign-in method as the notice names it, as the apps' Profile screen does.
func securityNoticeSignInMethod(method string) string {
	switch method {
	case model.SecuritySignInMethodEmail:
		return "Email"
	case model.SecuritySignInMethodPhone:
		return "Phone"
	case model.SecuritySignInMethodApple:
		return "Apple"
	case model.SecuritySignInMethodGoogle:
		return "Google"
	case model.SecuritySignInMethodSolana:
		return "Solana wallet"
	case model.SecuritySignInMethodBittensor:
		return "Bittensor wallet"
	case model.SecuritySignInMethodSeedphrase:
		return "Seedphrase"
	default:
		return "Sign-in method"
	}
}

// What happened to a sign-in method, as the notice says it.
func securityNoticeSignInAction(action string) string {
	switch action {
	case model.SecuritySignInAdded:
		return "Added"
	case model.SecuritySignInRemoved:
		return "Removed"
	case model.SecuritySignInReplaced:
		return "Replaced"
	default:
		return "Changed"
	}
}

// The changes, oldest first.
func (self *SecuritySignInChangedTemplate) Rows() []*SecuritySignInChangeRow {
	rows := []*SecuritySignInChangeRow{}
	for _, change := range self.Changes {
		if change == nil {
			continue
		}
		method := securityNoticeSignInMethod(change.Method)
		if change.Identity != "" {
			method += " (" + change.Identity + ")"
		}
		rows = append(rows, &SecuritySignInChangeRow{
			When:   securityNoticeTime(change.ChangeTime),
			Method: method,
			Action: securityNoticeSignInAction(change.Action),
			Via:    securityNoticeVia(change.Via),
		})
	}
	return rows
}

// The one action every listed change took, or "" when they differ.
func (self *SecuritySignInChangedTemplate) onlyAction() string {
	action := ""
	for _, change := range self.Changes {
		if change == nil {
			continue
		}
		if action != "" && action != change.Action {
			return ""
		}
		action = change.Action
	}
	return action
}

// The subject: what happened, without the details.
func (self *SecuritySignInChangedTemplate) Subject() string {
	several := 1 < len(self.Changes) || 0 < self.MoreCount
	switch self.onlyAction() {
	case model.SecuritySignInAdded:
		if several {
			return "Sign-in methods were added to your URnetwork account"
		}
		return "A sign-in method was added to your URnetwork account"
	case model.SecuritySignInRemoved:
		if several {
			return "Sign-in methods were removed from your URnetwork account"
		}
		return "A sign-in method was removed from your URnetwork account"
	default:
		return "Your URnetwork sign-in methods were changed"
	}
}

// The headline: what happened, in a few words.
func (self *SecuritySignInChangedTemplate) Headline() string {
	switch self.onlyAction() {
	case model.SecuritySignInAdded:
		return "Sign-in method added"
	case model.SecuritySignInRemoved:
		return "Sign-in method removed"
	default:
		return "Sign-in methods changed"
	}
}

// The headline as the plain-text part sets it, in capitals.
func (self *SecuritySignInChangedTemplate) TextHeadline() string {
	return strings.ToUpper(self.Headline())
}

func (self *SecuritySignInChangedTemplate) MoreChangesText() string {
	return securityNoticeMoreChanges(self.MoreCount)
}

// One API key change as the API key notice shows it.
type SecurityApiKeyChangeRow struct {
	When string
	// the key's name in quotes, or "(no name)"
	Key string
	// when a deleted key was created, or ""
	KeyCreated string
	Action     string
	// how it was made, or ""
	Via string
}

// The API key notice: an API key was created for the account or deleted from
// it. It names each key by its name and creation time, never by the key.
type SecurityApiKeyChangedTemplate struct {
	model.SecurityApiKeyNotice
	BaseTemplate
}

func (self *SecurityApiKeyChangedTemplate) Name() string {
	return model.SecurityApiKeyTemplateName
}

// An API key's name as the notice shows it: in quotes, kept to one line
// (model.SecurityNoticeApiKeyName), or "(no name)".
func securityNoticeApiKeyName(name string) string {
	name = model.SecurityNoticeApiKeyName(name)
	if name == "" {
		return "(no name)"
	}
	return "“" + name + "”"
}

// What happened to an API key, as the notice says it.
func securityNoticeApiKeyAction(action string) string {
	switch action {
	case model.SecurityApiKeyCreated:
		return "Created"
	case model.SecurityApiKeyDeleted:
		return "Deleted"
	default:
		return "Changed"
	}
}

// The changes, oldest first.
func (self *SecurityApiKeyChangedTemplate) Rows() []*SecurityApiKeyChangeRow {
	rows := []*SecurityApiKeyChangeRow{}
	for _, change := range self.Changes {
		if change == nil {
			continue
		}
		row := &SecurityApiKeyChangeRow{
			When:   securityNoticeTime(change.ChangeTime),
			Key:    securityNoticeApiKeyName(change.Name),
			Action: securityNoticeApiKeyAction(change.Action),
			Via:    securityNoticeVia(change.Via),
		}
		if change.KeyCreateTime != nil {
			row.KeyCreated = securityNoticeTime(*change.KeyCreateTime)
		}
		rows = append(rows, row)
	}
	return rows
}

// The one action every listed change took, or "" when they differ.
func (self *SecurityApiKeyChangedTemplate) onlyAction() string {
	action := ""
	for _, change := range self.Changes {
		if change == nil {
			continue
		}
		if action != "" && action != change.Action {
			return ""
		}
		action = change.Action
	}
	return action
}

// Whether the notice lists more than one change.
func (self *SecurityApiKeyChangedTemplate) several() bool {
	return 1 < len(self.Changes) || 0 < self.MoreCount
}

// The subject: what happened, without the details.
func (self *SecurityApiKeyChangedTemplate) Subject() string {
	switch self.onlyAction() {
	case model.SecurityApiKeyCreated:
		if self.several() {
			return "API keys were created for your URnetwork account"
		}
		return "An API key was created for your URnetwork account"
	case model.SecurityApiKeyDeleted:
		if self.several() {
			return "API keys were deleted from your URnetwork account"
		}
		return "An API key was deleted from your URnetwork account"
	default:
		return "Your URnetwork API keys were changed"
	}
}

// The headline: what happened, in a few words.
func (self *SecurityApiKeyChangedTemplate) Headline() string {
	switch self.onlyAction() {
	case model.SecurityApiKeyCreated:
		if self.several() {
			return "API keys created"
		}
		return "API key created"
	case model.SecurityApiKeyDeleted:
		if self.several() {
			return "API keys deleted"
		}
		return "API key deleted"
	default:
		return "API keys changed"
	}
}

// The headline as the plain-text part sets it, in capitals.
func (self *SecurityApiKeyChangedTemplate) TextHeadline() string {
	return strings.ToUpper(self.Headline())
}

// Whether a listed change created a key, which can then act for the account.
func (self *SecurityApiKeyChangedTemplate) AnyCreated() bool {
	for _, change := range self.Changes {
		if change != nil && change.Action == model.SecurityApiKeyCreated {
			return true
		}
	}
	return false
}

func (self *SecurityApiKeyChangedTemplate) MoreChangesText() string {
	return securityNoticeMoreChanges(self.MoreCount)
}

// The password-changed notice's details: when and how the password changed.
// A notice owed before they were recorded shows neither.
func (self *AuthPasswordSetTemplate) When() string {
	if self.ChangeTime == nil {
		return ""
	}
	return securityNoticeTime(*self.ChangeTime)
}

func (self *AuthPasswordSetTemplate) ViaText() string {
	return securityNoticeVia(self.Via)
}
