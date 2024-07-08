package controller

import (
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math"
	"math/big"
	"net/http"
	"sync"
	"time"

	"bringyour.com/bringyour"
	"bringyour.com/bringyour/model"
	"bringyour.com/bringyour/session"
	"bringyour.com/bringyour/task"
	"github.com/go-jose/go-jose/v3"
	"github.com/go-jose/go-jose/v3/jwt"
)

var (
	// processedPayments keeps track of payments that are being processed or have been processed
	processedPayments = make(map[bringyour.Id]struct{})
	// mu protects access to processedPayments
	mu sync.Mutex
)

func isBeingProcessed(paymentId bringyour.Id) bool {
	mu.Lock()
	defer mu.Unlock()

	_, exists := processedPayments[paymentId]
	return exists
}

func markAsProcessed(paymentId bringyour.Id) {
	mu.Lock()
	defer mu.Unlock()

	processedPayments[paymentId] = struct{}{}
}


// run once on startup
func SchedulePendingPayments(session *session.ClientSession) {

	pendingPayments := model.GetPendingPayments(session.Ctx)

	// schedule a task for CoinbasePayment for each paymentId
	// (use balk because the payment id might already be worked on)
	for _, payment := range pendingPayments {
		if isBeingProcessed(payment.PaymentId) || payment.Completed || payment.Canceled  {
			continue
		}

		markAsProcessed(payment.PaymentId)

		task.ScheduleTask(
			CoinbasePayment,
			&CoinbasePaymentArgs{
				Payment: payment,
			},
			session,
		)

	}
}

// runs twice a day
func SendPayments(session *session.ClientSession) {

	plan := model.PlanPayments(session.Ctx)

	// create coinbase payment records
	// schedule a task for CoinbasePayment for each paymentId
	// (use balk because the payment id might already be worked on)
	for _, payment := range plan.WalletPayments { // STU_TODO: is plan.WalletPayments the correct field?
		if isBeingProcessed(payment.PaymentId) || payment.Completed || payment.Canceled {
			continue
		}

		markAsProcessed(payment.PaymentId)

		task.ScheduleTask(
			CoinbasePayment,
			&CoinbasePaymentArgs{
				Payment: payment,
			},
			session,
		)

	}
	
}



// https://docs.cloud.coinbase.com/sign-in-with-coinbase/docs/api-key-authentication

// TODO start a task to retry a payment until it completes
// payment_id
func RetryPayment(payment_id bringyour.Id, session session.ClientSession) {}



type CoinbasePaymentArgs struct {
	Payment *model.AccountPayment
}

type CoinbasePaymentResult struct {
	Complete bool
}


func CoinbasePayment(coinbasePayment *CoinbasePaymentArgs, clientSession *session.ClientSession) (*CoinbasePaymentResult, error) {


	// if has payment record, get the status of the transaction
	// if complete, finish and send email
	// if in progress, wait
	payment := coinbasePayment.Payment

	// GET https://api.coinbase.com/v2/accounts/:account_id/transactions/:transaction_id
	// https://docs.cloud.coinbase.com/sign-in-with-coinbase/docs/api-transactions

	if payment.Completed || payment.Canceled {
		return &CoinbasePaymentResult{
			Complete: true,
		}, nil
	}

	var tx *CoinbaseTransactionResponseData
	var txResponseBodyBytes []byte // STU_TODO: this is ResponseBodyBytes from fetching the transaction data?

	if payment.PaymentRecord != "" {

		// get the status of the transaction
		getTxDataResult, err := getCoinbaseTxData(payment.PaymentRecord)
		if err != nil {
			return nil, err
		}

		if getTxDataResult.TxData == nil {
			// no transaction
			return nil, fmt.Errorf("No transaction data found for payment %s", payment.PaymentId)
		}

		tx = getTxDataResult.TxData
		txResponseBodyBytes = getTxDataResult.ResponseBodyBytes
	}

	// "completed"
	// "pending", "waiting_for_clearing", "waiting_for_signature"

	switch tx.Status {
	case "pending", "waiting_for_clearing", "waiting_for_signature":
		// check later		
		return &CoinbasePaymentResult{
			Complete: false,
		}, nil

	case "completed":
		// set payment completed

		// send an email

		// do not rerun task

		model.CompletePayment(
			clientSession.Ctx, 
			payment.PaymentId, 
			string(txResponseBodyBytes), // STU_TODO: check this
		)

		userAuth, err := model.GetUserAuth(clientSession.Ctx, payment.NetworkId)
		if err != nil {
			return nil, err
		}

		SendAccountMessageTemplate(userAuth, &SendPaymentTemplate{})

		return &CoinbasePaymentResult{
			Complete: true,
		}, nil

	default:
		// no transaction or error
		// send the payment

		// get the wallet by wallet id
		accountWallet := model.GetAccountWallet(clientSession.Ctx, payment.WalletId)

		// STU_TODO: check this
		payoutAmount := payment.TokenAmount

		// TODO: ensure payout - transaction fees >= minimum payout amount

		// send the payment
		txData, err := sendCoinbasePayment(
				&CoinbaseSendRequest{
				Type: "send",
				To: accountWallet.WalletAddress,
				Amount: fmt.Sprintf("%.4f", payoutAmount),
				Currency: "USDC",
				// don't expose descriptions on the blockchain
				Description: "",
				Idem: payment.PaymentId.String(),
			}, 
			clientSession,
		)
		if err != nil {
			return nil, err
		}

		// set the payment record
		model.SetPaymentRecord(
			clientSession.Ctx, 
			payment.PaymentId, 
			"USDC", 
			payoutAmount, 
			txData.TransactionId,
		)

		return &CoinbasePaymentResult{
			Complete: false,
		}, nil

	}
}

