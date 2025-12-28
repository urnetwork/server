package controller

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	mathrand "math/rand"
	"net/http"
	"strings"
	"sync"

	// "io"
	"strconv"
	"time"

	// "strings"

	"github.com/urnetwork/glog/v2025"

	"github.com/urnetwork/server/v2025"
	"github.com/urnetwork/server/v2025/jwt"
	"github.com/urnetwork/server/v2025/model"
	"github.com/urnetwork/server/v2025/session"

	"github.com/gagliardetto/solana-go"
)

var circleConfig = sync.OnceValue(func() map[string]any {
	c := server.Vault.RequireSimpleResource("circle.yml").Parse()
	return c["circle"].(map[string]any)
})

var walletSetId = sync.OnceValue(func() string {
	c := server.Vault.RequireSimpleResource("circle.yml").Parse()
	return c["wallet_set_id"].(string)
})

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

	return server.HttpPostRequireStatusOk(
		session.Ctx,
		"https://api.circle.com/v1/w3s/user/initialize",
		map[string]any{
			"idempotencyKey": server.NewId(),
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

type WalletValidateAddressResult struct {
	Valid bool `json:"valid,omitempty"`
}

type WalletValidateAddressArgs struct {
	Address string `json:"address,omitempty"`
	Chain   string `json:"chain,omitempty"` // https://developers.circle.com/w3s/reference/createvalidateaddress for valid blockchain params
}

func WalletValidateAddress(
	walletValidateAddress *WalletValidateAddressArgs,
	session *session.ClientSession,
) (*WalletValidateAddressResult, error) {

	if walletValidateAddress.Address == solanaUSDCAddress() {
		return &WalletValidateAddressResult{
			Valid: false,
		}, nil
	}

	_, err := solana.PublicKeyFromBase58(walletValidateAddress.Address)
	if err != nil {
		return &WalletValidateAddressResult{
			Valid: false,
		}, nil
	}

	return &WalletValidateAddressResult{
		Valid: true,
	}, nil
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
) (result *WalletCircleTransferOutResult, returnErr error) {
	if !walletCircleTransferOut.Terms {
		returnErr = fmt.Errorf("You must accept the terms of transfer.")
		return
	}

	walletInfo, err := findMostRecentCircleWallet(session)
	if err != nil {
		returnErr = err
		return
	}

	if walletInfo == nil {
		returnErr = fmt.Errorf("no wallet info found")
		return
	}

	circleUserToken, err := createCircleUserToken(session)
	if err != nil {
		returnErr = err
		return
	}

	// retry with timeout
	for i := range 4 {
		select {
		case <-session.Ctx.Done():
			returnErr = fmt.Errorf("Done.")
			return
		case <-time.After(500*time.Millisecond + time.Duration(mathrand.Int63n(int64(i+1)*int64(time.Second)))):
		}

		result, returnErr = server.HttpPostRequireStatusOk(
			session.Ctx,
			"https://api.circle.com/v1/w3s/user/transactions/transfer",
			map[string]any{
				"userId":             circleUserToken.circleUserId,
				"destinationAddress": walletCircleTransferOut.ToAddress,
				"amounts": []string{
					fmt.Sprintf("%f", model.NanoCentsToUsd(walletCircleTransferOut.AmountUsdcNanoCents)),
				},
				"idempotencyKey": server.NewId(),
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
		if returnErr == nil {
			return
		}
	}
	return
}

type GetPublicKeyResult struct {
	PublicKey string `json:"publicKey"`
}

func getPublicKey(ctx context.Context) (*string, error) {

	circleApiToken := circleConfig()["api_token"]

	url := "https://api.circle.com/v1/w3s/config/entity/publicKey"

	publicKey, err := server.HttpGetRequireStatusOk(
		ctx,
		url,
		func(header http.Header) {
			header.Add("Accept", "application/json")
			header.Add("Authorization", fmt.Sprintf("Bearer %s", circleApiToken))
		},
		func(response *http.Response, responseBodyBytes []byte) (*string, error) {
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
	circleUserId server.Id,
	resultErr error,
) {
	circleUserId = model.GetOrCreateCircleUserId(
		session.Ctx,
		session.ByJwt.NetworkId,
		session.ByJwt.UserId,
	)

	circleApiToken := circleConfig()["api_token"]

	_, resultErr = server.HttpPost(
		session.Ctx,
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
	circleUserId  server.Id
	UserToken     string `json:"user_token"`
	EncryptionKey string `json:"encryption_key"`
}

func createCircleUserToken(
	session *session.ClientSession,
) (*CircleUserToken, error) {
	circleUserId := model.GetOrCreateCircleUserId(
		session.Ctx,
		session.ByJwt.NetworkId,
		session.ByJwt.UserId,
	)

	circleApiToken := circleConfig()["api_token"]

	return server.HttpPostRequireStatusOk(
		session.Ctx,
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

func VerifyCircleBody(req *http.Request) (io.Reader, error) {

	// server.Logger().Println("VerifyCircleBody")

	bodyBytes, err := io.ReadAll(req.Body)
	if err != nil {
		glog.Infof("[wallet]VerifyCircleBody: reading body err: %s\n", err.Error())
		return nil, err
	}

	err = verifyCircleAuth(
		req.Context(),
		req.Header.Get("X-Circle-Key-Id"),
		req.Header.Get("X-Circle-Signature"),
		bodyBytes,
	)
	if err != nil {
		// server.Logger().Printf("VerifyCircleBody: verifyCircleAuth: %s\n", err.Error())
		return nil, err
	}

	// server.Logger().Println("VerifyCircleBody: continuing")

	return bytes.NewReader(bodyBytes), nil
}

func verifyCircleAuth(ctx context.Context, keyId string, signature string, responseBodyBytes []byte) error {

	circleApiToken := circleConfig()["api_token"]

	pk, err := server.HttpGetRequireStatusOk(
		ctx,
		fmt.Sprintf("https://api.circle.com/v2/notifications/publicKey/%s", keyId),
		func(header http.Header) {
			header.Add("Accept", "application/json")
			header.Add("Authorization", fmt.Sprintf("Bearer %s", circleApiToken))
		},
		func(response *http.Response, responseBodyBytes []byte) (*string, error) {
			_, data, err := parseCircleResponseData(responseBodyBytes)
			if err != nil {
				// server.Logger().Printf("verifyCircleAuth: parseCircleResponseData err: %s\n", err.Error())
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
		// server.Logger().Printf("verifyCircleAuth: verifySignature err: %s\n", err.Error())
		return err
	}

	return nil
}

type ECDSASignature struct {
	R, S *big.Int
}

func verifySignature(publicKeyBase64 string, signatureBase64 string, responseBodyBytes []byte) error {
	// server.Logger().Printf("verifySignature: publicKeyBase64: %s\n", publicKeyBase64)
	// server.Logger().Printf("verifySignature: signatureBase64: %s\n", signatureBase64)
	// server.Logger().Printf("verifySignature: responseBodyBytes: %s\n", string(responseBodyBytes))

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

	// Parse the ASN.1 DER-encoded signature
	var ecdsaSig ECDSASignature
	_, err = asn1.Unmarshal(signatureBytes, &ecdsaSig)
	if err != nil {
		return fmt.Errorf("failed to unmarshal DER signature: %w", err)
	}

	// compact the JSON before hashing
	var compactJSON bytes.Buffer
	if err := json.Compact(&compactJSON, responseBodyBytes); err != nil {
		return fmt.Errorf("failed to compact JSON: %w", err)
	}
	formattedJSONBytes := compactJSON.Bytes()

	// Hash the compacted JSON response body
	hash := sha256.Sum256(formattedJSONBytes)

	// Verify the signature using the parsed r and s values
	if ecdsa.Verify(ecdsaPublicKey, hash[:], ecdsaSig.R, ecdsaSig.S) {
		return nil
	} else {
		return fmt.Errorf("signature verification failed")
	}
}

type CircleNotification[T any] struct {
	SubscriptionId   string    `json:"subscriptionId"`
	NotificationId   string    `json:"notificationId"`
	NotificationType string    `json:"notificationType"`
	Notification     T         `json:"notification"`
	Timestamp        time.Time `json:"timestamp"`
	Version          int       `json:"version"`
}

type CircleWallet struct {
	ID           string    `json:"id"`
	State        string    `json:"state"`
	WalletSetId  string    `json:"walletSetId"`
	CustodyType  string    `json:"custodyType"`
	UserId       string    `json:"userId"`
	Address      string    `json:"address"`
	AddressIndex int       `json:"addressIndex"`
	Blockchain   string    `json:"blockchain"`
	UpdateDate   time.Time `json:"updateDate"`
	CreateDate   time.Time `json:"createDate"`
}

type CircleChallengeEvent struct {
	ID             string   `json:"id"`
	UserId         string   `json:"userId"`
	Status         string   `json:"status"`
	CorrelationIds []string `json:"correlationIds"`
	ErrorCode      int      `json:"errorCode"`
	Type           string   `json:"type"`
}

type CircleWalletWebhookResult struct{}

func CircleWalletWebhook(
	circleWalletWebhook *CircleNotification[CircleChallengeEvent],
	clientSession *session.ClientSession,
) (*CircleWalletWebhookResult, error) {

	// server.Logger().Println("CircleWalletWebhook:")

	if circleWalletWebhook.NotificationType == "challenges.initialize" {

		event := circleWalletWebhook.Notification
		status := strings.ToUpper(event.Status)
		eventType := strings.ToUpper(event.Type)

		if eventType == "INITIALIZE" && status == "COMPLETE" {

			if len(event.CorrelationIds) <= 0 {
				return nil, fmt.Errorf("no correlation ids")
			}

			circleWalletId := event.CorrelationIds[0]

			circleWallet, err := getCircleWallet(clientSession.Ctx, circleWalletId)
			if err != nil {
				// server.Logger().Printf("CircleWalletWebhook: getCircleWalletErr: %s\n", err.Error())

				return nil, err
			}

			userId, err := server.ParseId(event.UserId)
			if err != nil {
				// server.Logger().Printf("CircleWalletWebhook: ParseId: %s\n", err.Error())
				return nil, err
			}

			userUC := model.GetCircleUCByCircleUCUserId(clientSession.Ctx, userId)
			if userUC == nil {
				// server.Logger().Printf("CircleWalletWebhook: GetCircleUCByCircleUCUserId no user found: %s\n", userId)
				return nil, fmt.Errorf("no circle user control found")
			}

			blockchain := strings.ToUpper(circleWallet.Blockchain)

			// check for an existing wallet
			existingAccountWallet := model.GetAccountWalletByCircleId(clientSession.Ctx, circleWalletId)
			if existingAccountWallet != nil {
				return nil, fmt.Errorf("account wallet already exists")
			}

			wallet := &model.CreateAccountWalletCircleArgs{
				CircleWalletId:   circleWallet.ID,
				NetworkId:        userUC.NetworkId,
				Blockchain:       blockchain,
				WalletAddress:    circleWallet.Address,
				DefaultTokenType: "USDC",
			}

			// server.Logger().Println("CircleWalletWebhook: about to CreateAccountWalletCircle")

			// no account_wallet exists, create a new one
			walletId := model.CreateAccountWalletCircle(
				clientSession.Ctx,
				wallet,
			)

			if walletId == nil {
				// server.Logger().Println("CircleWalletWebhook: no wallet id found")
				return nil, fmt.Errorf("error creating account wallet")
			}

			// check if a payout wallet is set for this network
			payoutWallet := model.GetPayoutWalletId(clientSession.Ctx, userUC.NetworkId)

			// if a payout wallet doesn't exist for the network
			// set payout wallet
			if payoutWallet == nil {
				model.SetPayoutWallet(clientSession.Ctx, userUC.NetworkId, *walletId)
			}

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

func getCircleWallet(ctx context.Context, circleWalletId string) (*CircleWallet, error) {
	return server.HttpGetRequireStatusOk(
		ctx,
		fmt.Sprintf("https://api.circle.com/v1/w3s/wallets/%s", circleWalletId),
		func(header http.Header) {
			header.Add("Accept", "application/json")
			header.Add("Authorization", fmt.Sprintf("Bearer %s", circleConfig()["api_token"]))
		},
		func(response *http.Response, responseBodyBytes []byte) (*CircleWallet, error) {
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

	walletInfos, err := server.HttpGetRequireStatusOk(
		session.Ctx,
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
		data, err := server.HttpGetRequireStatusOk(
			session.Ctx,
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
										parsedBlockchain := false
										parsedSymbol := false

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

										if blockchainAny, ok := v["blockchain"]; ok {
											if blockchain, ok := blockchainAny.(string); ok {
												walletInfo.Blockchain = blockchain
												parsedBlockchain = true
											}
										}

										if symbolAny, ok := v["symbol"]; ok {
											if symbol, ok := symbolAny.(string); ok {
												walletInfo.BlockchainSymbol = symbol
												parsedSymbol = true
											}
										}

										parsedUsdc = parsedTokenId && parsedBalance && parsedBlockchain && parsedSymbol
									}
								}
							}
						}
					}
				}
			}
		}

		if parsedNative || parsedUsdc {
			// fully parsed
			completeWalletInfos = append(completeWalletInfos, walletInfo)
		}
	}

	return completeWalletInfos, nil
}

type PopulateAccountWalletsArgs struct {
}

type PopulateAccountWalletsResult struct {
}

func PopulateAccountWallets(
	populateAccountWallet *PopulateAccountWalletsArgs,
	clientSession *session.ClientSession,
) (*PopulateAccountWalletsResult, error) {

	circleUCUsers := model.GetCircleUCUsers(clientSession.Ctx)

	if len(circleUCUsers) == 0 {
		return nil, fmt.Errorf("no users found")
	}

	glog.V(2).Infof("[walletc]%d circle users found \n", len(circleUCUsers))

	errUserIds := []server.Id{}

	for _, user := range circleUCUsers {
		time.Sleep(500 * time.Millisecond)
		err := handleUser(user, clientSession)
		if err != nil {
			glog.Infof("[walletc]error for user %s: %v \n", user.UserId.String(), err)
			errUserIds = append(errUserIds, user.CircleUCUserId)
		}
	}

	// create a JSON file with the user ids that failed
	if len(errUserIds) > 0 {
		errUserIdStrs := []string{}
		for _, errUserId := range errUserIds {
			errUserIdStrs = append(errUserIdStrs, errUserId.String())
		}
		glog.Infof("[walletc]Error creating account wallets for the following users: %s", strings.Join(errUserIdStrs, ", "))
	}

	glog.V(2).Infof("[walletc]PopulateAccountWallets done")

	return &PopulateAccountWalletsResult{}, nil

}

func handleUser(user model.CircleUC, clientSession *session.ClientSession) error {
	userSession := session.NewLocalClientSession(clientSession.Ctx, "0.0.0.0:0", &jwt.ByJwt{
		NetworkId: user.NetworkId,
		UserId:    user.UserId,
	})

	walletInfo, err := findCircleWallets(userSession)
	if err != nil {
		return err
	}

	for i, circleWallet := range walletInfo {

		// check if account wallet exists
		accountWallet := model.GetAccountWalletByCircleId(clientSession.Ctx, circleWallet.WalletId)
		if accountWallet != nil {
			glog.V(2).Infof("[walletc]Account wallet already exists for wallet id %s \n", circleWallet.WalletId)
			continue
		}

		createAccountWallet := &model.CreateAccountWalletCircleArgs{
			CircleWalletId:   circleWallet.WalletId,
			NetworkId:        user.NetworkId,
			Blockchain:       circleWallet.Blockchain,
			WalletAddress:    circleWallet.Address,
			DefaultTokenType: "USDC",
		}
		walletId := model.CreateAccountWalletCircle(clientSession.Ctx, createAccountWallet)

		if walletId == nil {
			return fmt.Errorf("Error creating Circle account wallet")
		}

		// set the payout wallet
		if i == 0 {
			model.SetPayoutWallet(clientSession.Ctx, user.NetworkId, *walletId)
		}
	}

	return nil
}
