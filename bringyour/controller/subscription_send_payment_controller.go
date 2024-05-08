package controller

import (
    "crypto/rand"
    "crypto/x509"
    "encoding/pem"
    "fmt"
    "math"
    "math/big"
    "time"

    "gopkg.in/square/go-jose/v3"
    "gopkg.in/square/go-jose/v3/jwt"
)





// run once on startup
func SchedulePendingPayments() {

	GetPendingPayments()
	// schedule a task for CoinbasePayment for each paymentId
	// (use balk because the payment id might already be worked on)
}




// runs twice a day
func SendPayments() {

	PlanPayments()
	// create coinbase payment records
	// schedule a task for CoinbasePayment for each paymentId
	// (use balk because the payment id might already be worked on)

	
}



// https://docs.cloud.coinbase.com/sign-in-with-coinbase/docs/api-key-authentication

// TODO start a task to retry a payment until it completes
// payment_id




type CoinbasePaymentArgs struct {
	PaymentId bringyour.Id
}

type CoinbasePaymentResult struct {
	
}


func CoinbasePayment(coinbasePayment *CoinbasePaymentArgs, clientSession *session.ClientSession) (string, error) {


	// if has payment record, get the status of the transaction
	// if complete, finish and send email
	// if in progress, wait

	// GET https://api.coinbase.com/v2/accounts/:account_id/transactions/:transaction_id
	// https://docs.cloud.coinbase.com/sign-in-with-coinbase/docs/api-transactions

	if payment.Completed || payment.Canceled {
		return &CoinbasePaymentResult{
			Complete: true,
		}, nil
	}

	var status string

	transactionId := payment.PaymentRecord
	if transactionId != "" {

		path := fmt.Sprintf("/v2/accounts/%s/transactions/%s", coinbaseAccountId(), transactionId)
		jwt, err := coinbaseJwt("GET", coinbaseApiHost(), path)

	}

	// "completed"
	// "pending", "waiting_for_clearing", "waiting_for_signature"

	switch status {
	case "pending", "waiting_for_clearing", "waiting_for_signature":
		// check later		
		return &CoinbasePaymentResult{
			Complete: false,
		}, nil

	case "completed":
		// set payment completed

		// send an email

		// do not rerun task

		CompletePayment(ctx, paymentId, string(responseBodyBytes))

		GETUSERINFO(payment.NetworkId)

		SendAccountMessageTemplate(userAuth, &SendPaymentTemplate{})

		return &CoinbasePaymentResult{
			Complete: true,
		}, nil

	default:
		// no transaction or error

		path := fmt.Sprintf("/v2/accounts/%s/transactions", coinbaseAccountId())
		jwt, err := coinbaseJwt("POST", coinbaseApiHost(), path)

		&SendRequest{
			Type: "send",
			To: WALLETADDR,
			Amount: fmt.Sprintf("%.4f", AMOUNT_USD),
			Currency: "USDC",
			// don't expose descriptions on the blockchain
			Description: "",
			Idem: paymentId.String(),
		}


		// TODO set the payment record

		SetPaymentRecord(ctx, paymentId, "USDC", AMOUNT_USDC, transactionId)

		return &CoinbasePaymentResult{
			Complete: false,
		}, nil

	}
}



// CoinbaseApiHost = "api.coinbase.com"

var coinbaseApiHost = sync.OnceValue(func()(string) {
	// FIXME parse vault
})


var coinbaseApiKeyName = sync.OnceValue(func()(string) {
	// FIXME parse vault coinbase.yml
})


var coinbaseApiKeySecret = sync.OnceValue(func()(string) {
	// FIXME parse vault coinbase.yml
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

    block, _ := pem.Decode([]byte(keySecret))
    if block == nil {
        return "", fmt.Errorf("jwt: Could not decode private key")
    }

    key, err := x509.ParseECPrivateKey(block.Bytes)
    if err != nil {
        return "", fmt.Errorf("jwt: %w", err)
    }

    sig, err := jose.NewSigner(
        jose.SigningKey{Algorithm: jose.ES256, Key: key},
        (&jose.SignerOptions{NonceSource: nonceSource{}}).WithType("JWT").WithHeader("kid", keyName),
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
            Subject:   keyName,
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
