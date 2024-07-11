package controller

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	// "io"
	"strconv"
	"time"

	// "strings"

	"bringyour.com/bringyour"
	"bringyour.com/bringyour/model"
	"bringyour.com/bringyour/session"
)

type WalletCircleInitResult struct {
	UserToken   *CircleUserToken       `json:"user_token,omitempty"`
	ChallengeId string                 `json:"challenge_id,omitempty"`
	Error       *WalletCircleInitError `json:"error,omitempty"`
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
			"accountType":    "SCA",
			"blockchains": []string{
				circleConfig()["blockchain"].(string),
			},
		},
		func(header http.Header) {
			header.Add("Accept", "application/json")
			header.Add("Authorization", fmt.Sprintf("Bearer %s", circleConfig()["api_token"]))
			header.Add("X-User-Token", circleUserToken.UserToken)
		},
		func(response *http.Response, responseBodyBytes []byte) (*WalletCircleInitResult, error) {
			challengeId, err := parseCircleChallengeId(responseBodyBytes)

			if err == nil {
				return &WalletCircleInitResult{
					UserToken:   circleUserToken,
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
	Chain   string `json:"chain,omitempty"` // https://developers.circle.com/w3s/reference/createvalidateaddress for valid blockchain params
}

type WalletValidateAddressResult struct {
	Valid bool `json:"valid,omitempty"`
}

func WalletValidateAddress(
	walletValidateAddress *WalletValidateAddressArgs,
	session *session.ClientSession,
) (*WalletValidateAddressResult, error) {
	return bringyour.HttpPostRequireStatusOk(
		"https://api.circle.com/v1/w3s/transactions/validateAddress",
		map[string]any{
			"blockchain": walletValidateAddress.Chain,
			"address":    walletValidateAddress.Address,
		},
		func(header http.Header) {
			header.Add("Accept", "application/json")
			header.Add("Authorization", fmt.Sprintf("Bearer %s", circleConfig()["api_token"]))
		},
		func(response *http.Response, responseBodyBytes []byte) (*WalletValidateAddressResult, error) {
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

var circleConfig = sync.OnceValue(func() map[string]any {
	c := bringyour.Vault.RequireSimpleResource("circle.yml").Parse()
	return c["circle"].(map[string]any)
})

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
	ToAddress           string          `json:"to_address"`
	AmountUsdcNanoCents model.NanoCents `json:"amount_usdc_nano_cents"`
	Terms               bool            `json:"terms"`
}

type WalletCircleTransferOutResult struct {
	UserToken   *CircleUserToken              `json:"user_token,omitempty"`
	ChallengeId string                        `json:"challenge_id,omitempty"`
	Error       *WalletCircleTransferOutError `json:"error,omitempty"`
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
			"userId":             circleUserToken.circleUserId,
			"destinationAddress": walletCircleTransferOut.ToAddress,
			"amounts": []string{
				fmt.Sprintf("%f", model.NanoCentsToUsd(walletCircleTransferOut.AmountUsdcNanoCents)),
			},
			"idempotencyKey": bringyour.NewId(),
			"tokenId":        walletInfo.TokenId,
			"walletId":       walletInfo.WalletId,
			"feeLevel":       "LOW",
		},
		func(header http.Header) {
			header.Add("Accept", "application/json")
			header.Add("Authorization", fmt.Sprintf("Bearer %s", circleConfig()["api_token"]))
			header.Add("X-User-Token", circleUserToken.UserToken)
		},
		func(response *http.Response, responseBodyBytes []byte) (*WalletCircleTransferOutResult, error) {
			challengeId, err := parseCircleChallengeId(responseBodyBytes)

			if err == nil {
				return &WalletCircleTransferOutResult{
					UserToken:   circleUserToken,
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
		func(response *http.Response, responseBodyBytes []byte) (any, error) {
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
	circleUserId  bringyour.Id
	UserToken     string `json:"user_token"`
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
		func(response *http.Response, responseBodyBytes []byte) (*CircleUserToken, error) {
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
	WalletId             string          `json:"wallet_id"`
	TokenId              string          `json:"token_id"`
	Blockchain           string          `json:"blockchain"`
	BlockchainSymbol     string          `json:"blockchain_symbol"`
	CreateDate           time.Time       `json:"create_date"`
	BalanceUsdcNanoCents model.NanoCents `json:"balance_usdc_nano_cents"`
	Address              string          `json:"address"`
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
		func(response *http.Response, responseBodyBytes []byte) ([]*CircleWalletInfo, error) {
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
			func(response *http.Response, responseBodyBytes []byte) (map[string]any, error) {
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
