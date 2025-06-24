package controller

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/golang/glog"

	"github.com/urnetwork/server"
)

type CircleApi interface {
	EstimateTransferFee(
		amount float64,
		destinationAddress string,
		network string,
	) (*FeeEstimateResult, error)
	CreateTransferTransaction(
		amountInUsd float64,
		destinationAddress string,
		network string,
	) (*CreateTransferTransactionResult, error)
	GetTransaction(id string) (*GetTransactionResult, error)
}

type CoreCircleApiClient struct{}

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

func (c *CoreCircleApiClient) CreateTransferTransaction(
	amountInUsd float64,
	destinationAddress string,
	network string,
) (*CreateTransferTransactionResult, error) {

	hexEncodedEntitySecret := entitySecret()

	adminWalletId, err := getWalletIdByNetwork(network)
	if err != nil {
		return nil, err
	}

	usdcNetworkAddress, err := getUsdcAddressByNetwork(network)
	if err != nil {
		return nil, err
	}

	cipher, err := generateEntitySecretCipher(hexEncodedEntitySecret)
	if err != nil {
		return nil, err
	}

	uri := "https://api.circle.com/v1/w3s/developer/transactions/transfer"

	res, err := server.HttpPostRequireStatusOk(
		uri,
		map[string]any{
			"idempotencyKey":         server.NewId(),
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

func (c *CoreCircleApiClient) GetTransaction(id string) (*GetTransactionResult, error) {

	uri := fmt.Sprintf("https://api.circle.com/v1/w3s/transactions/%s", id)

	circleApiToken := circleConfig()["api_token"]

	return server.HttpGetRequireStatusOk(
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
func CreateDeveloperWalletSet(name string) {
	hexEncodedEntitySecret := entitySecret()

	circleApiToken := circleConfig()["api_token"]

	cipher, err := generateEntitySecretCipher(hexEncodedEntitySecret)
	if err != nil {
		glog.Infof("[circlec]error generating entity secret cipher: %s", err)
		return
	}

	url := "https://api.circle.com/v1/w3s/developer/walletSets"
	idemKey := server.NewId()

	walletSet, err := server.HttpPostRequireStatusOk(
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
func CreateDeveloperWallet(walletSetId string) {
	hexEncodedEntitySecret := entitySecret()

	circleApiToken := circleConfig()["api_token"]

	cipher, err := generateEntitySecretCipher(hexEncodedEntitySecret)
	if err != nil {
		glog.Infof("[circlec]error generating entity secret cipher: %s", err)
		return
	}

	url := "https://api.circle.com/v1/w3s/developer/wallets"
	idemKey := server.NewId()

	wallets, err := server.HttpPostRequireStatusOk(
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

func generateEntitySecretCipher(hexEncodedEntitySecret string) ([]byte, error) {

	entitySecret, err := hex.DecodeString(hexEncodedEntitySecret)
	if err != nil {
		panic(err)
	}

	publicKeyString, err := getPublicKey()
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
