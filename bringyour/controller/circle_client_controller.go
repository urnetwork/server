package controller

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"bringyour.com/bringyour"
)

type CircleApi interface {
	EstimateTransferFee(estimateTransferFee SendPaymentArgs) (*FeeEstimateResult, error)
	SendPayment(sendPayment SendPaymentArgs) (*CircleTransferTransactionResult, error)
	GetTransaction(id string) (*GetTransactionResult, error)
}

type CoreCircleApiClient struct {}

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

type SendPaymentArgs struct {
	Amount float64
	DestinationAddress string
	Network string
}

type CircleTransferTransactionResult struct {
	Id string `json:"id"`
	State string `json:"state"`
}

func (c *CoreCircleApiClient) SendPayment(sendPayment SendPaymentArgs) (*CircleTransferTransactionResult, error) {

    hexEncodedEntitySecret := entitySecret()

    cipher, err := generateEntitySecretCipher(hexEncodedEntitySecret)
    if err != nil {
        return nil, err
    }

    usdcNetworkAddress, err := getUsdcAddressByNetwork(sendPayment.Network)
    if err != nil {
        return nil, err
    }

    // estimateFees, err := EstimateTransferFee(
    //     SendPaymentArgs{
    //         Amount: sendPayment.Amount,
    //         DestinationAddress: sendPayment.DestinationAddress,
    //         Network: sendPayment.Network,
    //     },
    // )
    // if err != nil {
    //     return nil, err
    // }

    walletId, err := getWalletIdByNetwork(sendPayment.Network)
    if err != nil {
        return nil, err
    }

    uri := "https://api.circle.com/v1/w3s/developer/transactions/transfer"

	res, err := bringyour.HttpPostRequireStatusOk(
		uri,
		map[string]any{
            "idempotencyKey": bringyour.NewId(),
            "amounts": []string{fmt.Sprintf("%f", sendPayment.Amount)},
            "destinationAddress": sendPayment.DestinationAddress,
            "entitySecretCiphertext": cipher,
            "tokenAddress": usdcNetworkAddress,
            "walletId": walletId,
            "blockchain": sendPayment.Network,
            // for testing
            "feeLevel": "MEDIUM",
		},
		func(header http.Header) {
				header.Add("Accept", "application/json")
				header.Add("Authorization", fmt.Sprintf("Bearer %s", circleConfig()["api_token"]))
		},
		func(response *http.Response, responseBodyBytes []byte)(*CircleTransferTransactionResult, error) {
            result := &CircleResponse[CircleTransferTransactionResult]{}

            err := json.Unmarshal(responseBodyBytes, result)

            if err != nil {
                return nil, err
            }

            return &result.Data, nil
		},
	)

    if err != nil {
        fmt.Printf("Error sending payment: %s", err)
        return nil, err
    }

    fmt.Println("Transaction ID: ", res.Id)
    fmt.Println("Transaction State: ", res.State)

    return res, nil

}

type FeeEstimate struct {
	GasLimit     string  `json:"gasLimit"`
	PriorityFee  string `json:"priorityFee"`
	BaseFee      string `json:"baseFee"`
}

type FeeEstimateResult struct {
	High *FeeEstimate `json:"high,omitempty"`
	Medium *FeeEstimate `json:"medium,omitempty"`
	Low *FeeEstimate `json:"low,omitempty"`
}

