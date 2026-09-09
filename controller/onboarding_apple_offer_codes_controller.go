// SPDX-License-Identifier: MPL-2.0

package controller

// App Store one-time offer codes for the welcome offer (mmm/onboarding/PLAN.md
// "THE OFFER", App Store). A daily task keeps the pool table topped up with
// fresh one-time-use codes generated through the App Store Connect API; each
// issued offer takes one code (model.IssueOnboardingOffer). Without credentials
// the task logs a warning and does nothing, and the custom code
// (onboarding.yml offer.apple_custom_offer_code) stays the App Store path.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// appStoreConnectCredentials is vault/main/apple_app_store_connect.yml:
//
//	app_store_connect:
//	  key_id: <App Store Connect API key id>
//	  issuer_id: <issuer id>
//	  private_key: |
//	    -----BEGIN PRIVATE KEY-----
//	    ...
//	    -----END PRIVATE KEY-----
//
// The key needs the App Manager role (offer codes are an app-level write).
type appStoreConnectCredentials struct {
	KeyId      string
	IssuerId   string
	PrivateKey string
}

func appStoreConnectCredentialsFromVault() *appStoreConnectCredentials {
	resource, err := server.Vault.SimpleResource("apple_app_store_connect.yml")
	if err != nil {
		return nil
	}
	var config struct {
		AppStoreConnect *struct {
			KeyId      string `yaml:"key_id"`
			IssuerId   string `yaml:"issuer_id"`
			PrivateKey string `yaml:"private_key"`
		} `yaml:"app_store_connect"`
	}
	if err := resource.UnmarshalYamlE(&config); err != nil || config.AppStoreConnect == nil {
		return nil
	}
	c := config.AppStoreConnect
	if strings.TrimSpace(c.KeyId) == "" || strings.TrimSpace(c.IssuerId) == "" || strings.TrimSpace(c.PrivateKey) == "" {
		return nil
	}
	return &appStoreConnectCredentials{
		KeyId:      strings.TrimSpace(c.KeyId),
		IssuerId:   strings.TrimSpace(c.IssuerId),
		PrivateKey: c.PrivateKey,
	}
}

var appStoreConnectCredentialsFunc = appStoreConnectCredentialsFromVault

var appStoreConnectBaseUrl = "https://api.appstoreconnect.apple.com"

// appStoreConnectToken signs the ES256 JWT the App Store Connect API takes
// (aud appstoreconnect-v1, no bid: this is not the Server API).
func appStoreConnectToken(creds *appStoreConnectCredentials) (string, error) {
	key, err := gojwt.ParseECPrivateKeyFromPEM([]byte(creds.PrivateKey))
	if err != nil {
		return "", fmt.Errorf("parse App Store Connect private key: %w", err)
	}
	now := server.NowUtc()
	token := gojwt.NewWithClaims(gojwt.SigningMethodES256, gojwt.MapClaims{
		"iss": creds.IssuerId,
		"iat": now.Unix(),
		"exp": now.Add(15 * time.Minute).Unix(),
		"aud": "appstoreconnect-v1",
	})
	token.Header["kid"] = creds.KeyId
	return token.SignedString(key)
}

// ----- the top-up decision (pure) -----

// AppleOfferCodeTopUpDecision is what the daily task decides from the pool state and
// the config.
type AppleOfferCodeTopUpDecision struct {
	// Generate is whether a batch is requested now
	Generate bool
	// Reason is why not, when Generate is false
	Reason string
	// BatchSize and ExpiresAt of the batch to request
	BatchSize int
	ExpiresAt time.Time
}

// DecideAppleOfferCodeTopUp: no offer code id or no credentials = inert; a pool
// above min_available = nothing to do; else one batch of batch_size (never
// below Apple's minimum of 500) expiring expiry_days from now.
func DecideAppleOfferCodeTopUp(cfg model.OnboardingAppleOfferCodesConfig, hasCredentials bool, available int, now time.Time) AppleOfferCodeTopUpDecision {
	if strings.TrimSpace(cfg.OfferCodeId) == "" {
		return AppleOfferCodeTopUpDecision{Reason: "onboarding.offer.apple.offer_code_id is empty"}
	}
	if !hasCredentials {
		return AppleOfferCodeTopUpDecision{Reason: "vault/main/apple_app_store_connect.yml is absent or incomplete"}
	}
	minAvailable := cfg.MinAvailable
	if minAvailable <= 0 {
		minAvailable = 200
	}
	if minAvailable <= available {
		return AppleOfferCodeTopUpDecision{Reason: fmt.Sprintf("%d codes available, above the %d floor", available, minAvailable)}
	}
	batch := cfg.BatchSize
	if batch < 500 {
		batch = 500
	}
	days := cfg.ExpiryDays
	if days <= 0 {
		days = 5
	}
	return AppleOfferCodeTopUpDecision{
		Generate:  true,
		BatchSize: batch,
		ExpiresAt: now.Add(time.Duration(days) * 24 * time.Hour),
	}
}

