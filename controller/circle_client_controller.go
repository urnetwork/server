package controller

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Only provider payout supplies this identity. Customer/admin transfers keep
// their separate policies; delayed limiter admission rechecks the actual debt.
type providerUsdcPaymentContextKey struct{}

// This instance-owned observation seam orders a mutation after the actual
// public read in regression tests. Production callers never populate it.
type providerPaymentReadObserverKey struct{}

type providerPaymentSubmission struct {
	Basis   model.ProviderPaymentBasis
	Amount  float64
	Network string
}

type circleTransferArguments struct {
	IdempotencyKey server.Id
	Amount         float64
	Destination    string
	Network        string
}

// The limiter and the final earning check form one send boundary, also used
// by deterministic tests that advance policy while admission is suspended.
func circleTransferAfterAdmission(ctx context.Context, args circleTransferArguments, wait func(context.Context) error, send func(context.Context) (*CreateTransferTransactionResult, error)) (*CreateTransferTransactionResult, error) {
	if err := wait(ctx); err != nil {
		return nil, err
	}
	if err := requireCircleProviderTransfer(ctx, args); err != nil {
		return nil, err
	}
	return send(ctx)
}

func requireCircleProviderPayment(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if value := ctx.Value(providerUsdcPaymentContextKey{}); value != nil {
		submission, ok := value.(providerPaymentSubmission)
		if !ok {
			return model.ErrProviderPaymentBasisChanged
		}
		return model.RequireProviderPaymentRequest(ctx, &submission.Basis, submission.Amount, submission.Network)
	}
	return nil
}

// Actual Core arguments, not merely a valid payment id, must match the
// retained attempt. Customer/admin sends have no provider submission context.
func requireCircleProviderTransfer(ctx context.Context, args circleTransferArguments) error {
	if err := requireCircleProviderPayment(ctx); err != nil {
		return err
	}
	if submission, ok := ctx.Value(providerUsdcPaymentContextKey{}).(providerPaymentSubmission); ok {
		if submission.Basis.IdempotencyKey != args.IdempotencyKey || submission.Amount != args.Amount ||
			submission.Basis.WalletAddress != args.Destination || submission.Network != args.Network {
			return model.ErrProviderPaymentBasisChanged
		}
	}
	return nil
}

type CircleApi interface {
	EstimateTransferFee(
		ctx context.Context,
		amount float64,
		destinationAddress string,
		network string,
	) (*FeeEstimateResult, error)
	// `idempotencyKey` must be stable across retries of the same logical
	// transfer, so the processor deduplicates resubmits instead of
	// double-sending funds
	CreateTransferTransaction(
		ctx context.Context,
		idempotencyKey server.Id,
		amountInUsd float64,
		destinationAddress string,
		network string,
	) (*CreateTransferTransactionResult, error)
	GetTransaction(
		ctx context.Context,
		id string,
	) (*GetTransactionResult, error)
}

type CoreCircleApiClient struct {
	readSettings PaymentReadSettings
	readHooks    paymentReadHooks
	readToken    func() string
}

// The returned client's read policy is immutable and safe for concurrent GETs.
// Each operation owns its own timer/transport; POST methods are unchanged.
func NewCoreCircleApiClient(settings PaymentReadSettings) (*CoreCircleApiClient, error) {
	settings, err := settings.normalized()
	if err != nil {
		return nil, err
	}
	return &CoreCircleApiClient{readSettings: settings}, nil
}

var circleClientInstance CircleApi = &CoreCircleApiClient{}

func NewCircleClient() CircleApi {
	return circleClientInstance
}

// for stubbing in tests
func SetCircleClient(client CircleApi) {
	circleClientInstance = client
}

type CircleResponse[T any] struct {
	Data T `json:"data"`
}

type CreateTransferTransactionResult struct {
	Id    string `json:"id"`
	State string `json:"state"`
}

