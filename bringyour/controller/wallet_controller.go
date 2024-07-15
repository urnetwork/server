package controller

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"strings"
	"sync"

	// "io"
	"strconv"
	"time"

	// "strings"

	"bringyour.com/bringyour"
	"bringyour.com/bringyour/model"
	"bringyour.com/bringyour/session"
)


var circleConfig = sync.OnceValue(func() map[string]any {
    c := bringyour.Vault.RequireSimpleResource("circle.yml").Parse()
    return c["circle"].(map[string]any)
})

var walletSetId = sync.OnceValue(func() string {
    c := bringyour.Vault.RequireSimpleResource("circle.yml").Parse()
    return c["wallet_set_id"].(string)
})

type WalletCircleInitResult struct {
    UserToken *CircleUserToken `json:"user_token,omitempty"`
    ChallengeId string `json:"challenge_id,omitempty"`
    Error *WalletCircleInitError `json:"error,omitempty"`
}


type WalletCircleInitError struct {
    Message string `json:"message"`
}


func WalletCircleInit(
    session *session.ClientSession,
) (*WalletCircleInitResult, error) {
    _, err := createCircleUser(session)
    if err != nil {
        return nil, err
    }

    circleUserToken, err := createCircleUserToken(session)
    if err != nil {
        return nil, err
    }

    return bringyour.HttpPostRequireStatusOk(
        "https://api.circle.com/v1/w3s/user/initialize",
        map[string]any{
            "idempotencyKey": bringyour.NewId(),
            "accountType": "SCA",
            "blockchains": []string{
                circleConfig()["blockchain"].(string),
            },
        },
        func(header http.Header) {
            header.Add("Accept", "application/json")
            header.Add("Authorization", fmt.Sprintf("Bearer %s", circleConfig()["api_token"]))
            header.Add("X-User-Token", circleUserToken.UserToken)
        },
        func(response *http.Response, responseBodyBytes []byte)(*WalletCircleInitResult, error) {
            challengeId, err := parseCircleChallengeId(responseBodyBytes)

            if err == nil {
                return &WalletCircleInitResult{
                    UserToken: circleUserToken,
                    ChallengeId: challengeId,
                }, nil
            } else {
                return &WalletCircleInitResult{
                    Error: &WalletCircleInitError{
                        Message: "Bad challenge response.",
                    },
                }, nil
            }
        },
    )
}


type WalletValidateAddressArgs struct {
    Address string `json:"address,omitempty"`
}


type WalletValidateAddressResult struct {
    Valid bool `json:"valid,omitempty"`
}


func WalletValidateAddress(
    walletValidateAddress *WalletValidateAddressArgs,
    session *session.ClientSession,
) (*WalletValidateAddressResult, error) {
    circleUserToken, err := createCircleUserToken(session)
    if err != nil {
        return nil, err
    }

    return bringyour.HttpPostRequireStatusOk(
        "https://api.circle.com/v1/w3s/transactions/validateAddress",
        map[string]any{
            "blockchain": circleConfig()["blockchain"],
            "address": walletValidateAddress.Address,
        },
        func(header http.Header) {
            header.Add("Accept", "application/json")
            header.Add("Authorization", fmt.Sprintf("Bearer %s", circleConfig()["api_token"]))
            header.Add("X-User-Token", circleUserToken.UserToken)
        },
        func(response *http.Response, responseBodyBytes []byte)(*WalletValidateAddressResult, error) {
            _, data, err := parseCircleResponseData(responseBodyBytes)
            if err != nil {
                return nil, err
            }

            valid := false
            if validAny := data["isValid"]; validAny != nil {
                if v, ok := validAny.(bool); ok {
                    valid = v
                } 
            }

            return &WalletValidateAddressResult{
                Valid: valid,
            }, nil
        },
    )
}


type WalletBalanceResult struct {
    WalletInfo *CircleWalletInfo `json:"wallet_info,omitempty"`
}


func WalletBalance(session *session.ClientSession) (*WalletBalanceResult, error) {
    walletInfo, err := findMostRecentCircleWallet(session)
    if err != nil {
        return nil, err
    }

    return &WalletBalanceResult{
        WalletInfo: walletInfo,
    }, nil
}


type WalletCircleTransferOutArgs struct {
    ToAddress string `json:"to_address"`
    AmountUsdcNanoCents model.NanoCents `json:"amount_usdc_nano_cents"`
    Terms bool `json:"terms"`
}