// ----- the App Store Connect calls -----

type ascOneTimeUseCodesCreateArgs struct {
	Data struct {
		Type       string `json:"type"`
		Attributes struct {
			NumberOfCodes  int    `json:"numberOfCodes"`
			ExpirationDate string `json:"expirationDate"`
		} `json:"attributes"`
		Relationships struct {
			OfferCode struct {
				Data struct {
					Type string `json:"type"`
					Id   string `json:"id"`
				} `json:"data"`
			} `json:"offerCode"`
		} `json:"relationships"`
	} `json:"data"`
}

type ascOneTimeUseCodesCreateResult struct {
	Data struct {
		Id         string `json:"id"`
		Attributes struct {
			NumberOfCodes  int    `json:"numberOfCodes"`
			ExpirationDate string `json:"expirationDate"`
			Active         bool   `json:"active"`
		} `json:"attributes"`
	} `json:"data"`
	Errors []struct {
		Status string `json:"status"`
		Code   string `json:"code"`
		Title  string `json:"title"`
		Detail string `json:"detail"`
	} `json:"errors"`
}

func ascHeader(token string) server.HeaderCallback {
	return func(header http.Header) {
		header.Add("Authorization", "Bearer "+token)
	}
}

// generateAppleOfferCodeBatch creates one-time-use codes for the offer code and
// downloads their values (App Store Connect returns the batch as CSV).
func generateAppleOfferCodeBatch(ctx context.Context, creds *appStoreConnectCredentials, offerCodeId string, count int, expiresAt time.Time) ([]*model.AppleOfferCode, error) {
	token, err := appStoreConnectToken(creds)
	if err != nil {
		return nil, err
	}
	args := &ascOneTimeUseCodesCreateArgs{}
	args.Data.Type = "subscriptionOfferCodeOneTimeUseCodes"
	args.Data.Attributes.NumberOfCodes = count
	args.Data.Attributes.ExpirationDate = expiresAt.UTC().Format("2006-01-02")
	args.Data.Relationships.OfferCode.Data.Type = "subscriptionOfferCodes"
	args.Data.Relationships.OfferCode.Data.Id = offerCodeId

	status, created, err := server.HttpPostWithStatus[ascOneTimeUseCodesCreateResult](
		ctx,
		strings.TrimRight(appStoreConnectBaseUrl, "/")+"/v1/subscriptionOfferCodeOneTimeUseCodes",
		args,
		ascHeader(token),
		brevoResponseJsonObject[ascOneTimeUseCodesCreateResult],
	)
	if err != nil {
		return nil, fmt.Errorf("create one-time codes: %w", err)
	}
	if status == nil || status.Code < 200 || 300 <= status.Code {
		detail := ""
		if 0 < len(created.Errors) {
			detail = created.Errors[0].Code + " " + created.Errors[0].Detail
		}
		return nil, fmt.Errorf("create one-time codes: %s %s", status.Status, detail)
	}
	if created.Data.Id == "" {
		return nil, fmt.Errorf("create one-time codes: no batch id")
	}

	// the values: GET .../{id}/values answers text/csv (one code per line, a
	// header row) — a plain request, not the JSON helper
	request, err := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(appStoreConnectBaseUrl, "/")+"/v1/subscriptionOfferCodeOneTimeUseCodes/"+created.Data.Id+"/values", nil)
	if err != nil {
		return nil, err
	}
	request.Header.Add("Authorization", "Bearer "+token)
	request.Header.Add("Accept", "text/csv")
	response, err := server.DefaultHttpClient().Do(request)
	if err != nil {
		return nil, fmt.Errorf("download one-time codes: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || 300 <= response.StatusCode {
		return nil, fmt.Errorf("download one-time codes: %s", response.Status)
	}
	codes := parseAppleOfferCodeValues(string(body), expiresAt)
	if len(codes) == 0 {
		return nil, fmt.Errorf("download one-time codes: batch %s has no values", created.Data.Id)
	}
	return codes, nil
}

