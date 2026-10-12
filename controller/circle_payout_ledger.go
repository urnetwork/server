// The one-time sn-reconcile census reads the payout wallets' own Circle
// history. A separate interface keeps the existing CircleApi fakes compiling.
package controller

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math/big"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/urnetwork/server/model"
)

// 2.5M items at circleReadPageSize per page, far beyond the weekly payout
// history since the earliest stuck row or the entity's wallets. More pages is
// an error.
const maximumCirclePayoutLedgerPages = 50_000

type CirclePayoutLedger interface {
	// Every transaction of one developer wallet created at or after `from`, in
	// every state. An incomplete listing is an error, never a short result.
	ListWalletTransactions(ctx context.Context, walletId string, from time.Time) ([]CircleTransaction, error)
	// The wallet's balance of the token at `tokenAddress`; zero when absent.
	GetWalletTokenBalance(ctx context.Context, walletId string, tokenAddress string) (model.NanoCents, error)
	// Every developer-controlled wallet of the entity, on every blockchain and
	// in every state. An incomplete listing is an error, never a short result.
	ListDeveloperWallets(ctx context.Context) ([]CircleWallet, error)
}

var circlePayoutLedgerInstance CirclePayoutLedger = &CoreCircleApiClient{}

func NewCirclePayoutLedger() CirclePayoutLedger {
	return circlePayoutLedgerInstance
}

// for stubbing in tests
func SetCirclePayoutLedger(ledger CirclePayoutLedger) {
	circlePayoutLedgerInstance = ledger
}