// isCircleInvalidDestinationError identifies the definitive pre-chain Circle
// rejection for an invalid destination. Only this exact 400 is safe to reset:
// transport failures and rate limits can be ambiguous after submission and
// must retain their idempotency key.
func isCircleInvalidDestinationError(err error) bool {
	var statusErr *server.HttpStatusError
	if !errors.As(err, &statusErr) || statusErr.StatusCode != http.StatusBadRequest {
		return false
	}

	var response struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if json.Unmarshal([]byte(statusErr.ResponseBody), &response) != nil {
		return false
	}
	message := strings.TrimSuffix(strings.TrimSpace(response.Message), ".")
	return response.Code == 155219 || strings.EqualFold(message, "Invalid destination address")
}

func (c *CoreCircleApiClient) CreateTransferTransaction(
	ctx context.Context,
	idempotencyKey server.Id,
	amountInUsd float64,
	destinationAddress string,
	network string,
) (*CreateTransferTransactionResult, error) {
	args := circleTransferArguments{IdempotencyKey: idempotencyKey, Amount: amountInUsd, Destination: destinationAddress, Network: network}
	if err := requireCircleProviderTransfer(ctx, args); err != nil {
		return nil, err
	}
	hexEncodedEntitySecret := entitySecret()

	adminWalletId, err := getWalletIdByNetwork(network)
	if err != nil {
		return nil, err
	}

	usdcNetworkAddress, err := getUsdcAddressByNetwork(network)
	if err != nil {
		return nil, err
	}

	cipher, err := generateEntitySecretCipher(ctx, hexEncodedEntitySecret)
	if err != nil {
		return nil, err
	}

	uri := "https://api.circle.com/v1/w3s/developer/transactions/transfer"
	res, err := circleTransferAfterAdmission(ctx, args, waitForCircleTransferAdmission, func(ctx context.Context) (*CreateTransferTransactionResult, error) {
		return server.HttpPostRequireStatusOk(
			ctx,
			uri,
			map[string]any{
				"idempotencyKey":         idempotencyKey,
				"amounts":                []string{fmt.Sprintf("%f", amountInUsd)},
				"destinationAddress":     destinationAddress,
				"entitySecretCiphertext": cipher,
				"tokenAddress":           usdcNetworkAddress,
				"walletId":               adminWalletId,
				"blockchain":             network,
				"feeLevel":               "MEDIUM",
			},
			func(header http.Header) {
				header.Add("Accept", "application/json")
				header.Add("Authorization", fmt.Sprintf("Bearer %s", circleConfig()["api_token"]))
			},
			func(response *http.Response, responseBodyBytes []byte) (*CreateTransferTransactionResult, error) {
				result := &CircleResponse[CreateTransferTransactionResult]{}

				err := json.Unmarshal(responseBodyBytes, result)

				if err != nil {
					return nil, err
				}

				return &result.Data, nil
			},
		)
	})

	if err != nil {
		glog.Infof("[circlec]error sending payment: %s", err)
		return nil, err
	}

	return res, nil

}

type FeeEstimate struct {
	GasLimit    string `json:"gasLimit"`
	PriorityFee string `json:"priorityFee"`
	BaseFee     string `json:"baseFee"`
	GasPrice    string `json:"gasPrice"`
	MaxFee      string `json:"maxFee"`
}

type FeeEstimateResult struct {
	High   *FeeEstimate `json:"high,omitempty"`
	Medium *FeeEstimate `json:"medium,omitempty"`
	Low    *FeeEstimate `json:"low,omitempty"`
}