func (c *CoreCircleApiClient) EstimateTransferFee(
	estimateTransferFee SendPaymentArgs,
) (*FeeEstimateResult, error) {
    circleApiToken := circleConfig()["api_token"]

    url := "https://api.circle.com/v1/w3s/transactions/transfer/estimateFee"

    usdcNetworkAddress, err := getUsdcAddressByNetwork(estimateTransferFee.Network)
    if err != nil {
        return nil, err
    }

    walletId, err := getWalletIdByNetwork(estimateTransferFee.Network)
    if err != nil {
        return nil, err
    }

    feeEstimate, err := bringyour.HttpPostRequireStatusOk(
        url,
        map[string]any{
            "amounts": []string{fmt.Sprintf("%f", estimateTransferFee.Amount)},
            "destinationAddress": estimateTransferFee.DestinationAddress,
            "walletId": walletId,
            "tokenAddress": usdcNetworkAddress,
            "blockchain": estimateTransferFee.Network,
        },
        func(header http.Header) {
            header.Add("Accept", "application/json")
            header.Add("Authorization", fmt.Sprintf("Bearer %s", circleApiToken))
        },
        func(response *http.Response, responseBodyBytes []byte)(*FeeEstimateResult, error) {
            result := &CircleResponse[FeeEstimateResult]{}

            err := json.Unmarshal(responseBodyBytes, result)

            if err != nil {
                return nil, err
            }

            return &result.Data, nil
        },
    )

    if err != nil {
        return nil, err
    }

    return feeEstimate, nil
}

type CircleTransactionResult struct {
	Transaction CircleTransaction `json:"transaction"`
}

type CircleTransaction struct {
	Id string `json:"id"`
	Amounts []string `json:"amounts"`
	AmountInUSD string `json:"amountInUSD"`
	Blockchain string `json:"blockchain"`
	DestinationAddress string `json:"destinationAddress"`
	EstimatedFees *FeeEstimate `json:"estimatedFees"`
	NetworkFee string `json:"networkFee"`
	NetworkFeeInUSD string `json:"networkFeeInUSD"`
	SourceAddress string `json:"sourceAddress"`
	State string `json:"state"`
	TokenId string `json:"tokenId"`
	Operation string `json:"operation"`
	TransactionType string `json:"transactionType"`
	TxHash string `json:"txHash"`
	WalletId string `json:"walletId"`
}

type GetTransactionResult struct {
	Transaction CircleTransaction `json:"transaction"`
	ResponseBodyBytes []byte
}

func (c *CoreCircleApiClient) GetTransaction(id string) (*GetTransactionResult, error) {

	uri := fmt.Sprintf("https://api.circle.com/v1/w3s/transactions/%s", id)

	circleApiToken := circleConfig()["api_token"]

	return bringyour.HttpGetRequireStatusOk(
		uri,
		func(header http.Header) {
				header.Add("Accept", "application/json")
				header.Add("Authorization", fmt.Sprintf("Bearer %s", circleApiToken))
		},
		func(response *http.Response, responseBodyBytes []byte)(*GetTransactionResult, error) {
				result := &CircleResponse[CircleTransactionResult]{}

				err := json.Unmarshal(responseBodyBytes, result)
				if err != nil {
						return nil, err
				}

				return &GetTransactionResult{
						Transaction: result.Data.Transaction,
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

var entitySecret = sync.OnceValue(func()(string) {
	c := bringyour.Vault.RequireSimpleResource("circle.yml").Parse()
	return c["circle"].(map[string]any)["entity_secret"].(string)
})

var solanaUSDCAddress = sync.OnceValue(func() string {
	c := bringyour.Vault.RequireSimpleResource("circle.yml").Parse()
	return c["circle"].(map[string]any)["solana_usdc_address"].(string)
})

var polygonUSDCAddress = sync.OnceValue(func() string {
	c := bringyour.Vault.RequireSimpleResource("circle.yml").Parse()
	return c["circle"].(map[string]any)["polygon_usdc_address"].(string)
})

var solanaWalletId = sync.OnceValue(func()(string) {
	c := bringyour.Vault.RequireSimpleResource("circle.yml").Parse()
return c["circle"].(map[string]any)["solana_wallet_id"].(string)
})

var polygonWalletId = sync.OnceValue(func()(string) {
	c := bringyour.Vault.RequireSimpleResource("circle.yml").Parse()
return c["circle"].(map[string]any)["polygon_wallet_id"].(string)
})