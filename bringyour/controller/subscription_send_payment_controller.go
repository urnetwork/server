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





func CoinbasePayment(paymentId bringyour.Id) (string, error) {


	// if has payment record, get the status of the transaction
	// if complete, finish and send email



	// if error, try again
	// if no payment record, start a new record

	// use the send api

	// https://docs.cloud.coinbase.com/sign-in-with-coinbase/docs/api-transactions#send-money

	// POST https://api.coinbase.com/v2/accounts/:account_id/transactions
	path := fmt.Sprintf("/v2/accounts/%s/transactions", coinbaseAccountId())
	jwt, err := coinbaseJwt("POST", coinbaseApiHost(), path)

	// Authentication: Bearer $jwt


	
	// to get the status of the transaction, use
	// /v2/accounts/2bbf394c-193b-5b2a-9155-3b4732659ede/transactions/3c04e35e-8e5a-5ff1-9155-00675db4ac02



	&SendRequest{
		Type: "send",
		To: WALLETADDR,
		Amount: fmt.Sprintf("%.4f", AMOUNT_USD),
		Currency: "USDC",
		// don't expose descriptions on the blockchain
		Description: "",
		Idem: paymentId.String(),
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


type CoinbaseSendRequest struct {
	Type string
	To string
	Amount string
	Currency string
	Description string
	// SkipNotifications bool
	Idem string
}

type CoinbaseSendResponse struct {
	Data *SendResponseData
}

type CoinbaseSendResponseData struct {
	TransactionId string `json:"id"`
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