// Every page of one collection, following only validated Link cursors. A
// repeated page or more than `maximumPages` pages fails the whole read, and a
// dead context fails it before the configuration is read.
func circleReadEveryPage[R any](ctx context.Context, client *CoreCircleApiClient, first string, key string, maximumPages int, readNext func(http.Header) (string, error), visit func(R) error) error {
	if ctx == nil {
		return fmt.Errorf("Circle %s listing requires a context", key)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	headers := client.readHeaders("")
	seenPages := map[string]bool{}
	for uri := first; uri != ""; {
		if seenPages[uri] {
			return fmt.Errorf("Circle %s pagination repeated a page", key)
		}
		if len(seenPages) == maximumPages {
			return fmt.Errorf("Circle %s exceed %d pages", key, maximumPages)
		}
		seenPages[uri] = true
		var next string
		page, err := paymentHttpGet(ctx, client.readSettings, client.readHooks, uri, headers, func(response *http.Response, body []byte) ([]R, error) {
			// Circle documents self and first links on every page, so a page
			// without any Link cannot be told apart from a truncated listing.
			if len(response.Header.Values("Link")) == 0 {
				return nil, fmt.Errorf("Circle %s page has no pagination Link", key)
			}
			var err error
			if next, err = readNext(response.Header); err != nil {
				return nil, err
			}
			return circleReadPage[R](body, key)
		})
		if err != nil {
			return err
		}
		for _, item := range page {
			if err := visit(item); err != nil {
				return err
			}
		}
		uri = next
	}
	return nil
}

// Circle may omit our fixed filters from a next link. circleReadNext accepts
// a link only when it repeats exactly the base's filters unchanged, so each
// subset of ours is tried as the base: the link passes under the subset it
// repeats, and under none when it changes one or adds another. Only its
// cursor and page size are taken, under every filter of ours.
func circleReadNextUnderFilters(endpoint string, filters url.Values, header http.Header) (string, error) {
	keys := []string{}
	for key := range filters {
		if key != "pageSize" {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	seen := map[string]bool{}
	refusals := []error{}
	for subset := 0; subset < 1<<len(keys); subset++ {
		base := url.Values{}
		for i, key := range keys {
			if subset&(1<<i) != 0 {
				base[key] = filters[key]
			}
		}
		next, err := circleReadNext(endpoint+"?"+base.Encode(), header)
		if err != nil {
			if !seen[err.Error()] {
				seen[err.Error()] = true
				refusals = append(refusals, err)
			}
			continue
		}
		if next == "" {
			return "", nil
		}
		// circleReadNext re-encoded this query, and it holds exactly one cursor.
		_, query, _ := strings.Cut(next, "?")
		link := (&url.URL{RawQuery: query}).Query()
		rebuilt := maps.Clone(filters)
		for _, key := range []string{"pageSize", "pageAfter", "pageBefore"} {
			if value := link.Get(key); value != "" {
				rebuilt.Set(key, value)
			}
		}
		return endpoint + "?" + rebuilt.Encode(), nil
	}
	return "", errors.Join(refusals...)
}

// No state, type or operation filter applies, and `includeAll` also lists
// unmonitored tokens. Circle's inclusive `from` takes whole seconds, so the
// query floors it and drops transactions created in that second before `from`.
// One read budget cannot span the payout history: each page is its own read,
// and the page bound limits the listing.
func (self *CoreCircleApiClient) ListWalletTransactions(ctx context.Context, walletId string, from time.Time) ([]CircleTransaction, error) {
	// `walletIds` is a comma-separated list; the filter must name one wallet.
	if !validCircleReadId(walletId) || strings.Contains(walletId, ",") || from.IsZero() {
		return nil, fmt.Errorf("invalid Circle wallet transaction listing arguments")
	}
	since := from.UTC().Truncate(time.Second)
	endpoint := "https://api.circle.com/v1/w3s/transactions"
	filters := url.Values{
		"walletIds":  {walletId},
		"from":       {since.Format(time.RFC3339)},
		"includeAll": {"true"},
		"pageSize":   {strconv.Itoa(circleReadPageSize)},
	}
	readNext := func(header http.Header) (string, error) {
		return circleReadNextUnderFilters(endpoint, filters, header)
	}
	seen := map[string]bool{}
	transactions := []CircleTransaction{}
	err := circleReadEveryPage(ctx, self, endpoint+"?"+filters.Encode(), "transactions", maximumCirclePayoutLedgerPages, readNext, func(transaction CircleTransaction) error {
		created, err := time.Parse(time.RFC3339, transaction.CreateDate)
		if err != nil || !validCircleReadId(transaction.Id) || seen[transaction.Id] || transaction.WalletId != walletId || created.Before(since) {
			return fmt.Errorf("Circle wallet %s listing has an invalid, repeated, foreign or early transaction %.64q", walletId, transaction.Id)
		}
		seen[transaction.Id] = true
		if !created.Before(from) {
			transactions = append(transactions, transaction)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return transactions, nil
}

// The API key alone lists the entity's wallets. Circle documents no custody
// filter, so a user-controlled wallet in the response is validated like every
// other and left out: it is never one of our own destinations. Each page is
// its own read, as in the transaction listing.
func (self *CoreCircleApiClient) ListDeveloperWallets(ctx context.Context) ([]CircleWallet, error) {
	endpoint := "https://api.circle.com/v1/w3s/wallets"
	filters := url.Values{"pageSize": {strconv.Itoa(circleReadPageSize)}}
	readNext := func(header http.Header) (string, error) {
		return circleReadNextUnderFilters(endpoint, filters, header)
	}
	seen := map[string]bool{}
	wallets := []CircleWallet{}
	err := circleReadEveryPage(ctx, self, endpoint+"?"+filters.Encode(), "wallets", maximumCirclePayoutLedgerPages, readNext, func(wallet CircleWallet) error {
		if !validCircleReadWallet(&wallet) || seen[wallet.ID] || (wallet.CustodyType != "DEVELOPER" && wallet.CustodyType != "ENDUSER") {
			return fmt.Errorf("Circle wallet listing has an invalid, repeated or unclassified wallet %.64q", wallet.ID)
		}
		seen[wallet.ID] = true
		if wallet.CustodyType == "DEVELOPER" {
			wallets = append(wallets, wallet)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return wallets, nil
}

type circleLedgerBalance struct {
	Amount string `json:"amount"`
	Token  *struct {
		Id           string `json:"id"`
		TokenAddress string `json:"tokenAddress"`
	} `json:"token"`
}

// One read lists every token, unmonitored ones included. A partial, malformed
// or ambiguous listing is an error, never a zero balance.
func (self *CoreCircleApiClient) GetWalletTokenBalance(ctx context.Context, walletId string, tokenAddress string) (model.NanoCents, error) {
	ctx, cancel, err := self.beginReads(ctx)
	if err != nil {
		return 0, err
	}
	defer cancel()
	if !validCircleReadId(walletId) || tokenAddress == "" || len(tokenAddress) > 512 {
		return 0, fmt.Errorf("invalid Circle wallet balance arguments")
	}
	first := "https://api.circle.com/v1/w3s/wallets/" + url.PathEscape(walletId) + "/balances?" + url.Values{
		"includeAll": {"true"},
		"pageSize":   {strconv.Itoa(circleReadPageSize)},
	}.Encode()
	readNext := func(header http.Header) (string, error) {
		return circleReadNext(first, header)
	}
	seen := map[string]bool{}
	var balance *big.Rat
	err = circleReadEveryPage(ctx, self, first, "tokenBalances", maximumCircleObservationPages, readNext, func(entry circleLedgerBalance) error {
		if entry.Token == nil || !validCircleReadId(entry.Token.Id) || seen[entry.Token.Id] || len(seen) == maximumCircleTokens {
			return fmt.Errorf("Circle balance listing has a missing, repeated or excess token")
		}
		seen[entry.Token.Id] = true
		amount, err := circleObservedAmount(entry.Amount)
		if err != nil {
			return err
		}
		if strings.EqualFold(entry.Token.TokenAddress, tokenAddress) {
			if balance != nil {
				return fmt.Errorf("Circle token balance is ambiguous")
			}
			balance = amount
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	if balance == nil {
		return 0, nil
	}
	scaled := new(big.Rat).Mul(balance, big.NewRat(1_000_000_000, 1))
	if !scaled.IsInt() || !scaled.Num().IsInt64() {
		return 0, fmt.Errorf("Circle token balance exceeds exact supported precision/range")
	}
	return model.NanoCents(scaled.Num().Int64()), nil
}