type WalletCircleTransferOutResult struct {
    UserToken *CircleUserToken `json:"user_token,omitempty"`
    ChallengeId string `json:"challenge_id,omitempty"`
    Error *WalletCircleTransferOutError `json:"error,omitempty"`
}


type WalletCircleTransferOutError struct {
    Message string `json:"message"`
}


func WalletCircleTransferOut(
    walletCircleTransferOut *WalletCircleTransferOutArgs,
    session *session.ClientSession,
) (*WalletCircleTransferOutResult, error) {
    if !walletCircleTransferOut.Terms {
        return nil, fmt.Errorf("You must accept the terms of transfer.")
    }

    walletInfo, err := findMostRecentCircleWallet(session)
    if err != nil {
        return nil, err
    }

    circleUserToken, err := createCircleUserToken(session)
    if err != nil {
        return nil, err
    }

    return bringyour.HttpPostRequireStatusOk(
        "https://api.circle.com/v1/w3s/user/transactions/transfer",
        map[string]any{
            "userId": circleUserToken.circleUserId,
            "destinationAddress": walletCircleTransferOut.ToAddress,
            "amounts": []string{
                fmt.Sprintf("%f", model.NanoCentsToUsd(walletCircleTransferOut.AmountUsdcNanoCents)),
            },
            "idempotencyKey": bringyour.NewId(),
            "tokenId": walletInfo.TokenId,
            "walletId": walletInfo.WalletId,
            "feeLevel": "LOW",
        },
        func(header http.Header) {
            header.Add("Accept", "application/json")
            header.Add("Authorization", fmt.Sprintf("Bearer %s", circleConfig()["api_token"]))
            header.Add("X-User-Token", circleUserToken.UserToken)
        },
        func(response *http.Response, responseBodyBytes []byte)(*WalletCircleTransferOutResult, error) {
            challengeId, err := parseCircleChallengeId(responseBodyBytes)

            if err == nil {
                return &WalletCircleTransferOutResult{
                    UserToken: circleUserToken,
                    ChallengeId: challengeId,
                }, nil
            } else {
                return &WalletCircleTransferOutResult{
                    Error: &WalletCircleTransferOutError{
                        Message: "Bad challenge response.",
                    },
                }, nil
            }
        },
    )
}

type CircleResponse[T any] struct {
	Data T `json:"data"`
}

var entitySecret = sync.OnceValue(func()(string) {
	c := bringyour.Vault.RequireSimpleResource("circle.yml").Parse()
	return c["circle"].(map[string]any)["entity_secret"].(string)
})

type WalletSet struct {
    Id string `json:"id"`
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
        fmt.Printf("Error generating entity secret cipher: %s", err)
        return
    }

    url := "https://api.circle.com/v1/w3s/developer/walletSets"
    idemKey := bringyour.NewId()

    walletSet, err := bringyour.HttpPostRequireStatusOk(
        url,
        map[string]any{
            "idempotencyKey": idemKey,
            "name": name,
            "entitySecretCiphertext": cipher,
        },
        func(header http.Header) {
            header.Add("Accept", "application/json")
            header.Add("Authorization", fmt.Sprintf("Bearer %s", circleApiToken))
        },
        func(response *http.Response, responseBodyBytes []byte)(*WalletSet, error) {
            result := &CircleResponse[WalletSetResult]{}

            err := json.Unmarshal(responseBodyBytes, result)

            if err != nil {
                return nil, err
            }

            return result.Data.WalletSet, nil
        },
    )

    if err != nil {
        fmt.Printf("Error creating wallet set: %s", err)
        return
    }

    fmt.Println("Created Wallet Set ID: ", walletSet.Id)
    
}

