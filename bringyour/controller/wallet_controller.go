package controller

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
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

func VerifyCircleBody(req *http.Request)(io.Reader, error) {

    bodyBytes, err := io.ReadAll(req.Body)
    if err != nil {
        return nil, err
    }

	err = verifyCircleAuth(
		req.Header.Get("X-Circle-Key-Id"), 
		req.Header.Get("X-Circle-Signature"),
        bodyBytes,
	)
	if err != nil {
		return nil, err
	}

	return bytes.NewReader(bodyBytes), nil
}

func verifyCircleAuth(keyId string, signature string, responseBodyBytes []byte) error {

    circleApiToken := circleConfig()["api_token"]

	pk, err := bringyour.HttpGetRequireStatusOk(
		fmt.Sprintf("https://api.circle.com/v2/notifications/publicKey/%s", keyId),
        func(header http.Header) {
            header.Add("Accept", "application/json")
            header.Add("Authorization", fmt.Sprintf("Bearer %s", circleApiToken))
        },
        func(response *http.Response, responseBodyBytes []byte) (*string, error) {
            _, data, err := parseCircleResponseData(responseBodyBytes)
            if err != nil {
                return nil, err
            }

            if publicKey := data["publicKey"]; publicKey != nil {
                if pk, ok := publicKey.(string); ok {
                    return &pk, nil
                }
            }

            return nil, fmt.Errorf("no public key found")
        },
    )
    

    if err != nil {
        return err
    }

    err = verifySignature(*pk, signature, responseBodyBytes)
    if err != nil {
        return err
    }

	return nil
}

func verifySignature(publicKeyBase64 string, signatureBase64 string, responseBodyBytes []byte) error {
	// Decode the public key from base64
	publicKeyDer, err := base64.StdEncoding.DecodeString(publicKeyBase64)
	if err != nil {
		return fmt.Errorf("failed to decode public key: %w", err)
	}

	// Parse the public key
	publicKey, err := x509.ParsePKIXPublicKey(publicKeyDer)
	if err != nil {
		return fmt.Errorf("failed to parse public key: %w", err)
	}
	ecdsaPublicKey, ok := publicKey.(*ecdsa.PublicKey)
	if !ok {
		return errors.New("failed to cast public key to ECDSA")
	}

	// Decode the signature from base64
	signatureBytes, err := base64.StdEncoding.DecodeString(signatureBase64)
	if err != nil {
		return fmt.Errorf("failed to decode signature: %w", err)
	}

	// Format the JSON response body
	var jsonData map[string]interface{}
	if err := json.Unmarshal(responseBodyBytes, &jsonData); err != nil {
		return fmt.Errorf("failed to unmarshal JSON response body: %w", err)
	}
	formattedJson, err := json.Marshal(jsonData)
	if err != nil {
		return fmt.Errorf("failed to marshal JSON data: %w", err)
	}

	// Hash the formatted JSON response body
	hash := sha256.Sum256(formattedJson)

	// Verify the signature
	r := big.Int{}
	s := big.Int{}
	sigLen := len(signatureBytes)
	r.SetBytes(signatureBytes[:sigLen/2])
	s.SetBytes(signatureBytes[sigLen/2:])

	if ecdsa.Verify(ecdsaPublicKey, hash[:], &r, &s) {
		return nil // Signature is valid
	} else {
        return fmt.Errorf("signature is invalid")
	}
}

type CircleNotification[T any] struct {
    SubscriptionId string `json:"subscriptionId"`
    NotificationId string `json:"notificationId"`
    NotificationType string `json:"notificationType"`
    Notification T `json:"notification"`
    Timestamp time.Time `json:"timestamp"`
    Version int `json:"version"`
}

type CircleWallet struct {
    ID           string     `json:"id"`
    State        string     `json:"state"`
    WalletSetId  string     `json:"walletSetId"`
    CustodyType  string     `json:"custodyType"`
    UserId       string     `json:"userId"`
    Address      string     `json:"address"`
    AddressIndex int        `json:"addressIndex"`
    Blockchain   string     `json:"blockchain"`
    UpdateDate   time.Time  `json:"updateDate"`
    CreateDate   time.Time  `json:"createDate"`
}

type CircleChallengeEvent struct {
    ID string `json:"id"`
    UserId string `json:"userId"`
    Status string `json:"status"`
    CorrelationIds []string `json:"correlationIds"`
    ErrorCode int `json:"errorCode"`
    Type string `json:"type"`
}

type CircleWalletWebhookResult struct {}

func CircleWalletWebhook(
    circleWalletWebhook *CircleNotification[CircleChallengeEvent],
    clientSession *session.ClientSession,
) (*CircleWalletWebhookResult, error) {

    if circleWalletWebhook.NotificationType == "challenges.initialize" {

        event := circleWalletWebhook.Notification
        status := strings.ToUpper(event.Status)
        eventType := strings.ToUpper(event.Type)
    
        if eventType == "INITIALIZE" && status == "COMPLETE" {

            if len(event.CorrelationIds) <= 0 {
                return nil, fmt.Errorf("no correlation ids")
            }
    
            walletId, err := bringyour.ParseId(event.CorrelationIds[0])
            if err != nil {
                return nil, err
            }

            wallet, err := getCircleWallet(walletId)
            if err != nil {
                return nil, err
            }
    
            userId, err := bringyour.ParseId(event.UserId)
            if err != nil {
                return nil, err
            }
    
            userUC := model.GetCircleUCByCircleUCUserId(clientSession.Ctx, userId)
            if userUC == nil {
                return nil, fmt.Errorf("no circle user control found")
            }
    
            blockchain := strings.ToUpper(wallet.Blockchain)
    
            // check for an existing wallet
            existingAccountWallet := model.GetAccountWallet(clientSession.Ctx, walletId)
            if existingAccountWallet != nil {
                return nil, fmt.Errorf("account wallet already exists")
            }
    
            // no account_wallet exists, create a new one
            model.CreateAccountWallet(
                clientSession.Ctx,
                &model.AccountWallet{
                    WalletId: walletId,
                    NetworkId: userUC.NetworkId,
                    WalletType: model.WalletTypeCircleUserControlled,
                    Blockchain: blockchain,
                    WalletAddress: wallet.Address,
                    DefaultTokenType: "USDC",
                },
            )
    
            // fixme: check if a payout wallet is set for this network
            // depends on feature/set-payout-wallet
    
        }

    }

    return &CircleWalletWebhookResult{}, nil

}

type CircleResult[T any] struct {
    Data T `json:"data"`
}

type CircleWalletResult struct {
    Wallet CircleWallet `json:"wallet"`
}

func getCircleWallet(walletId bringyour.Id) (*CircleWallet, error) {
    return bringyour.HttpGetRequireStatusOk(
        fmt.Sprintf("https://api.circle.com/v1/w3s/wallets/%s", walletId),
        func(header http.Header) {
            header.Add("Accept", "application/json")
            header.Add("Authorization", fmt.Sprintf("Bearer %s", circleConfig()["api_token"]))
        },
        func(response *http.Response, responseBodyBytes []byte)(*CircleWallet, error) {
			result := &CircleResult[CircleWalletResult]{}

			err := json.Unmarshal(responseBodyBytes, result)
			if err != nil {
				return nil, err
			}

			return &result.Data.Wallet, nil
        },
    )
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