type GetCoinbaseTxDataResult struct {
	TxData *CoinbaseTransactionResponseData
	ResponseBodyBytes []byte
}


func getCoinbaseTxData(transactionId string) (*GetCoinbaseTxDataResult, error) {

	path := fmt.Sprintf("/v2/accounts/%s/transactions/%s", coinbaseAccountId(), transactionId)
	jwt, err := coinbaseJwt("GET", coinbaseApiHost(), path)
	if err != nil {
		return nil, err
	}

	return bringyour.HttpGetRequireStatusOk(
		path,
		func(header http.Header) {
				header.Add("Accept", "application/json")
				header.Add("Authorization", fmt.Sprintf("Bearer %s", jwt))
		},
		func(response *http.Response, responseBodyBytes []byte)(*GetCoinbaseTxDataResult, error) {
				txResult := &CoinbaseTransactionResponse{}
				err := json.Unmarshal(responseBodyBytes, txResult)

				if err != nil {
						return nil, err
				}

				return &GetCoinbaseTxDataResult{
					TxData: txResult.Data,
					ResponseBodyBytes: responseBodyBytes,
				}, nil
		},
	)
}

func sendCoinbasePayment(
	sendRequest *CoinbaseSendRequest, 
	session *session.ClientSession,
) (*CoinbaseSendResponseData, error) {
	path := fmt.Sprintf("/v2/accounts/%s/transactions", coinbaseAccountId())
	jwt, err := coinbaseJwt("POST", coinbaseApiHost(), path)

	if err != nil {
		return nil, err
	}

	return bringyour.HttpGetRequireStatusOk(
		path,
		func(header http.Header) {
				header.Add("Accept", "application/json")
				header.Add("Authorization", fmt.Sprintf("Bearer %s", jwt))
		},
		func(response *http.Response, responseBodyBytes []byte)(*CoinbaseSendResponseData, error) {
				result := &CoinbaseSendResponseData{}
				err := json.Unmarshal(responseBodyBytes, result)

				if err != nil {
						return nil, err
				}

				return result, nil
		},
	)

}

// CoinbaseApiHost = "api.coinbase.com"

var coinbaseApiHost = sync.OnceValue(func()(string) {
	c := bringyour.Vault.RequireSimpleResource("coinbase.yml").Parse()
	return c["api"].(map[string]any)["host"].(string)
})


var coinbaseApiKeyName = sync.OnceValue(func()(string) {
	c := bringyour.Vault.RequireSimpleResource("coinbase.yml").Parse()
	return c["api"].(map[string]any)["key_name"].(string)
})


var coinbaseApiKeySecret = sync.OnceValue(func()(string) {
	c := bringyour.Vault.RequireSimpleResource("coinbase.yml").Parse()
	return c["api"].(map[string]any)["private_key"].(string)
})

var coinbaseAccountId = sync.OnceValue(func()(string) {
	c := bringyour.Vault.RequireSimpleResource("coinbase.yml").Parse()
	return c["api"].(map[string]any)["account_id"].(string)
})


type CoinbaseTransactionResponse struct {
	Data *CoinbaseTransactionResponseData `json:"data"`
}

type CoinbaseTransactionResponseData struct {
	TransactionId string `json:"id"`
	Type string  `json:"type"`
	Status string  `json:"status"`
}


type CoinbaseSendRequest struct {
	Type string `json:"type"`
	To string `json:"to"`
	Amount string `json:"amount"`
	Currency string `json:"currency"`
	Description string `json:"description"`
	Idem string `json:"idem"`
}

type CoinbaseSendResponse struct {
	Data *CoinbaseSendResponseData `json:"data"`
}

type CoinbaseSendResponseData struct {
	TransactionId string `json:"id"`
	Network *CoinbaseSendResponseNetwork `json:"network"`
}

type CoinbaseSendResponseNetwork struct {
	Status string `json:"status"`
	StatusDescription string `json:"status_description"`
	Hash string `json:"hash"`
	NetworkName string `json:"network_name"`
}


func coinbaseJwt(requestMethod string, requestHost string, requestPath string) (string, error) {
	uri := fmt.Sprintf("%s %s%s", requestMethod, requestHost, requestPath)

    block, _ := pem.Decode([]byte(coinbaseApiKeySecret()))
    if block == nil {
        return "", fmt.Errorf("jwt: Could not decode private key")
    }

    key, err := x509.ParseECPrivateKey(block.Bytes)
    if err != nil {
        return "", fmt.Errorf("jwt: %w", err)
    }

    sig, err := jose.NewSigner(
        jose.SigningKey{Algorithm: jose.ES256, Key: key},
        (&jose.SignerOptions{NonceSource: nonceSource{}}).WithType("JWT").WithHeader("kid", coinbaseApiKeyName()),
    )
    if err != nil {
        return "", fmt.Errorf("jwt: %w", err)
    }

    type CoinbaseKeyClaims struct {
	    *jwt.Claims
	    URI string `json:"uri"`
	}

    cl := &CoinbaseKeyClaims{
        Claims: &jwt.Claims{
            Subject:   coinbaseApiKeyName(),
            Issuer:    "coinbase-cloud",
            NotBefore: jwt.NewNumericDate(time.Now()),
            Expiry:    jwt.NewNumericDate(time.Now().Add(2 * time.Minute)),
        },
        URI: uri,
    }
    jwtStr, err := jwt.Signed(sig).Claims(cl).CompactSerialize()
    if err != nil {
        return "", fmt.Errorf("jwt: %w", err)
    }
    return jwtStr, nil
}


type nonceSource struct{}

func (n nonceSource) Nonce() (string, error) {
    r, err := rand.Int(rand.Reader, big.NewInt(math.MaxInt64))
    if err != nil {
        return "", err
    }
    return r.String(), nil
}