type DeveloperWallet struct {
    Id string `json:"id"`
    State string `json:"state"`
    WalletSetId string `json:"walletSetId"`
    CustodyType string `json:"custodyType"`
    Address string `json:"address"`
    Blockchain string `json:"blockchain"`
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
        fmt.Printf("Error generating entity secret cipher: %s", err)
        return
    }

    url := "https://api.circle.com/v1/w3s/developer/wallets"
    idemKey := bringyour.NewId()

    wallets, err := bringyour.HttpPostRequireStatusOk(
        url,
        map[string]any{
            "idempotencyKey": idemKey,
            "accountType": "EOA",
            "blockchains": []string{"MATIC", "SOL"},
            "count": 1,
            "entitySecretCiphertext": cipher,
            "walletSetId": walletSetId,
        },
        func(header http.Header) {
            header.Add("Accept", "application/json")
            header.Add("Authorization", fmt.Sprintf("Bearer %s", circleApiToken))
        },
        func(response *http.Response, responseBodyBytes []byte)([]*DeveloperWallet, error) {
            result := &CircleResponse[DeveloperWalletResult]{}

            err := json.Unmarshal(responseBodyBytes, result)

            if err != nil {
                return nil, err
            }

            return result.Data.Wallets, nil
        },
    )

    if err != nil {
        fmt.Printf("Error creating wallet set: %s", err)
        return
    }

    for _, wallet := range wallets {
        fmt.Println(":: Created Wallet ========")
        fmt.Println("Created Wallet ID: ", wallet.Id)
        fmt.Println("Wallet Address: ", wallet.Address)
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

type GetPublicKeyResult struct {
    PublicKey string `json:"publicKey"`
}

func getPublicKey() (*string, error) {

    circleApiToken := circleConfig()["api_token"]

    url := "https://api.circle.com/v1/w3s/config/entity/publicKey"

    publicKey, err := bringyour.HttpGetRequireStatusOk(
        url,
        func(header http.Header) {
            header.Add("Accept", "application/json")
            header.Add("Authorization", fmt.Sprintf("Bearer %s", circleApiToken))
        },
        func(response *http.Response, responseBodyBytes []byte)(*string, error) {
            result := &CircleResponse[GetPublicKeyResult]{}

            err := json.Unmarshal(responseBodyBytes, result)
            if err != nil {
                return nil, err
            }

            return &result.Data.PublicKey, nil
        },
    )

    if err != nil {
        return nil, err
    }

    if publicKey == nil || len(*publicKey) == 0 {
        return nil, fmt.Errorf("no public key found")
    }

    return publicKey, nil
}

func parseRsaPublicKeyFromPem(pubPEM []byte) (*rsa.PublicKey, error) {
    block, _ := pem.Decode(pubPEM)
    if block == nil {
        return nil, fmt.Errorf("failed to parse PEM block containing the key")
    }

    pub, err := x509.ParsePKIXPublicKey(block.Bytes)
    if err != nil {
        return nil, err
    }

    switch pub := pub.(type) {
    case *rsa.PublicKey:
        return pub, nil
    default:
    }
    return nil, fmt.Errorf("key type is not rsa")
}

// EncryptOAEP rsa encrypt oaep.
func encryptOAEP(pubKey *rsa.PublicKey, message []byte) (ciphertext []byte, err error) {
	random := rand.Reader
	ciphertext, err = rsa.EncryptOAEP(sha256.New(), random, pubKey, message, nil)
	if err != nil {
		return nil, err
	}
	return
}


func createCircleUser(session *session.ClientSession) (
    circleUserId bringyour.Id,
    resultErr error,
) {
    circleUserId = model.GetOrCreateCircleUserId(
        session.Ctx,
        session.ByJwt.NetworkId,
        session.ByJwt.UserId,
    )

    circleApiToken := circleConfig()["api_token"]

    _, resultErr = bringyour.HttpPost(
        "https://api.circle.com/v1/w3s/users",
        map[string]any{
            "userId": circleUserId,
        },
        func(header http.Header) {
            header.Add("Accept", "application/json")
            header.Add("Authorization", fmt.Sprintf("Bearer %s", circleApiToken))
        },
        func(response *http.Response, responseBodyBytes []byte)(any, error) {
            if 200 <= response.StatusCode && response.StatusCode < 300 {
                // all ok
                return nil, nil
            }

            // attempt to parse the response and find the `code`
            responseBody := map[string]any{}
            err := json.Unmarshal(responseBodyBytes, &responseBody)
            if err != nil {
                return nil, err
            }

            if codeAny, ok := responseBody["code"]; ok {
                if code, ok := codeAny.(float64); ok {
                    // "Existing user already created with the provided userId."
                    if code == 155101 {
                        // already have a user, all ok
                        return nil, nil
                    }
                }
            }

            return nil, fmt.Errorf("Bad status: %s %s", response.Status, string(responseBodyBytes))
        },
    )
    return
}


type CircleUserToken struct {
    // this field is not exported
    circleUserId bringyour.Id
    UserToken string `json:"user_token"`
    EncryptionKey string `json:"encryption_key"`
}


func createCircleUserToken(session *session.ClientSession) (*CircleUserToken, error) {
    circleUserId := model.GetOrCreateCircleUserId(
        session.Ctx,
        session.ByJwt.NetworkId,
        session.ByJwt.UserId,
    )

    circleApiToken := circleConfig()["api_token"]

    return bringyour.HttpPostRequireStatusOk(
        "https://api.circle.com/v1/w3s/users/token",
        map[string]any{
            "userId": circleUserId,
        },
        func(header http.Header) {
            header.Add("Accept", "application/json")
            header.Add("Authorization", fmt.Sprintf("Bearer %s", circleApiToken))
        },
        func(response *http.Response, responseBodyBytes []byte)(*CircleUserToken, error) {
            _, data, err := parseCircleResponseData(responseBodyBytes)
            if err != nil {
                return nil, err
            }

            circleUserToken := &CircleUserToken{
                circleUserId: circleUserId,
            }
            hasUserToken := false
            hasEncryptionKey := false

            if userTokenAny := data["userToken"]; userTokenAny != nil {
                if v, ok := userTokenAny.(string); ok {
                    circleUserToken.UserToken = v
                    hasUserToken = true
                } 
            }

            if encryptionKeyAny := data["encryptionKey"]; encryptionKeyAny != nil {
                if v, ok := encryptionKeyAny.(string); ok {
                    circleUserToken.EncryptionKey = v
                    hasEncryptionKey = true
                } 
            }

            if hasUserToken && hasEncryptionKey {
                return circleUserToken, nil
            }
            return nil, fmt.Errorf("Bad user token.")
        },
    )
}


func parseCircleResponseData(responseBodyBytes []byte) (
    responseBody map[string]any,
    resultData map[string]any,
    resultErr error,
) {
    responseBody = map[string]any{}
    err := json.Unmarshal(responseBodyBytes, &responseBody)
    if err != nil {
        resultErr = err
        return
    }

    if data, ok := responseBody["data"]; ok {
        switch v := data.(type) {
        case map[string]any:
            resultData = v
            return
        }
    }
    err = fmt.Errorf("Response is missing data.")
    return
}


func parseCircleChallengeId(responseBodyBytes []byte) (string, error) {
    _, data, err := parseCircleResponseData(responseBodyBytes)
    if err != nil {
        return "", err
    }

    if challengeIdAny := data["challengeId"]; challengeIdAny != nil {
        if v, ok := challengeIdAny.(string); ok {
            return v, nil
        }
    }

    return "", fmt.Errorf("Bad challenge response.")
}


type CircleWalletInfo struct {
    WalletId string `json:"wallet_id"`
    TokenId string `json:"token_id"`
    Blockchain string `json:"blockchain"`
    BlockchainSymbol string `json:"blockchain_symbol"`
    CreateDate time.Time `json:"create_date"`
    BalanceUsdcNanoCents model.NanoCents `json:"balance_usdc_nano_cents"`
    Address string `json:"address"`
}


func findMostRecentCircleWallet(session *session.ClientSession) (*CircleWalletInfo, error) {
    circleWallets, err := findCircleWallets(session)

    if err != nil {
        return nil, err
    }

    if len(circleWallets) == 0 {
        return nil, nil
    }

    mostRecentWalletInfo := circleWallets[0]
    for _, walletInfo := range circleWallets[1:len(circleWallets)] {
        if mostRecentWalletInfo.CreateDate.Before(walletInfo.CreateDate) {
            mostRecentWalletInfo = walletInfo
        }
    }
    return mostRecentWalletInfo, nil
}


func findCircleWallets(session *session.ClientSession) ([]*CircleWalletInfo, error) {
    // list wallets for user. Choose most recent wallet
    // https://api.circle.com/v1/w3s/wallets
    // get token balances for each wallet
    // https://api.circle.com/v1/w3s/wallets/{id}/balances

    circleUserToken, err := createCircleUserToken(session)
    if err != nil {
        return nil, err
    }

    circleApiToken := circleConfig()["api_token"]

    walletInfos, err := bringyour.HttpGetRequireStatusOk(
        "https://api.circle.com/v1/w3s/wallets",
        func(header http.Header) {
            header.Add("Accept", "application/json")
            header.Add("Authorization", fmt.Sprintf("Bearer %s", circleApiToken))
            header.Add("X-User-Token", circleUserToken.UserToken)
        },
        func(response *http.Response, responseBodyBytes []byte)([]*CircleWalletInfo, error) {
            _, data, err := parseCircleResponseData(responseBodyBytes)
            if err != nil {
                return nil, err
            }

            walletInfos := []*CircleWalletInfo{}

            if walletsAny, ok := data["wallets"]; ok {
                if v, ok := walletsAny.([]any); ok {
                    for _, wallet := range v {
                        if walletAny, ok := wallet.(map[string]any); ok {

                            walletInfo := &CircleWalletInfo{}

                            parsedId := false
                            parsedAddress := false
                            parsedCreateDate := false

                            if walletIdAny, ok := walletAny["id"]; ok {
                                if walletId, ok := walletIdAny.(string); ok {
                                    walletInfo.WalletId = walletId
                                    parsedId = true
                                }
                            }

                            if addressAny, ok := walletAny["address"]; ok {
                                if address, ok := addressAny.(string); ok {
                                    walletInfo.Address = address
                                    parsedAddress = true
                                }
                            }


                            // e.g. "createDate":"2023-10-17T22:16:04Z"
                            if createDateAny, ok := walletAny["createDate"]; ok {
                                if createDateStr, ok := createDateAny.(string); ok {
                                    if createDate, err := time.Parse(time.RFC3339, createDateStr); err == nil {
                                        walletInfo.CreateDate = createDate
                                        parsedCreateDate = true
                                    }
                                }
                            }

                            if parsedId && parsedAddress && parsedCreateDate {
                                walletInfos = append(walletInfos, walletInfo)
                            }
                        }
                    }
                }
            }

            return walletInfos, nil
        },
    )
    if err != nil {
        return nil, err
    }

    // complete the wallet infos by parsing the wallet balances
    completeWalletInfos := []*CircleWalletInfo{}

    for _, walletInfo := range walletInfos {
        data, err := bringyour.HttpGetRequireStatusOk(
            fmt.Sprintf("https://api.circle.com/v1/w3s/wallets/%s/balances?includeAll=true", walletInfo.WalletId),
            func(header http.Header) {
                header.Add("Accept", "application/json")
                header.Add("Authorization", fmt.Sprintf("Bearer %s", circleApiToken))
                header.Add("X-User-Token", circleUserToken.UserToken)
            },
            func(response *http.Response, responseBodyBytes []byte)(map[string]any, error) {
                _, data, err := parseCircleResponseData(responseBodyBytes)
                return data, err
            },
        )
        if err != nil {
            return nil, err
        }

        parsedNative := false
        parsedUsdc := false

        if tokenBalancesAny, ok := data["tokenBalances"]; ok {
            if v, ok := tokenBalancesAny.([]any); ok {
                if len(v) == 0 {
                    // there are no tokens in this wallet
                    walletInfo.Blockchain = circleConfig()["blockchain_name"].(string)
                    walletInfo.BlockchainSymbol = circleConfig()["blockchain"].(string)
                    parsedNative = true
                    parsedUsdc = true
                } else {
                    for _, tokenBalance := range v {
                        if tokenBalanceAny, ok := tokenBalance.(map[string]any); ok {
                            if tokenAny, ok := tokenBalanceAny["token"]; ok {
                                if v, ok := tokenAny.(map[string]any); ok {
                                    var native bool
                                    if nativeAny, ok := v["isNative"]; ok {
                                        if v, ok := nativeAny.(bool); ok {
                                            native = v
                                        } else {
                                            // bad token balance
                                            continue
                                        }
                                    }

                                    if native {
                                        parsedName := false
                                        parsedSymbol := false
                                        
                                        if nameAny, ok := v["name"]; ok {
                                            if name, ok := nameAny.(string); ok {
                                                walletInfo.Blockchain = name
                                                parsedName = true
                                            }
                                        }

                                        if symbolAny, ok := v["symbol"]; ok {
                                            if symbol, ok := symbolAny.(string); ok {
                                                walletInfo.BlockchainSymbol = symbol
                                                parsedSymbol = true
                                            }
                                        }

                                        parsedNative = parsedName && parsedSymbol
                                    } else if v["symbol"] == "USDC" {
                                        // usdc

                                        parsedTokenId := false
                                        parsedBalance := false

                                        if tokenIdAny, ok := v["id"]; ok {
                                            if tokenId, ok := tokenIdAny.(string); ok {
                                                walletInfo.TokenId = tokenId
                                                parsedTokenId = true
                                            } 
                                        }

                                        if amountAny, ok := tokenBalanceAny["amount"]; ok {
                                            if amountStr, ok := amountAny.(string); ok {
                                                if amountUsdc, err := strconv.ParseFloat(amountStr, 64); err == nil {
                                                    walletInfo.BalanceUsdcNanoCents = model.UsdToNanoCents(amountUsdc)
                                                    parsedBalance = true
                                                }
                                            }
                                        }

                                        parsedUsdc = parsedTokenId && parsedBalance
                                    }
                                }
                            }
                        }
                    }
                }
            }
        }

        if parsedNative && parsedUsdc {
            // fully parsed
            completeWalletInfos = append(completeWalletInfos, walletInfo)
        }
    }

    return completeWalletInfos, nil
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

type SendPaymentArgs struct {
    Amount float64
    DestinationAddress string
    Network string
}

func EstimateTransferFee(
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

type CircleTransferTransactionResult struct {
    Id string `json:"id"`
    State string `json:"state"`
}

type SendPaymentResult struct {}

func SendPayment(sendPayment SendPaymentArgs) (*CircleTransferTransactionResult, error) {

    hexEncodedEntitySecret := entitySecret()

    cipher, err := generateEntitySecretCipher(hexEncodedEntitySecret)
    if err != nil {
        fmt.Printf("Error generating entity secret cipher: %s", err)
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

var solanaUSDCAddress = sync.OnceValue(func() string {
    c := bringyour.Vault.RequireSimpleResource("circle.yml").Parse()
    return c["circle"].(map[string]any)["solana_usdc_address"].(string)
})

var polygonUSDCAddress = sync.OnceValue(func() string {
    c := bringyour.Vault.RequireSimpleResource("circle.yml").Parse()
    return c["circle"].(map[string]any)["polygon_usdc_address"].(string)
})

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

var solanaWalletId = sync.OnceValue(func()(string) {
    c := bringyour.Vault.RequireSimpleResource("circle.yml").Parse()
	return c["circle"].(map[string]any)["solana_wallet_id"].(string)
})

var polygonWalletId = sync.OnceValue(func()(string) {
    c := bringyour.Vault.RequireSimpleResource("circle.yml").Parse()
	return c["circle"].(map[string]any)["polygon_wallet_id"].(string)
})

func getWalletIdByNetwork(network string) (id string, err error) {
    network = strings.TrimSpace(network)
    network = strings.ToUpper(network)
    fmt.Println("Network: ", network)

    switch network {
    case "SOL", "SOLANA":
        id = solanaWalletId()
        fmt.Println("Solana Wallet ID: ", id)
    case "MATIC", "POLY", "POLYGON":
        id = polygonWalletId()
    default:
        err = fmt.Errorf("unsupported network: %s", network)
    }

    return
}

func CalcuateFeePolygon(feeEstimate FeeEstimate) (*float64, error) {

    gasLimit, err := strconv.ParseFloat(feeEstimate.GasLimit, 64)
    if err != nil {
        return nil, err
    }

    baseFee, err := strconv.ParseFloat(feeEstimate.BaseFee, 64)
    if err != nil {
        return nil, err
    }

    totalFee := baseFee * gasLimit

    return &totalFee, nil
}

func ConvertFeeToUSDC(currencyTicker string, fee float64) (*float64, error) {

    ratesResult, err := CoinbaseFetchExchangeRates(currencyTicker)
    if err != nil {
        return nil, err
    }

    rateStr, exists := ratesResult.Rates["USDC"]
    if !exists {
        return nil, fmt.Errorf("currency ticker not found for %s", currencyTicker)
    }

    rate, err := strconv.ParseFloat(rateStr, 64)
    if err != nil {
        return nil, fmt.Errorf("failed to parse rate: %v", err)
    }

    feeUsdc := fee * rate

    return &feeUsdc, nil

}