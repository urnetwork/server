package controller

// Alchemy's webhooks for the crypto payment rail (mmm/BTCPAY.md §17.3):
//
//	POST /pay/alchemy   every Alchemy webhook (https://api.bringyour.com/pay/alchemy)
//
// The ETH Address Activity webhook is a fast path only. A delivery is verified
// with its own webhook's signing key (Alchemy's X-Alchemy-Signature: the hex
// HMAC-SHA256 of the raw body), routed by its webhook id and network, and
// answered 2xx at once: the invoice addresses it names are read on the next
// ETH scan, whatever their age, and that read of the chain credits
// (crypto_pay_watch.go). Polling is the source of truth, so ETH's availability
// does not depend on the webhook.
//
// Bitcoin has no webhook (mmm/BTCPAY.md §16.7): Alchemy documents no Bitcoin
// Address Activity webhook, its Bitcoin address notifications are the UTXO
// API's WebSocket, an add-on the owner did not buy (2026-11-11), and the
// Bitcoin scanner reads every block anyway, so a hint would bring nothing
// forward.
//
// The ETH webhook watches every address the pool takes from Coinbase: the
// refill adds each one through Alchemy's Notify API when alchemy.yml names
// the webhook and notify_auth_token (ethereumWallet.RegisterAddresses). The
// webhook itself is made once, by the owner at deploy time:
//
//	bringyourctl crypto alchemy-webhook [--signing-key-file=<path>]
//
// (CryptoAlchemyEnsureEthWebhook), which finds the webhook for that url before
// it makes one, prints its id, and writes its signing key to the operator's
// file, never to the output. Neither the server nor a test calls Alchemy's
// dashboard for it on its own.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"

	"github.com/ethereum/go-ethereum/common"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// The url every Alchemy webhook delivers to.
const cryptoAlchemyWebhookUrl = "https://api.bringyour.com/pay/alchemy"

// Alchemy's network name of the ETH mainnet webhook.
const cryptoAlchemyEthNetwork = "ETH_MAINNET"

// A delivery is read up to this many bytes.
const cryptoAlchemyMaxBodyBytes = 4 * 1024 * 1024

// The Notify API, the dashboard's webhook management. Replaceable only by
// tests.
var cryptoAlchemyDashboardUrl = "https://dashboard.alchemy.com/api"

// The most addresses one Notify update takes.
const cryptoAlchemyNotifyBatch = 500

// cryptoAlchemyWebhook is one configured Alchemy webhook.
type cryptoAlchemyWebhook struct {
	asset      string
	id         string
	signingKey string
}

// The configured webhooks, from vault alchemy.yml (read on each delivery): the
// ETH one. Replaceable only by tests.
var cryptoAlchemyWebhooks = func() []*cryptoAlchemyWebhook {
	webhooks := []*cryptoAlchemyWebhook{}
	settings, err := cryptoWalletSettingsFunc(model.CryptoAssetEth)
	if err == nil && settings.AlchemyWebhookId != "" && settings.AlchemyWebhookSigningKey != "" {
		webhooks = append(webhooks, &cryptoAlchemyWebhook{
			asset:      model.CryptoAssetEth,
			id:         settings.AlchemyWebhookId,
			signingKey: settings.AlchemyWebhookSigningKey,
		})
	}
	return webhooks
}

// AlchemyWebhookVerifiable is nil when a webhook's signing key is configured;
// without one every delivery is refused (503) before its body is read.
func AlchemyWebhookVerifiable() error {
	if len(cryptoAlchemyWebhooks()) == 0 {
		return errors.New("alchemy.yml names no webhook id with its signing key")
	}
	return nil
}

// cryptoAlchemySignatureValid checks Alchemy's signature: the hex HMAC-SHA256
// of the raw body with the webhook's signing key, compared in constant time.
func cryptoAlchemySignatureValid(body []byte, signature string, signingKey string) bool {
	mac := hmac.New(sha256.New, []byte(signingKey))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(strings.ToLower(strings.TrimSpace(signature))))
}

