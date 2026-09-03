package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
)

func AuthLogin(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputNoAuth(controller.AuthLogin, w, r)
}

func AuthWalletNonce(w http.ResponseWriter, r *http.Request) {
	router.WrapNoAuth(model.AuthWalletNonceCreate, w, r)
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

// AuthAppleOAuthCallback receives Apple's form_post for the apps that sign in
// with Apple through the system browser (android, windows, linux) and hands the
// result back to the app through its own scheme. No auth, no body wrapper: the
// request is a browser form, not an api call.
func AuthAppleOAuthCallback(w http.ResponseWriter, r *http.Request) {
	controller.AppleOAuthCallback(w, r)
}

func AuthConnect(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, controller.SsoRedirectUrl(), http.StatusSeeOther)
}

/**
 * Apple webhook handling
 */

func AppleNotification(w http.ResponseWriter, r *http.Request) {
	const maxAppleNotificationBytes int64 = 1024 * 1024
	if r.ContentLength > maxAppleNotificationBytes {
		http.Error(w, "Notification is too large", http.StatusRequestEntityTooLarge)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxAppleNotificationBytes)
	defer r.Body.Close()

	var payload controller.AppleNotificationPayload
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil || payload.SignedPayload == "" {
		glog.Infof("[apple] Rejected malformed notification envelope")
		http.Error(w, "Could not parse notification payload", http.StatusBadRequest)
		return
	}

	verifier := defaultAppleNotificationVerifier()
	notification, err := verifier.verifyNotification(payload.SignedPayload)
	if err != nil {
		glog.Infof("[apple] Rejected notification: %v", err)
		http.Error(w, "Invalid signed notification", http.StatusBadRequest)
		return
	}
	processed, err := controller.ProcessAppleNotification(r.Context(), *notification, verifier.config.ProductIds)
	if err != nil {
		glog.Infof("[apple] Rejected verified notification %s: %v", notification.NotificationUUID, err)
		http.Error(w, "Invalid notification claims", http.StatusBadRequest)
		return
	}
	glog.Infof(
		"[apple] Notification type=%s uuid=%s processed=%t",
		notification.NotificationType,
		notification.NotificationUUID,
		processed,
	)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("{}"))
}

func AuthRefreshToken(w http.ResponseWriter, r *http.Request) {
	// router.WrapWithInputNoAuth(controller.AuthRefreshToken, w, r)
	router.WrapRequireAuth(controller.RefreshToken, w, r)
}

func AuthAdd(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(controller.AddAuth, w, r)
}

func AuthRemove(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(controller.RemoveAuth, w, r)
}