func (c *CoreCircleApiClient) EstimateTransferFee(
	ctx context.Context,
	amount float64,
	destinationAddress string,
	network string,
) (*FeeEstimateResult, error) {
	circleApiToken := circleConfig()["api_token"]

	url := "https://api.circle.com/v1/w3s/transactions/transfer/estimateFee"

	usdcNetworkAddress, err := getUsdcAddressByNetwork(network)
	if err != nil {
		return nil, err
	}

	walletId, err := getWalletIdByNetwork(network)
	if err != nil {
		return nil, err
	}

	return server.HttpPostRequireStatusOk(
		ctx,
		url,
		map[string]any{
			"amounts":            []string{fmt.Sprintf("%f", amount)},
			"destinationAddress": destinationAddress,
			"walletId":           walletId,
			"tokenAddress":       usdcNetworkAddress,
			"blockchain":         network,
		},
		func(header http.Header) {
			header.Add("Accept", "application/json")
			header.Add("Authorization", fmt.Sprintf("Bearer %s", circleApiToken))
		},
		func(response *http.Response, responseBodyBytes []byte) (*FeeEstimateResult, error) {
			result := &CircleResponse[FeeEstimateResult]{}

			err := json.Unmarshal(responseBodyBytes, result)

			if err != nil {
				return nil, err
			}

			return &result.Data, nil
		},
	)
}

type CircleTransactionResult struct {
	Transaction CircleTransaction `json:"transaction"`
}

type CircleTransaction struct {
	Id                 string       `json:"id"`
	Amounts            []string     `json:"amounts"`
	AmountInUSD        string       `json:"amountInUSD"`
	Blockchain         string       `json:"blockchain"`
	DestinationAddress string       `json:"destinationAddress"`
	EstimatedFees      *FeeEstimate `json:"estimatedFees"`
	NetworkFee         string       `json:"networkFee"`
	NetworkFeeInUSD    string       `json:"networkFeeInUSD"`
	SourceAddress      string       `json:"sourceAddress"`
	State              string       `json:"state"`
	TokenId            string       `json:"tokenId"`
	Operation          string       `json:"operation"`
	TransactionType    string       `json:"transactionType"`
	TxHash             string       `json:"txHash"`
	WalletId           string       `json:"walletId"`
}

type GetTransactionResult struct {
	Transaction       CircleTransaction `json:"transaction"`
	ResponseBodyBytes []byte
}

