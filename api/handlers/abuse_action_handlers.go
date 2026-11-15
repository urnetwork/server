package handlers

// POST /abuse/action on api (ABUSE-M5-12; D-ABUSE-5, D-ABUSE-7, R-ABUSE-6, I-6, I-13; QD-126): §21.16's staff
// route, x-internal, Public in api/route_authz.go's table since it takes a credential of its own kind and no
// URnetwork token. A request carries `Authorization: Bearer <staff bearer>` and a JSON body whose `kind` picks the
// verb (controller.AbuseStaffAction's table). The bearer is checked against vault's abuse-staff.yml before the body
// is read, and every failure, a missing or broken file included, answers one 403 "Forbidden.", so a refusal says
// nothing of which part failed. The file is read again every VerifierRereadInterval, so a removal or a rotation lands
// without a restart, and a read that fails refuses every caller until one succeeds.
//
// Answers carry Cache-Control: no-store. Nothing here logs a bearer, a network id, a target or a reason; a refusal's
// log line names the part refused. The lb's rule keeping the route to the operators' addresses is warp's.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/sidecar/dnsservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// Tunables of the staff route.
type AbuseActionRouteSettings struct {
	// The most bytes of a request's body: 64 KiB, any verb's arguments many times over.
	RequestMaxByteCount int64
	// How long one read of abuse-staff.yml answers before the next: 60 s, §19.11's reread, so a removed credential
	// stops within a minute.
	VerifierRereadInterval time.Duration
	// The credential's own. nil: controller.DefaultAbuseStaffSettings.
	Staff *controller.AbuseStaffSettings
}

func DefaultAbuseActionRouteSettings() *AbuseActionRouteSettings {
	return &AbuseActionRouteSettings{
		RequestMaxByteCount:    64 * 1024,
		VerifierRereadInterval: 60 * time.Second,
		Staff:                  controller.DefaultAbuseStaffSettings(),
	}
}

// What the route runs on. Each nil takes api's own.
type AbuseActionHandlersDeps struct {
	// Reads abuse-staff.yml. nil: controller.LoadAbuseStaffVerifier.
	LoadVerifier func() (*controller.AbuseStaffVerifier, error)
	// The clock. nil: server.NowUtc.
	Now func() time.Time
	// nil: the defaults.
	Settings *AbuseActionRouteSettings
}

// The staff route, the file header's. Safe for concurrent use.
type AbuseActionHandlers struct {
	loadVerifier func() (*controller.AbuseStaffVerifier, error)
	now          func() time.Time
	settings     *AbuseActionRouteSettings

	stateLock sync.Mutex
	// the last read of the file and when the next is due; nil after a read that failed
	verifier       *controller.AbuseStaffVerifier
	verifierRead   bool
	verifierReadAt time.Time
}

// The route on deps, nil taking api's own.
func NewAbuseActionHandlers(deps *AbuseActionHandlersDeps) *AbuseActionHandlers {
	if deps == nil {
		deps = &AbuseActionHandlersDeps{}
	}
	self := &AbuseActionHandlers{
		loadVerifier: deps.LoadVerifier,
		now:          deps.Now,
		settings:     deps.Settings,
	}
	if self.loadVerifier == nil {
		self.loadVerifier = controller.LoadAbuseStaffVerifier
	}
	if self.now == nil {
		self.now = server.NowUtc
	}
	if self.settings == nil {
		self.settings = DefaultAbuseActionRouteSettings()
	}
	if self.settings.Staff == nil {
		self.settings.Staff = controller.DefaultAbuseStaffSettings()
	}
	return self
}

// api's route, made on first use.
var defaultAbuseActionHandlers = sync.OnceValue(func() *AbuseActionHandlers {
	return NewAbuseActionHandlers(nil)
})

// POST /abuse/action on api's own routes.
func AbuseAction(w http.ResponseWriter, r *http.Request) {
	defaultAbuseActionHandlers().Action(w, r)
}

