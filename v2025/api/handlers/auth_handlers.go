package handlers

import (
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/golang/glog"

	gojwt "github.com/golang-jwt/jwt/v5"
	"github.com/urnetwork/server/v2025/controller"
	"github.com/urnetwork/server/v2025/model"
	"github.com/urnetwork/server/v2025/router"
)

func AuthLogin(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputNoAuth(controller.AuthLogin, w, r)
}

func AuthLoginWithPassword(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputNoAuth(controller.AuthLoginWithPassword, w, r)
}

func AuthVerify(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputNoAuth(controller.AuthVerify, w, r)
}

func AuthVerifySend(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputNoAuth(controller.AuthVerifySend, w, r)
}

func AuthPasswordReset(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputNoAuth(controller.AuthPasswordReset, w, r)
}

func AuthPasswordSet(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputNoAuth(controller.AuthPasswordSet, w, r)
}

func AuthNetworkCheck(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputNoAuth(model.NetworkCheck, w, r)
}

func AuthNetworkCreate(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputNoAuth(controller.NetworkCreate, w, r)
}

func AuthCodeCreate(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(model.AuthCodeCreate, w, r)
}

func AuthCodeLogin(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputNoAuth(model.AuthCodeLogin, w, r)
}

func AuthConnect(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, controller.SsoRedirectUrl(), http.StatusSeeOther)
}

/**
 * Apple webhook handling
 */

func AppleNotification(w http.ResponseWriter, r *http.Request) {

	bodyBytes, err := io.ReadAll(r.Body)
	defer r.Body.Close()
	if err != nil {
		glog.Errorf("[apple] Failed to read request body: %v", err)
		http.Error(w, "Could not read notification", http.StatusInternalServerError)
		return
	}

	// Parse the payload
	var payload controller.AppleNotificationPayload
	err = json.Unmarshal(bodyBytes, &payload)
	if err != nil {
		glog.Errorf("[apple] Failed to unmarshal payload: %v", err)
		http.Error(w, "Could not parse notification payload", http.StatusBadRequest)
		return
	}

	parts := strings.Split(payload.SignedPayload, ".")
	if len(parts) != 3 {
		glog.Errorf("[apple] Invalid JWT format")
		http.Error(w, "Invalid payload format", http.StatusBadRequest)
		return
	}

	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		glog.Errorf("[apple] Failed to decode JWT header: %v", err)
		http.Error(w, "Invalid header encoding", http.StatusBadRequest)
		return
	}

	var header map[string]interface{}
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		glog.Errorf("[apple] Failed to parse JWT header: %v", err)
		http.Error(w, "Invalid header format", http.StatusBadRequest)
		return
	}

	// Check if we have x5c certificates in the header
	x5c, ok := header["x5c"].([]interface{})
	if !ok {
		glog.Errorf("[apple] JWT header missing 'x5c' certificates")
		http.Error(w, "Invalid JWT", http.StatusBadRequest)
		return
	}

	// Extract the first certificate from the chain (which should be the signing certificate)
	if len(x5c) == 0 {
		glog.Errorf("[apple] JWT x5c chain is empty")
		http.Error(w, "Invalid JWT", http.StatusBadRequest)
		return
	}

	certB64, ok := x5c[0].(string)
	if !ok {
		glog.Errorf("[apple] Invalid x5c certificate format")
		http.Error(w, "Invalid JWT", http.StatusBadRequest)
		return
	}

	// Decode the base64-encoded certificate
	certDER, err := base64.StdEncoding.DecodeString(certB64)
	if err != nil {
		glog.Errorf("[apple] Failed to decode x5c certificate: %v", err)
		http.Error(w, "Invalid JWT", http.StatusBadRequest)
		return
	}

	// Parse the certificate
	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		glog.Errorf("[apple] Failed to parse x5c certificate: %v", err)
		http.Error(w, "Invalid JWT", http.StatusBadRequest)
		return
	}

	publicKey := cert.PublicKey

	// Parse and verify the JWT
	token, err := gojwt.Parse(payload.SignedPayload, func(token *gojwt.Token) (interface{}, error) {

		alg, ok := header["alg"].(string)
		if !ok {
			return nil, fmt.Errorf("missing algorithm in JWT header")
		}

		// Make sure the algorithm matches what's expected
		switch alg {
		case "RS256":
			// For RSA algorithms
			rsaKey, ok := publicKey.(*rsa.PublicKey)
			if !ok {
				return nil, fmt.Errorf("key is not an RSA key")
			}
			return rsaKey, nil
		case "ES256":
			// For ECDSA algorithms
			ecKey, ok := publicKey.(*ecdsa.PublicKey)
			if !ok {
				return nil, fmt.Errorf("key is not an ECDSA key")
			}
			return ecKey, nil
		default:
			return nil, fmt.Errorf("unsupported algorithm: %s", alg)
		}
	})

	if err != nil {
		glog.Errorf("[apple] Failed to parse JWT: %v", err)
		http.Error(w, "Invalid JWT signature", http.StatusBadRequest)
		return
	}

	if !token.Valid {
		glog.Error("[apple] Invalid JWT signature")
		http.Error(w, "Invalid JWT signature", http.StatusBadRequest)
		return
	}

	claims, ok := token.Claims.(gojwt.MapClaims)
	if !ok {
		glog.Error("[apple] Failed to extract claims")
		http.Error(w, "Invalid JWT claims", http.StatusBadRequest)
		return
	}

	// Parse the notification data
	var notification controller.AppleNotificationDecodedPayload
	notificationBytes, err := json.Marshal(claims)
	if err != nil {
		glog.Errorf("[apple] Failed to marshal JWT payload: %v", err)
		http.Error(w, "Invalid payload format", http.StatusBadRequest)
		return
	}

	if err := json.Unmarshal(notificationBytes, &notification); err != nil {
		glog.Errorf("[apple] Failed to parse notification data: %v", err)
		http.Error(w, "Invalid notification format", http.StatusBadRequest)
		return
	}

	// Process the notification based on its type
	glog.Infoln("[apple] Notification type: %s, subtype: %s, UUID: %s",
		notification.NotificationType,
		notification.Subtype,
		notification.NotificationUUID)

	// Process different notification types
	switch notification.NotificationType {
	case "SUBSCRIBED":
		controller.HandleSubscribedApple(r.Context(), notification)
	case "EXPIRED":
		controller.HandleExpiredApple(notification)
	case "DID_RENEW":
		controller.HandleRenewalApple(r.Context(), notification)
	default:
		glog.Infof("[apple] Unhandled notification type: %s", notification.NotificationType)
	}

	// Send a successful response
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("{}"))
}