// VerifyAlchemyBody refuses a delivery whose webhook id is not configured or
// whose signature does not match that webhook's own signing key, before
// anything of it is read further. Fails closed.
func VerifyAlchemyBody(_ *session.ClientSession, req *http.Request) (io.Reader, error) {
	body, err := io.ReadAll(io.LimitReader(req.Body, cryptoAlchemyMaxBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if cryptoAlchemyMaxBodyBytes < len(body) {
		return nil, errors.New("The delivery is too large.")
	}
	var envelope struct {
		WebhookId string `json:"webhookId"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.WebhookId == "" {
		cryptoPayWebhookDeliveries.WithLabelValues("alchemy", "", "refused").Inc()
		return nil, errors.New("Invalid delivery.")
	}
	for _, webhook := range cryptoAlchemyWebhooks() {
		if webhook.id != envelope.WebhookId {
			continue
		}
		if !cryptoAlchemySignatureValid(body, req.Header.Get("X-Alchemy-Signature"), webhook.signingKey) {
			cryptoPayWebhookDeliveries.WithLabelValues("alchemy", webhook.asset, "refused").Inc()
			glog.Infof("[alchemy]refused a delivery with a bad signature\n")
			return nil, errors.New("Invalid signature.")
		}
		return bytes.NewReader(body), nil
	}
	cryptoPayWebhookDeliveries.WithLabelValues("alchemy", "", "refused").Inc()
	glog.Infof("[alchemy]refused a delivery for a webhook that is not configured\n")
	return nil, errors.New("Invalid delivery.")
}

// AlchemyWebhookArgs is an Address Activity delivery (the fields read here).
type AlchemyWebhookArgs struct {
	WebhookId string `json:"webhookId"`
	Id        string `json:"id"`
	Type      string `json:"type"`
	Event     struct {
		Network  string `json:"network"`
		Activity []struct {
			FromAddress string `json:"fromAddress"`
			ToAddress   string `json:"toAddress"`
			Hash        string `json:"hash"`
			Category    string `json:"category"`
			Asset       string `json:"asset"`
		} `json:"activity"`
	} `json:"event"`
}

type AlchemyWebhookResult struct {
	Message string `json:"message,omitempty"`
}

// AlchemyWebhook reads a verified delivery: the invoice addresses it names as
// recipients are read on the coin's next scan. It writes nothing else, and
// answers at once.
func AlchemyWebhook(args *AlchemyWebhookArgs, clientSession *session.ClientSession) (*AlchemyWebhookResult, error) {
	var webhook *cryptoAlchemyWebhook
	for _, configured := range cryptoAlchemyWebhooks() {
		if configured.id == args.WebhookId {
			webhook = configured
		}
	}
	if webhook == nil {
		return nil, errors.New("Invalid delivery.")
	}
	if webhook.asset != model.CryptoAssetEth || args.Event.Network != cryptoAlchemyEthNetwork {
		cryptoPayWebhookDeliveries.WithLabelValues("alchemy", webhook.asset, "ignored").Inc()
		return &AlchemyWebhookResult{Message: "Not an Ethereum mainnet delivery"}, nil
	}
	addresses := []string{}
	for _, activity := range args.Event.Activity {
		if activity.Category != "external" && activity.Category != "internal" {
			// a token transfer pays no ETH invoice
			continue
		}
		address, err := cryptoEthereumAddressCanonical(activity.ToAddress)
		if err == nil && !slices.Contains(addresses, address) {
			addresses = append(addresses, address)
		}
	}
	invoiceAddresses := cryptoInvoiceAddresses(clientSession.Ctx, webhook.asset, addresses)
	if len(invoiceAddresses) == 0 {
		cryptoPayWebhookDeliveries.WithLabelValues("alchemy", webhook.asset, "ignored").Inc()
		return &AlchemyWebhookResult{Message: "No invoice address"}, nil
	}
	cryptoAddScanHints(clientSession.Ctx, webhook.asset, invoiceAddresses)
	cryptoPayWebhookDeliveries.WithLabelValues("alchemy", webhook.asset, "hinted").Inc()
	return &AlchemyWebhookResult{Message: fmt.Sprintf("%d invoice addresses to read", len(invoiceAddresses))}, nil
}

// ----- the Notify API -----

// The new addresses are registered only when alchemy.yml names the webhook
// and the Notify API token: polling reads them either way.
var errCryptoAlchemyRegistrationOff = errors.New("alchemy.yml names no webhook id and notify_auth_token: new addresses are not registered")

// cryptoAlchemyNotify calls the Notify API with the team's token. Errors name
// the path and the status, never the token.
func cryptoAlchemyNotify(ctx context.Context, token string, method string, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	requestCtx, cancel := context.WithTimeout(ctx, cryptoProviderTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, method, cryptoAlchemyDashboardUrl+path, reader)
	if err != nil {
		return errors.New("alchemy notify: invalid url")
	}
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set("X-Alchemy-Token", token)
	response, err := cryptoProviderClient().Do(request)
	if err != nil {
		return errors.New("alchemy notify: transport failed")
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, cryptoProviderMaxResponseBytes))
	if err != nil {
		return errors.New("alchemy notify: read failed")
	}
	if response.StatusCode < 200 || 300 <= response.StatusCode {
		return fmt.Errorf("alchemy notify %s: http %d", path, response.StatusCode)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(responseBody, out); err != nil {
		return fmt.Errorf("alchemy notify %s: undecodable answer", path)
	}
	return nil
}

// cryptoAlchemyRegisterAddresses adds addresses to an Address Activity
// webhook, at most 500 per request (the update is idempotent).
func cryptoAlchemyRegisterAddresses(ctx context.Context, webhookId string, token string, addresses []string) error {
	if webhookId == "" || token == "" {
		return errCryptoAlchemyRegistrationOff
	}
	for start := 0; start < len(addresses); start += cryptoAlchemyNotifyBatch {
		end := min(start+cryptoAlchemyNotifyBatch, len(addresses))
		if err := cryptoAlchemyNotify(ctx, token, http.MethodPatch, "/update-webhook-addresses", map[string]any{
			"webhook_id":          webhookId,
			"addresses_to_add":    addresses[start:end],
			"addresses_to_remove": []string{},
		}, nil); err != nil {
			return err
		}
	}
	return nil
}

// ----- the deploy-time setup of the ETH webhook -----

// CryptoAlchemyWebhookResult is what the setup step found or made.
type CryptoAlchemyWebhookResult struct {
	WebhookId  string
	WebhookUrl string
	Created    bool
	// the signing key was written to the operator's file (never printed)
	SigningKeyWritten bool
}

type cryptoAlchemyTeamWebhook struct {
	Id          string `json:"id"`
	Network     string `json:"network"`
	WebhookType string `json:"webhook_type"`
	WebhookUrl  string `json:"webhook_url"`
	IsActive    bool   `json:"is_active"`
	SigningKey  string `json:"signing_key"`
}

// The vault's alchemy.yml app id and Notify token. Replaceable only by tests.
var cryptoAlchemySetupSettings = func() (appId string, token string, err error) {
	resource, err := server.Vault.SimpleResource("alchemy.yml")
	if err != nil {
		return "", "", err
	}
	appId, err = simpleResourceNonblankStringValue(resource, "alchemy", "app_id")
	if err != nil {
		return "", "", fmt.Errorf("alchemy.yml: %w", err)
	}
	token, err = simpleResourceNonblankStringValue(resource, "alchemy", "notify_auth_token")
	if err != nil {
		return "", "", fmt.Errorf("alchemy.yml: %w", err)
	}
	return strings.TrimSpace(appId), strings.TrimSpace(token), nil
}

// cryptoAlchemyPlaceholderAddress is a fresh random ETH address no one holds:
// Alchemy makes an Address Activity webhook with at least one address, and
// the pool's own are added after.
func cryptoAlchemyPlaceholderAddress() string {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return common.BytesToAddress(b).Hex()
}

// CryptoAlchemyEnsureEthWebhook finds the ETH Address Activity webhook that
// delivers to https://api.bringyour.com/pay/alchemy, or makes it, with the
// app id and the Notify token of alchemy.yml. When Alchemy returns the
// webhook's signing key and signingKeyPath is given, the key is written there
// (mode 0600, never over an existing file) and nowhere else. Its first run is
// an owner step at deploy time; nothing calls it on its own.
func CryptoAlchemyEnsureEthWebhook(ctx context.Context, signingKeyPath string) (*CryptoAlchemyWebhookResult, error) {
	appId, token, err := cryptoAlchemySetupSettings()
	if err != nil {
		return nil, err
	}
	result := &CryptoAlchemyWebhookResult{WebhookUrl: cryptoAlchemyWebhookUrl}
	var listed struct {
		Data []*cryptoAlchemyTeamWebhook `json:"data"`
	}
	if err := cryptoAlchemyNotify(ctx, token, http.MethodGet, "/team-webhooks", nil, &listed); err != nil {
		return nil, err
	}
	var webhook *cryptoAlchemyTeamWebhook
	for _, candidate := range listed.Data {
		if candidate != nil && candidate.WebhookType == "ADDRESS_ACTIVITY" &&
			candidate.Network == cryptoAlchemyEthNetwork && candidate.WebhookUrl == cryptoAlchemyWebhookUrl {
			webhook = candidate
			break
		}
	}
	if webhook == nil {
		var created struct {
			Data *cryptoAlchemyTeamWebhook `json:"data"`
		}
		if err := cryptoAlchemyNotify(ctx, token, http.MethodPost, "/create-webhook", map[string]any{
			"network":      cryptoAlchemyEthNetwork,
			"webhook_type": "ADDRESS_ACTIVITY",
			"webhook_url":  cryptoAlchemyWebhookUrl,
			"app_id":       appId,
			"name":         "URnetwork ETH invoices",
			"addresses":    []string{cryptoAlchemyPlaceholderAddress()},
		}, &created); err != nil {
			return nil, err
		}
		if created.Data == nil || created.Data.Id == "" {
			return nil, errors.New("alchemy answered no webhook id")
		}
		webhook = created.Data
		result.Created = true
	}
	result.WebhookId = webhook.Id
	if webhook.SigningKey != "" && signingKeyPath != "" {
		file, err := os.OpenFile(signingKeyPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return nil, fmt.Errorf("could not write the signing key file (it must not exist yet): %w", err)
		}
		_, writeErr := file.WriteString(webhook.SigningKey + "\n")
		closeErr := file.Close()
		if writeErr != nil || closeErr != nil {
			return nil, errors.New("could not write the signing key file")
		}
		result.SigningKeyWritten = true
	}
	return result, nil
}