// The verify file as last read, read again once VerifierRereadInterval has passed; nil while the last read failed.
func (self *AbuseActionHandlers) currentVerifier() *controller.AbuseStaffVerifier {
	self.stateLock.Lock()
	now := self.now()
	if self.verifierRead && now.Before(self.verifierReadAt.Add(self.settings.VerifierRereadInterval)) {
		verifier := self.verifier
		self.stateLock.Unlock()
		return verifier
	}
	self.stateLock.Unlock()

	// the read is a vault file's, outside the lock
	verifier, err := self.loadVerifier()
	if err != nil {
		// the error names the file and the part, never a digest
		glog.Errorf("[abuse]every staff request is refused: %v\n", err)
		verifier = nil
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.verifier = verifier
	self.verifierRead = true
	self.verifierReadAt = now
	return verifier
}

// The staff member the request's bearer names, or false, having answered 403 and said in the log which part failed.
func (self *AbuseActionHandlers) authenticate(w http.ResponseWriter, r *http.Request) (*controller.AbuseStaff, bool) {
	refuse := func(part string) (*controller.AbuseStaff, bool) {
		if glog.V(1) {
			glog.Infof("[abuse]a staff request was refused: %s\n", part)
		}
		http.Error(w, "Forbidden.", http.StatusForbidden)
		return nil, false
	}
	verifier := self.currentVerifier()
	if verifier == nil {
		return refuse("no staff file")
	}
	bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || bearer == "" {
		return refuse("no bearer")
	}
	staff, ok := verifier.Authenticate(bearer, self.now(), self.settings.Staff)
	if !ok {
		return refuse("credential")
	}
	return staff, true
}

// The status and fixed message a verb's error answers. A refusal says which rule refused and never echoes a value.
func abuseActionErrorAnswer(err error) (int, string) {
	switch {
	case errors.Is(err, controller.ErrAbuseStaffKindUnknown):
		return http.StatusBadRequest, "Unknown kind."
	case errors.Is(err, controller.ErrAbuseStaffRoleRefused):
		return http.StatusForbidden, "Your role does not take this kind."
	case errors.Is(err, controller.ErrAbuseStaffRequestInvalid),
		errors.Is(err, model.ErrAbuseLookupInvalid),
		errors.Is(err, model.ErrAbuseReviewInvalid),
		errors.Is(err, model.ErrAbuseActionInvalid):
		return http.StatusBadRequest, "The request does not read as the kind's, or misses what it needs."
	case errors.Is(err, model.ErrAbuseTargetRefused):
		return http.StatusForbidden, "The target is a root or a platform name, or none is configured."
	case errors.Is(err, model.ErrAbuseCaseNotFound),
		errors.Is(err, model.ErrAbuseActionNotFound),
		errors.Is(err, model.ErrAbuseAppealNotFound),
		errors.Is(err, dnsservice.ErrDnsRootUnknown):
		return http.StatusNotFound, "Not found."
	case errors.Is(err, model.ErrAbuseActionLifted):
		return http.StatusConflict, "The action was lifted already."
	case errors.Is(err, model.ErrAbuseActionConflict):
		return http.StatusConflict, "The id names another action."
	case errors.Is(err, model.ErrAbuseReviewHasActions):
		return http.StatusConflict, "The case has actions, and each takes its own review."
	case errors.Is(err, model.ErrAbuseAppealDecided):
		return http.StatusConflict, "The appeal was decided the other way."
	case errors.Is(err, model.ErrAbuseAppealSameReviewer):
		return http.StatusConflict, "A reviewer of the case's actions cannot decide its appeal."
	case errors.Is(err, model.ErrAbuseAppealActionsPending):
		return http.StatusConflict, "An action of the case waits on its first review."
	}
	return http.StatusServiceUnavailable, "Unavailable."
}

// POST /abuse/action: the file header's staff route.
func (self *AbuseActionHandlers) Action(w http.ResponseWriter, r *http.Request) {
	staff, ok := self.authenticate(w, r)
	if !ok {
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, self.settings.RequestMaxByteCount))
	if err != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			http.Error(w, "The request body is too large.", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "The request body could not be read.", http.StatusBadRequest)
		return
	}

	var result *controller.AbuseStaffResult
	if r := server.HandleError(func() {
		result, err = controller.AbuseStaffAction(r.Context(), staff, body)
	}); r != nil {
		if server.IsDoneError(r) {
			panic(r)
		}
		err = fmt.Errorf("the store did not answer: %v", r)
	}
	if err != nil {
		status, message := abuseActionErrorAnswer(err)
		if status == http.StatusServiceUnavailable {
			// a store, a mailer or an effect; the cause names no network, target or reason
			glog.Infof("[abuse]a staff request failed: %s\n", err)
			w.Header().Set("Retry-After", "5")
		}
		http.Error(w, message, status)
		return
	}
	resultJson, err := json.Marshal(result)
	if err != nil {
		glog.Infof("[abuse]a staff answer could not be encoded: %s\n", err)
		http.Error(w, "Unavailable.", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(resultJson)
}