// parseAppleOfferCodeValues reads the values download: one code per line,
// optionally a header row and a `code,expires` second column.
func parseAppleOfferCodeValues(data string, expiresAt time.Time) []*model.AppleOfferCode {
	var codes []*model.AppleOfferCode
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(strings.TrimRight(line, "\r"))
		if line == "" {
			continue
		}
		code := strings.TrimSpace(strings.Split(line, ",")[0])
		if code == "" || strings.EqualFold(code, "code") || strings.EqualFold(code, "offer code") {
			continue
		}
		codes = append(codes, &model.AppleOfferCode{Code: code, ExpiresAt: expiresAt})
	}
	return codes
}

// ----- the daily task -----

type AppleOfferCodeTopUpArgs struct {
}

type AppleOfferCodeTopUpResult struct {
	Available int    `json:"available"`
	Added     int    `json:"added"`
	Reason    string `json:"reason,omitempty"`
}

func ScheduleAppleOfferCodeTopUp(clientSession *session.ClientSession, tx server.PgTx, at time.Time) {
	task.ScheduleTaskInTx(
		tx,
		AppleOfferCodeTopUp,
		&AppleOfferCodeTopUpArgs{},
		clientSession,
		task.RunOnce("onboarding_apple_offer_code_top_up"),
		task.RunAt(at),
	)
}

// AppleOfferCodeTopUp keeps the pool above the floor. Refuses to call Apple
// while the engine is disabled (the codes would expire unused).
func AppleOfferCodeTopUp(
	args *AppleOfferCodeTopUpArgs,
	clientSession *session.ClientSession,
) (*AppleOfferCodeTopUpResult, error) {
	ctx := clientSession.Ctx
	now := server.NowUtc()
	available := model.CountAvailableAppleOfferCodes(ctx, now)
	onboardingAppleOfferCodesAvailable.Set(float64(available))
	result := &AppleOfferCodeTopUpResult{Available: available}

	if !OnboardingCampaignEnabled() {
		result.Reason = "onboarding campaign is disabled"
		onboardingAppleOfferCodeBatchesTotal.WithLabelValues("skipped").Inc()
		return result, nil
	}
	creds := appStoreConnectCredentialsFunc()
	decision := DecideAppleOfferCodeTopUp(model.Onboarding().Offer.Apple, creds != nil, available, now)
	if !decision.Generate {
		result.Reason = decision.Reason
		onboardingAppleOfferCodeBatchesTotal.WithLabelValues("skipped").Inc()
		glog.Warningf("[onboarding]apple offer code top-up skipped: %s (the custom offer code stays the App Store path)\n", decision.Reason)
		return result, nil
	}
	codes, err := generateAppleOfferCodeBatch(ctx, creds, model.Onboarding().Offer.Apple.OfferCodeId, decision.BatchSize, decision.ExpiresAt)
	if err != nil {
		onboardingAppleOfferCodeBatchesTotal.WithLabelValues("failed").Inc()
		return nil, err
	}
	added, err := model.LoadAppleOfferCodePool(ctx, codes)
	if err != nil {
		onboardingAppleOfferCodeBatchesTotal.WithLabelValues("failed").Inc()
		return nil, err
	}
	onboardingAppleOfferCodeBatchesTotal.WithLabelValues("created").Inc()
	result.Added = added
	onboardingAppleOfferCodesAvailable.Set(float64(model.CountAvailableAppleOfferCodes(ctx, now)))
	glog.Infof("[onboarding]apple offer code top-up: %d codes in batch, %d new, expiring %s\n", len(codes), added, decision.ExpiresAt.Format("2006-01-02"))
	return result, nil
}

func AppleOfferCodeTopUpPost(
	args *AppleOfferCodeTopUpArgs,
	result *AppleOfferCodeTopUpResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleAppleOfferCodeTopUp(clientSession, tx, server.NowUtc().Add(24*time.Hour))
	return nil
}