func (self *CoreCircleApiClient) GetTransaction(ctx context.Context, id string) (*GetTransactionResult, error) {
	if ctx == nil {
		return nil, fmt.Errorf("transaction GET requires a context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if id == "" || len(id) > 256 {
		return nil, fmt.Errorf("invalid transaction observation id")
	}
	uri := fmt.Sprintf("https://api.circle.com/v1/w3s/transactions/%s", url.PathEscape(id))
	token := self.readToken
	if token == nil {
		token = func() string { return fmt.Sprint(circleConfig()["api_token"]) }
	}
	circleApiToken := token()
	return paymentHttpGet(
		ctx,
		self.readSettings,
		self.readHooks,
		uri,
		func(header http.Header) {
			header.Add("Accept", "application/json")
			header.Add("Authorization", fmt.Sprintf("Bearer %s", circleApiToken))
		},
		func(response *http.Response, responseBodyBytes []byte) (*GetTransactionResult, error) {
			result := &CircleResponse[CircleTransactionResult]{}

			err := json.Unmarshal(responseBodyBytes, result)
			if err != nil {
				return nil, err
			}
			if result.Data.Transaction.Id != id {
				return nil, fmt.Errorf("transaction observation identity mismatch")
			}

			return &GetTransactionResult{
				Transaction:       result.Data.Transaction,
				ResponseBodyBytes: responseBodyBytes,
			}, nil
		},
	)

}

func getWalletIdByNetwork(network string) (id string, err error) {
	network = strings.TrimSpace(network)
	network = strings.ToUpper(network)

	switch network {
	case "SOL", "SOLANA":
		id = solanaWalletId()
	case "MATIC", "POLY", "POLYGON":
		id = polygonWalletId()
	default:
		err = fmt.Errorf("unsupported network: %s", network)
	}

	return
}

func getUsdcAddressByNetwork(network string) (address string, err error) {
	network = strings.TrimSpace(network)
	network = strings.ToUpper(network)

	switch network {
	case "SOL", "SOLANA":
		address = solanaUSDCAddress()
	case "MATIC", "POLY", "POLYGON":
		address = polygonUSDCAddress()
	default:
		err = fmt.Errorf("unsupported network: %s", network)
	}

	return
}

var entitySecret = sync.OnceValue(func() string {
	c := server.Vault.RequireSimpleResource("circle.yml").Parse()
	return c["circle"].(map[string]any)["entity_secret"].(string)
})

var solanaUSDCAddress = sync.OnceValue(func() string {
	c := server.Vault.RequireSimpleResource("circle.yml").Parse()
	return c["circle"].(map[string]any)["solana_usdc_address"].(string)
})

var polygonUSDCAddress = sync.OnceValue(func() string {
	c := server.Vault.RequireSimpleResource("circle.yml").Parse()
	return c["circle"].(map[string]any)["polygon_usdc_address"].(string)
})

var solanaWalletId = sync.OnceValue(func() string {
	c := server.Vault.RequireSimpleResource("circle.yml").Parse()
	return c["circle"].(map[string]any)["solana_wallet_id"].(string)
})

var polygonWalletId = sync.OnceValue(func() string {
	c := server.Vault.RequireSimpleResource("circle.yml").Parse()
	return c["circle"].(map[string]any)["polygon_wallet_id"].(string)
})

type WalletSet struct {
	Id          string `json:"id"`
	CustodyType string `json:"custodyType"`
}

type WalletSetResult struct {
	WalletSet *WalletSet `json:"walletSet,omitempty"`
}

// for developers to create a new wallet set for payouts
func CreateDeveloperWalletSet(ctx context.Context, name string) {
	hexEncodedEntitySecret := entitySecret()

	circleApiToken := circleConfig()["api_token"]

	cipher, err := generateEntitySecretCipher(ctx, hexEncodedEntitySecret)
	if err != nil {
		glog.Infof("[circlec]error generating entity secret cipher: %s", err)
		return
	}

	url := "https://api.circle.com/v1/w3s/developer/walletSets"
	idemKey := server.NewId()

	walletSet, err := server.HttpPostRequireStatusOk(
		ctx,
		url,
		map[string]any{
			"idempotencyKey":         idemKey,
			"name":                   name,
			"entitySecretCiphertext": cipher,
		},
		func(header http.Header) {
			header.Add("Accept", "application/json")
			header.Add("Authorization", fmt.Sprintf("Bearer %s", circleApiToken))
		},
		func(response *http.Response, responseBodyBytes []byte) (*WalletSet, error) {
			result := &CircleResponse[WalletSetResult]{}

			err := json.Unmarshal(responseBodyBytes, result)

			if err != nil {
				return nil, err
			}

			return result.Data.WalletSet, nil
		},
	)

	if err != nil {
		glog.Infof("[circlec]error creating wallet set: %s", err)
		return
	}

	glog.Infof("[circlec]created Wallet Set ID: ", walletSet.Id)
}

type DeveloperWallet struct {
	Id          string `json:"id"`
	State       string `json:"state"`
	WalletSetId string `json:"walletSetId"`
	CustodyType string `json:"custodyType"`
	Address     string `json:"address"`
	Blockchain  string `json:"blockchain"`
	AccountType string `json:"accountType"`
}

type DeveloperWalletResult struct {
	Wallets []*DeveloperWallet `json:"wallets,omitempty"`
}

// for developers to create a new wallet for payouts
// not for end users
func CreateDeveloperWallet(ctx context.Context, walletSetId string) {
	hexEncodedEntitySecret := entitySecret()

	circleApiToken := circleConfig()["api_token"]

	cipher, err := generateEntitySecretCipher(ctx, hexEncodedEntitySecret)
	if err != nil {
		glog.Infof("[circlec]error generating entity secret cipher: %s", err)
		return
	}

	url := "https://api.circle.com/v1/w3s/developer/wallets"
	idemKey := server.NewId()

	wallets, err := server.HttpPostRequireStatusOk(
		ctx,
		url,
		map[string]any{
			"idempotencyKey":         idemKey,
			"accountType":            "EOA",
			"blockchains":            []string{"MATIC", "SOL"},
			"count":                  1,
			"entitySecretCiphertext": cipher,
			"walletSetId":            walletSetId,
		},
		func(header http.Header) {
			header.Add("Accept", "application/json")
			header.Add("Authorization", fmt.Sprintf("Bearer %s", circleApiToken))
		},
		func(response *http.Response, responseBodyBytes []byte) ([]*DeveloperWallet, error) {
			result := &CircleResponse[DeveloperWalletResult]{}

			err := json.Unmarshal(responseBodyBytes, result)

			if err != nil {
				return nil, err
			}

			return result.Data.Wallets, nil
		},
	)

	if err != nil {
		glog.Infof("[circlec]error creating wallet set: %s\n", err)
		return
	}

	for _, wallet := range wallets {
		glog.Infof("[circlec]created wallet[%s] = %s\n", wallet.Id, wallet.Address)
	}

}

func generateEntitySecretCipher(ctx context.Context, hexEncodedEntitySecret string) ([]byte, error) {

	entitySecret, err := hex.DecodeString(hexEncodedEntitySecret)
	if err != nil {
		panic(err)
	}

	publicKeyString, err := getPublicKey(ctx)
	if err != nil {
		return nil, err
	}

	pubKey, err := parseRsaPublicKeyFromPem([]byte(*publicKeyString))
	if err != nil {
		return nil, err
	}

	cipher, err := encryptOAEP(pubKey, entitySecret)
	if err != nil {
		panic(err)
	}

	return cipher, nil
}

type PopulateTxHashRow struct {
	PaymentId      server.Id `json:"payment_id"`
	TxHash         string    `json:"tx_hash"`
	PaymentReceipt string    `json:"payment_receipt"`
}

/**
 * remove this after tx hashes are populating when payments are created
 */
func PopulateTxHashes(ctx context.Context) {

	var rows []PopulateTxHashRow

	server.Tx(ctx, func(tx server.PgTx) {
		result, err := tx.Query(
			ctx,
			`
				SELECT
					payment_id,
					payment_receipt
				FROM
					account_payment
				WHERE completed = true AND tx_hash IS NULL
			`,
		)

		server.WithPgResult(result, err, func() {

			for result.Next() {

				// transferStats = &TransferStats{}
				row := PopulateTxHashRow{}

				server.Raise(
					result.Scan(
						&row.PaymentId,
						&row.PaymentReceipt,
					),
				)

				var resp CircleResponse[CircleTransactionResult]
				err := json.Unmarshal([]byte(row.PaymentReceipt), &resp)
				if err != nil {
					// handle error
					glog.Infof("%s -> %s", row.PaymentId, err.Error())
					continue
				}

				if resp.Data.Transaction.TxHash == "" {
					glog.Infof("[circlec]no tx hash for payment %s", row.PaymentId)
					glog.Infof("%s -> empty string", row.PaymentId)
					continue
				}

				row.TxHash = resp.Data.Transaction.TxHash

				rows = append(rows, row)

			}
		})

		glog.Infof("updating %d rows", len(rows))

		for i, row := range rows {

			server.RaisePgResult(tx.Exec(
				ctx,
				`
				UPDATE account_payment
				SET
					tx_hash = $1
			    WHERE payment_id = $2
				`,
				row.TxHash,
				row.PaymentId,
			))

			glog.Infof("[%d/%d] %s -> %s", i+1, len(rows), row.PaymentId, row.TxHash)

		}

	})

}
