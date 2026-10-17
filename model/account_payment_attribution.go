// Circle attribution spans payments, not just one attempt row. Retaining a
// request and deciding a legacy destination share ownership before BEGIN;
// every record writer also owns the processor id before checking its history.
package model

import (
	"context"
	"strings"

	"github.com/urnetwork/server"
)

// Chain aliases and EVM address casing denote one physical destination. Solana
// addresses remain case-sensitive. Wallet ids alone do not identify this scope.
func providerPaymentDestination(blockchain, address string) (string, string) {
	chain := strings.ToUpper(strings.TrimSpace(blockchain))
	switch chain {
	case "POLY", "POLYGON":
		chain = "MATIC"
	case "SOLANA":
		chain = "SOL"
	}
	if chain == "MATIC" {
		address = strings.ToLower(address)
	}
	return chain, address
}

// The complete key set is admitted by OwnedTx before any payment row lock.
func providerPaymentAttributionKeys(paymentId server.Id, blockchain, address, record string) []server.PgOwnershipKey {
	keys := []server.PgOwnershipKey{server.NewPgOwnershipKey("account_payment", paymentId)}
	if address != "" {
		chain, destination := providerPaymentDestination(blockchain, address)
		keys = append(keys, server.NewPgOwnershipKeyFromString("circle_payment_destination", chain+"\x00"+destination))
	}
	if record != "" {
		keys = append(keys, server.NewPgOwnershipKeyFromString("circle_payment_record", record))
	}
	return keys
}

// The wallet's durable address is read before ownership admission, then checked
// again in the transaction. No PostgreSQL checkout is retained across admission.
func legacyProviderPaymentWallet(ctx context.Context, expected *AccountPayment) (*AccountWallet, error) {
	if expected == nil || expected.PaymentId == (server.Id{}) || expected.WalletId == nil {
		return nil, ErrProviderPaymentAttemptChanged
	}
	wallet := GetAccountWallet(ctx, *expected.WalletId)
	if wallet == nil || wallet.NetworkId != expected.NetworkId || wallet.WalletAddress == "" || wallet.Blockchain == "" {
		return nil, ErrProviderPaymentAttemptChanged
	}
	return wallet, nil
}

// Called while holding the destination key shared with both request retainers.
// A fresh retained request, including one whose processor ACK is lost, refuses
// legacy binding, release and correction. Current records are checked separately.
func lockLegacyProviderPaymentAttributionInTx(ctx context.Context, tx server.PgTx, expected *AccountPayment, wallet *AccountWallet) bool {
	if !lockLegacyProviderPaymentAttemptInTx(ctx, tx, expected) {
		return false
	}
	var exact bool
	server.Raise(tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM account_wallet
		WHERE wallet_id=$1 AND network_id=$2 AND blockchain=$3 AND wallet_address=$4)`,
		wallet.WalletId, expected.NetworkId, wallet.Blockchain, wallet.WalletAddress).Scan(&exact))
	if !exact {
		return false
	}
	chain, address := providerPaymentDestination(wallet.Blockchain, wallet.WalletAddress)
	var pending bool
	server.Raise(tx.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM account_payment p JOIN account_wallet w ON w.wallet_id=p.wallet_id
		JOIN audit_account_payment request ON request.event_id=p.circle_idempotency_key AND request.payment_id=p.payment_id
		WHERE NOT p.completed AND NOT p.canceled AND p.payment_record IS NULL
		AND request.event_type='circle_attempt_request'
		AND CASE upper(trim(w.blockchain)) WHEN 'POLY' THEN 'MATIC' WHEN 'POLYGON' THEN 'MATIC'
			WHEN 'SOLANA' THEN 'SOL' ELSE upper(trim(w.blockchain)) END=$1
		AND CASE WHEN $1='MATIC' THEN lower(w.wallet_address) ELSE w.wallet_address END=$2)`, chain, address).Scan(&pending))
	return !pending
}

// Accepted records are evidence even before an outcome journal can copy them
// from the current row, and remain owned after markers are reset. The response
// is JSON only for these writer-controlled statuses; arbitrary failure text is
// never cast. Existing duplicate ownership is refused, not migrated away.
const providerPaymentProcessorOwnershipSql = `
	SELECT payment_id, circle_idempotency_key::text attempt_key, payment_record record
	FROM account_payment WHERE payment_record IS NOT NULL
	UNION ALL
	SELECT payment_id, event_details::jsonb->>'idempotency_key', event_details::jsonb->'attempt'->>'payment_record'
	FROM audit_account_payment WHERE event_type='circle_attempt_outcome'
	UNION ALL
	SELECT payment_id, event_details::jsonb->'basis'->>'IdempotencyKey', event_details::jsonb->>'record'
	FROM audit_account_payment WHERE event_type='circle_late_acceptance'
	UNION ALL
	SELECT payment_id, event_details::jsonb->>'idempotency_key',
		(CASE WHEN event_details::jsonb->>'status' IN ('ACCEPTED','LEGACY_RECONCILED_FOUND','CONTRADICTORY_ACCEPTANCE')
		THEN (event_details::jsonb->>'response')::jsonb ELSE NULL END)->>'record'
	FROM audit_account_payment WHERE event_type='circle_attempt_outcome'
`

// A processor id can only repeat for the same payment and idempotency key.
// nil expected deliberately refuses every owner (the legacy first binding).
func providerPaymentRecordClaimedInTx(ctx context.Context, tx server.PgTx, record string, expected *AccountPayment) bool {
	var paymentId *server.Id
	var key *string
	if expected != nil {
		paymentId = &expected.PaymentId
		if expected.CircleIdempotencyKey != nil {
			value := expected.CircleIdempotencyKey.String()
			key = &value
		}
	}
	var claimed bool
	server.Raise(tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM (`+providerPaymentProcessorOwnershipSql+`) owned
		WHERE record=$1 AND ($2::uuid IS NULL OR payment_id IS DISTINCT FROM $2::uuid OR attempt_key IS DISTINCT FROM $3::text))`,
		record, paymentId, key).Scan(&claimed))
	return claimed
}
