package controller

// Stripe account deletion discovers subscriptions from every current local
// mapping plus exact provider metadata before it mutates provider or local
// state. Search is eventually consistent, so this closes missing-local-row
// history but cannot fence a Checkout completion racing the deletion itself.

import (
	"context"
	"errors"
	"fmt"
	"net/url"

	"github.com/urnetwork/server"
)

// Retains one provider subscription and every local invoice that mapped to it.
type stripeDeletionSubscription struct {
	subscription *stripeCustomerSubscription
	invoiceIds   []string
	invoiceIdSet map[string]bool
}

// Reports the provider states that cannot produce another subscription period.
func stripeSubscriptionTerminal(status string) bool {
	switch status {
	case "canceled", "incomplete_expired":
		return true
	}
	return false
}

// Searches all exact-network metadata pages and rejects ambiguous pagination.
func stripeSearchNetworkSubscriptions(ctx context.Context, networkId server.Id) ([]*stripeCustomerSubscription, error) {
	const maxPages = 1000
	queryText := fmt.Sprintf(
		"metadata['%s']:'%s'",
		stripeMetadataNetworkId,
		networkId.String(),
	)
	subs := []*stripeCustomerSubscription{}
	seenSubscriptionIds := map[string]bool{}
	seenPageTokens := map[string]bool{}
	pageToken := ""
	for pageIndex := 0; ; pageIndex += 1 {
		if maxPages <= pageIndex {
			return nil, errors.New("search subscriptions exceeded the page limit")
		}
		query := url.Values{
			"query": []string{queryText},
			"limit": []string{"100"},
		}
		if pageToken != "" {
			query.Set("page", pageToken)
		}
		list, err := server.HttpGetRequireStatusOk[*stripeCustomerSubscriptionList](
			ctx,
			fmt.Sprintf("%s/v1/subscriptions/search?%s", stripeApiBaseUrl, query.Encode()),
			stripeAuthHeader,
			server.ResponseJsonObject[*stripeCustomerSubscriptionList],
		)
		if err != nil {
			return nil, fmt.Errorf("search subscriptions: %w", err)
		}
		if list == nil {
			return nil, errors.New("search subscriptions returned no list")
		}
		for _, sub := range list.Data {
			if sub == nil || sub.Id == "" || sub.Status == "" {
				return nil, errors.New("search subscriptions returned a malformed subscription")
			}
			if sub.Metadata[stripeMetadataNetworkId] != networkId.String() {
				return nil, errors.New("search subscriptions returned mismatched network metadata")
			}
			if !seenSubscriptionIds[sub.Id] {
				seenSubscriptionIds[sub.Id] = true
				subs = append(subs, sub)
			}
		}
		if !list.HasMore {
			if list.NextPage != "" {
				return nil, errors.New("search subscriptions returned an unexpected next page")
			}
			break
		}
		if list.NextPage == "" {
			return nil, errors.New("search subscriptions omitted the next page")
		}
		if seenPageTokens[list.NextPage] {
			return nil, errors.New("search subscriptions returned a pagination cycle")
		}
		seenPageTokens[list.NextPage] = true
		pageToken = list.NextPage
	}
	return subs, nil
}

// Unions invoice, customer, and exact-metadata discovery before cancellation.
func stripeDiscoverDeletionSubscriptions(
	ctx context.Context,
	networkId server.Id,
	customerId string,
	invoiceIds []string,
) ([]*stripeDeletionSubscription, error) {
	deletions := []*stripeDeletionSubscription{}
	deletionBySubscriptionId := map[string]*stripeDeletionSubscription{}
	add := func(sub *stripeCustomerSubscription, invoiceId string) error {
		if sub == nil || sub.Id == "" || sub.Status == "" {
			return errors.New("subscription discovery returned a malformed subscription")
		}
		deletion, ok := deletionBySubscriptionId[sub.Id]
		if !ok {
			deletion = &stripeDeletionSubscription{
				subscription: sub,
				invoiceIdSet: map[string]bool{},
			}
			deletionBySubscriptionId[sub.Id] = deletion
			deletions = append(deletions, deletion)
		} else if !stripeSubscriptionTerminal(deletion.subscription.Status) && stripeSubscriptionTerminal(sub.Status) {
			// Cancellation is irreversible, so a terminal observation is stronger
			// than an earlier nonterminal list or eventually consistent search row.
			deletion.subscription = sub
		}
		if invoiceId != "" && !deletion.invoiceIdSet[invoiceId] {
			deletion.invoiceIdSet[invoiceId] = true
			deletion.invoiceIds = append(deletion.invoiceIds, invoiceId)
		}
		return nil
	}

	for _, invoiceId := range invoiceIds {
		if invoiceId == "" {
			return nil, errors.New("active Stripe renewal has no invoice id")
		}
		sub, err := stripeSubscriptionFromInvoice(ctx, invoiceId)
		if err != nil {
			return nil, fmt.Errorf("discover subscription from invoice: %w", err)
		}
		if err := add(sub, invoiceId); err != nil {
			return nil, err
		}
	}
	if customerId != "" {
		subs, err := stripeListCustomerSubscriptions(ctx, customerId)
		if err != nil {
			return nil, err
		}
		for _, sub := range subs {
			if err := add(sub, ""); err != nil {
				return nil, err
			}
		}
	}
	metadataSubs, err := stripeSearchNetworkSubscriptions(ctx, networkId)
	if err != nil {
		return nil, err
	}
	for _, sub := range metadataSubs {
		if err := add(sub, ""); err != nil {
			return nil, err
		}
	}
	return deletions, nil
}

// Cancels every nonterminal provider object and closes mapped local renewals
// only after an exact terminal subscription has been observed.
func stripeCancelDeletionSubscriptions(
	ctx context.Context,
	deletions []*stripeDeletionSubscription,
	closeRenewal func(invoiceId string) error,
) error {
	for _, deletion := range deletions {
		if deletion == nil || deletion.subscription == nil {
			return errors.New("subscription cancellation received malformed discovery")
		}
		sub := deletion.subscription
		if !stripeSubscriptionTerminal(sub.Status) {
			canceled, err := server.HttpDelete[*stripeCustomerSubscription](
				ctx,
				fmt.Sprintf("%s/v1/subscriptions/%s", stripeApiBaseUrl, url.PathEscape(sub.Id)),
				stripeAuthHeader,
				server.HttpResponseRequireStatusOk[*stripeCustomerSubscription](
					server.ResponseJsonObject[*stripeCustomerSubscription],
				),
			)
			if err != nil {
				return fmt.Errorf("cancel Stripe subscription: %w", err)
			}
			if canceled == nil || canceled.Id != sub.Id || canceled.Status != "canceled" {
				return errors.New("Stripe cancellation did not confirm the requested canceled subscription")
			}
			sub = canceled
		}
		if !stripeSubscriptionTerminal(sub.Status) {
			return errors.New("Stripe subscription did not reach a terminal state")
		}
		for _, invoiceId := range deletion.invoiceIds {
			if closeRenewal == nil {
				return errors.New("local renewal closer is missing")
			}
			if err := closeRenewal(invoiceId); err != nil {
				return err
			}
		}
	}
	return nil
}
