// Legacy attribution requires the processor's token identity, not a name or
// symbol. Every lookup shares the listing's bounded read owner; no process-wide
// token cache can carry another chain or configuration into a later decision.
package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Only these exact exclusions precede asset proof in the decision. An unknown
// asset in the relevant window still holds the entire group, even if failed.
func legacyProviderPaymentRelevantTransfer(attempts []*model.LegacyProviderPaymentAttempt, boundRecords map[string]bool) func(*CircleListedTransaction) bool {
	if len(attempts) == 0 {
		return func(*CircleListedTransaction) bool { return false }
	}
	windowStart := attempts[0].Payment.CreateTime
	for _, attempt := range attempts {
		windowStart = server.MinTime(windowStart, attempt.Payment.CreateTime)
	}
	windowStart = windowStart.Add(-legacyProviderPaymentWindowMargin)
	return func(transaction *CircleListedTransaction) bool {
		return !transaction.CreateDate.Before(windowStart) &&
			!strings.EqualFold(transaction.TransactionType, "INBOUND") &&
			!strings.EqualFold(transaction.CustodyType, "ENDUSER") && !boundRecords[transaction.Id]
	}
}

// Production reads prove only relevant token identities inside the same list
// owner. Test listers supply their own observations, as before.
func listLegacyProviderPaymentTransactions(ctx context.Context, lister circleTransactionLister, blockchain, destination string, attempts []*model.LegacyProviderPaymentAttempt, boundRecords map[string]bool) ([]*CircleListedTransaction, error) {
	if scoped, ok := lister.(interface {
		listDestinationTransactions(context.Context, string, string, func(*CircleListedTransaction) bool) ([]*CircleListedTransaction, error)
	}); ok {
		return scoped.listDestinationTransactions(ctx, blockchain, destination, legacyProviderPaymentRelevantTransfer(attempts, boundRecords))
	}
	return lister.ListDestinationTransactions(ctx, blockchain, destination)
}

// Pointer native status distinguishes a missing field from a non-native asset.
type circlePaymentToken struct {
	Id           string `json:"id"`
	Blockchain   string `json:"blockchain"`
	TokenAddress string `json:"tokenAddress"`
	IsNative     *bool  `json:"isNative"`
}

// The id is bound before metadata can become attribution evidence. Circle's
// token endpoint returns data.token with its chain and contract/mint address.
func (self *CoreCircleApiClient) getPaymentToken(ctx context.Context, id string) (*circlePaymentToken, error) {
	if !validCircleReadId(id) {
		return nil, fmt.Errorf("invalid Circle asset identity")
	}
	uri := "https://api.circle.com/v1/w3s/tokens/" + url.PathEscape(id)
	return paymentHttpGet(ctx, self.readSettings, self.readHooks, uri, self.readHeaders(""),
		func(_ *http.Response, body []byte) (*circlePaymentToken, error) {
			var response CircleResponse[struct {
				Token *circlePaymentToken `json:"token"`
			}]
			if err := json.Unmarshal(body, &response); err != nil {
				return nil, err
			}
			token := response.Data.Token
			if token == nil || token.Id != id || token.Blockchain == "" || token.IsNative == nil {
				return nil, fmt.Errorf("Circle asset observation is missing or belongs to another token")
			}
			return token, nil
		})
}

// Only EVM address casing is insignificant. Solana mint addresses are
// case-sensitive; a matching symbol on any chain is never asset authority.
func circlePaymentTokenMatches(token *circlePaymentToken, blockchain, address string) bool {
	if token == nil || token.Blockchain != blockchain || token.IsNative == nil || *token.IsNative ||
		address == "" || token.TokenAddress == "" {
		return false
	}
	switch blockchain {
	case "MATIC":
		return strings.EqualFold(token.TokenAddress, address)
	case "SOL":
		return token.TokenAddress == address
	default:
		return false
	}
}

// Missing or foreign assets remain unverified, so even a failed transfer cannot
// prove absence. An unavailable metadata read invalidates the whole listing.
func (self *CoreCircleApiClient) verifyListedUsdcAssets(ctx context.Context, blockchain string, transactions []*CircleListedTransaction, relevant func(*CircleListedTransaction) bool) error {
	addressSource := self.readUsdcAddress
	if addressSource == nil {
		addressSource = getUsdcAddressByNetwork
	}
	address := ""
	tokens := map[string]*circlePaymentToken{}
	for _, transaction := range transactions {
		transaction.usdcAssetVerified = false
		if relevant != nil && !relevant(transaction) {
			continue
		}
		if !validCircleReadId(transaction.TokenId) {
			continue
		}
		if address == "" {
			var err error
			address, err = addressSource(blockchain)
			if err != nil || address == "" || strings.TrimSpace(address) != address {
				return fmt.Errorf("configured Circle payout asset is unavailable")
			}
		}
		token, ok := tokens[transaction.TokenId]
		if !ok {
			if len(tokens) == maximumCircleTokens {
				return fmt.Errorf("Circle listing asset capacity exhausted")
			}
			var err error
			token, err = self.getPaymentToken(ctx, transaction.TokenId)
			if err != nil {
				return err
			}
			tokens[transaction.TokenId] = token
		}
		transaction.usdcAssetVerified = circlePaymentTokenMatches(token, blockchain, address)
	}
	return nil
}
