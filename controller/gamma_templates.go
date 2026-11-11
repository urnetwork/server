package controller

// The emails of Pay with Gamma (email_templates/gamma_*): the receipt of a
// credited intent and the one notice of an automatic charge the chain
// skipped. Both go through the account message outbox to the network's
// admin.

import (
	texttemplate "text/template"

	"github.com/urnetwork/server/model"
)

// GammaReceiptTemplate is the receipt of a purchase paid from a gamma
// deposit, the customer's or an automatic renewal, top-up or upgrade.
type GammaReceiptTemplate struct {
	Description      string
	AmountGamma      string
	Purpose          string
	Reference        string
	Block            string
	Pro              bool
	BalanceByteCount model.ByteCount
	BaseTemplate
}

func (self *GammaReceiptTemplate) Name() string {
	return "gamma_receipt"
}

func (self *GammaReceiptTemplate) Funcs(funcs texttemplate.FuncMap) {
	self.BaseTemplate.Funcs(funcs)
	funcs["Balance"] = self.Balance
	funcs["Automatic"] = self.Automatic
	funcs["PurposeText"] = self.PurposeText
}

// Balance is the data added, or "" for a plan.
func (self *GammaReceiptTemplate) Balance() string {
	if self.BalanceByteCount <= 0 {
		return ""
	}
	return model.ByteCountHumanReadable(self.BalanceByteCount)
}

// Automatic reports whether the biller made the purchase.
func (self *GammaReceiptTemplate) Automatic() bool {
	return self.Purpose != "" && self.Purpose != model.GammaPurposePurchase
}

func (self *GammaReceiptTemplate) PurposeText() string {
	return gammaPurposeText(self.Purpose)
}

// GammaChargeSkippedTemplate is the notice that an automatic charge could
// not be spent from the deposit, with what to change.
type GammaChargeSkippedTemplate struct {
	Description   string
	AmountGamma   string
	Purpose       string
	Reason        string
	DepositHotkey string
	Netuid        uint16
	ServiceHotkey string
	BaseTemplate
}

func (self *GammaChargeSkippedTemplate) Name() string {
	return "gamma_charge_skipped"
}

func (self *GammaChargeSkippedTemplate) Funcs(funcs texttemplate.FuncMap) {
	self.BaseTemplate.Funcs(funcs)
	funcs["PurposeText"] = self.PurposeText
	funcs["ReasonText"] = self.ReasonText
	funcs["FixText"] = self.FixText
}

func (self *GammaChargeSkippedTemplate) PurposeText() string {
	return gammaPurposeText(self.Purpose)
}

func (self *GammaChargeSkippedTemplate) ReasonText() string {
	switch self.Reason {
	case GammaErrorCodeInsufficientGamma:
		return "the deposit holds less gamma than the price."
	case GammaErrorCodeDailyCapExceeded:
		return "the deposit's daily spend cap does not allow the price now."
	case GammaErrorCodeWrongSpender:
		return "the deposit names another spender."
	case GammaErrorCodeNoDeposit:
		return "there is no deposit at the hotkey."
	case "spend_failed":
		return "the spend did not run."
	}
	return "the spend was refused."
}

func (self *GammaChargeSkippedTemplate) FixText() string {
	switch self.Reason {
	case GammaErrorCodeInsufficientGamma, GammaErrorCodeNoDeposit:
		return "Mint more SN25 Gamma into the deposit hotkey below, or turn the automatic charge off through the API."
	case GammaErrorCodeDailyCapExceeded:
		return "Raise the deposit's daily spend cap to at least " + self.AmountGamma + " gamma, or remove it."
	case GammaErrorCodeWrongSpender:
		return "Point the deposit back at URnetwork's service with btcli gamma set-spender --netuid 25 --hotkey <the deposit hotkey> --service-hotkey " + self.ServiceHotkey + "."
	}
	return "Check the deposit with btcli query gamma-deposit --netuid 25 --hotkey <the deposit hotkey>."
}

func gammaPurposeText(purpose string) string {
	switch purpose {
	case model.GammaPurposeRenew:
		return "renewal"
	case model.GammaPurposeTopup:
		return "data top-up"
	case model.GammaPurposeSeat:
		return "upgrade at your client limit"
	}
	return "purchase"
}
