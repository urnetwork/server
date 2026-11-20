package handlers

// api's end of the role-mail intake's notices (QD-1070, owner decision 612; §21.8, §21.16): the route the platform
// listener's role-mail intake posts a role mail's header facts to, under server.InternalGatePathPrefix, taking only
// the platform listener's class's bearer (server.InternalGateCallerMxedgePlatform), verified by its hash in constant
// time (server.InternalGateVerifier), and answering every other request 403 before anything is read: no bearer,
// one api does not hold, another class's, a URnetwork client's token or a staff member's.
//
//	POST /internal/gate/abuse-notice  mxedge-platform  {recipient_local_part, root, signatures: [{domain, result}]}
//	  -> {notice, paged}
//
// The body is the route's format (server.AbuseNoticeGateArgs) and nothing more: a field beyond it, a body, a
// subject or a From among them, or a second value after it, is refused 400 with nothing filed, so no mail text
// reaches api this way. controller.FileAbuseNoticeOfMailVerdicts matches the verdicts to abuse.yml's notice_senders
// and files the notice about the root, paging both rota members under the case's key from this process; a page
// the pager did not take answers paged false, the case standing for taskworker's clock pass to page. A mail that is
// no notice answers notice false and opens nothing. 503 with Retry-After is a mail api cannot judge or file now: a
// listed sender's key that did not answer, a store that did not answer or a binary without MG5. The listener
// answers it 451 so the sender sends the mail again, and no notice is lost to a moment's outage.
//
// Nothing here logs a bearer, a domain, a root or a local part; a refusal's log line names the part refused. Safe
// for concurrent use.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
)

// The route by the name its refusals count under.
const internalGateRouteAbuseNotice = "abuse-notice"

// Tunables of the route.
type InternalGateAbuseNoticeSettings struct {
	// The most bytes a request's body may hold: 4 KiB, a local part, a root and eight signatures many times over.
	RequestMaxByteCount int64
	// What a 503 asks the listener to wait, as Retry-After in whole seconds: 60 s. The listener answers 451 and the
	// sender's MTA keeps its own schedule, so this is a hint.
	UnavailableRetryAfter time.Duration
	// The route's format, which the listener's client carries too. nil: server.DefaultAbuseNoticeGateFormatSettings.
	Format *server.AbuseNoticeGateFormatSettings
}

func DefaultInternalGateAbuseNoticeSettings() *InternalGateAbuseNoticeSettings {
	return &InternalGateAbuseNoticeSettings{
		RequestMaxByteCount:   4 * 1024,
		UnavailableRetryAfter: 60 * time.Second,
		Format:                server.DefaultAbuseNoticeGateFormatSettings(),
	}
}

// What the handlers run on. Each nil takes api's own.
type InternalGateAbuseNoticeDeps struct {
	// The bearers' digests. nil: server.LoadInternalGateVerifier from api's vault, read once; a vault without the
	// file refuses every call 403.
	Verifier *server.InternalGateVerifier
	// Files one role mail's notice. nil: controller.FileAbuseNoticeOfMailVerdicts.
	File func(ctx context.Context, recipientLocalPart string, root string, verdicts []controller.AbuseNoticeMailVerdict) (*controller.AbuseNoticeResult, error)
	// nil: the defaults.
	Settings *InternalGateAbuseNoticeSettings
}

// See the file header.
type InternalGateAbuseNoticeHandlers struct {
	verifier func() *server.InternalGateVerifier
	file     func(ctx context.Context, recipientLocalPart string, root string, verdicts []controller.AbuseNoticeMailVerdict) (*controller.AbuseNoticeResult, error)
	settings *InternalGateAbuseNoticeSettings
	format   *server.AbuseNoticeGateFormatSettings
}

// The handlers on deps, nil taking api's own.
func NewInternalGateAbuseNoticeHandlers(deps *InternalGateAbuseNoticeDeps) *InternalGateAbuseNoticeHandlers {
	if deps == nil {
		deps = &InternalGateAbuseNoticeDeps{}
	}
	verifier := func() *server.InternalGateVerifier {
		return deps.Verifier
	}
	if deps.Verifier == nil {
		verifier = sync.OnceValue(func() *server.InternalGateVerifier {
			loaded, err := server.LoadInternalGateVerifier()
			if err != nil {
				// the error names the file and the part, never a bearer
				glog.Errorf("[internalgate]every role mail's notice is refused: %v\n", err)
				return nil
			}
			return loaded
		})
	}
	file := deps.File
	if file == nil {
		file = controller.FileAbuseNoticeOfMailVerdicts
	}
	settings := deps.Settings
	if settings == nil {
		settings = DefaultInternalGateAbuseNoticeSettings()
	}
	format := settings.Format
	if format == nil {
		format = server.DefaultAbuseNoticeGateFormatSettings()
	}
	return &InternalGateAbuseNoticeHandlers{
		verifier: verifier,
		file:     file,
		settings: settings,
		format:   format,
	}
}

// The one route, its pattern the client's path (server.AbuseNoticeGatePath), so both ends name one route.
func (self *InternalGateAbuseNoticeHandlers) Routes() []*router.Route {
	return []*router.Route{
		router.NewRoute("POST", server.AbuseNoticeGatePath, self.PostAbuseNotice),
	}
}

// Whether the request carries the platform listener's bearer; otherwise it is answered 403 and counted, before
// anything is read.
func (self *InternalGateAbuseNoticeHandlers) authenticate(w http.ResponseWriter, r *http.Request) bool {
	refuse := func(refusal string) bool {
		quotaServiceRefusalCounter.WithLabelValues(internalGateRouteAbuseNotice, refusal).Inc()
		http.Error(w, "Forbidden.", http.StatusForbidden)
		return false
	}
	verifier := self.verifier()
	if verifier == nil {
		return refuse("no_verify_file")
	}
	caller, ok := verifier.Authenticate(r)
	if !ok {
		return refuse("credential")
	}
	if caller != server.InternalGateCallerMxedgePlatform {
		if glog.V(1) {
			glog.Infof("[internalgate]%s's bearer refused on %s\n", caller, internalGateRouteAbuseNotice)
		}
		return refuse("class")
	}
	return true
}

// Refuses a request the route's format does not take, saying which part in the log alone.
func (self *InternalGateAbuseNoticeHandlers) invalid(w http.ResponseWriter, reason string) {
	if glog.V(1) {
		glog.Infof("[internalgate]%s refused: %s\n", internalGateRouteAbuseNotice, reason)
	}
	http.Error(w, "Invalid request.", http.StatusBadRequest)
}

// Answers 503 with UnavailableRetryAfter: the mail is judged again when its sender sends it again.
func (self *InternalGateAbuseNoticeHandlers) unavailable(w http.ResponseWriter, reason string) {
	glog.Infof("[internalgate]a role mail's notice was not judged now, so its sender is asked to send it again: %s\n", reason)
	w.Header().Set("Retry-After", strconv.Itoa(int(self.settings.UnavailableRetryAfter/time.Second)))
	http.Error(w, "Unavailable.", http.StatusServiceUnavailable)
}

// POST /internal/gate/abuse-notice: one role mail's notice, filed or not.
func (self *InternalGateAbuseNoticeHandlers) PostAbuseNotice(w http.ResponseWriter, r *http.Request) {
	if !self.authenticate(w, r) {
		return
	}
	args := &server.AbuseNoticeGateArgs{}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, self.settings.RequestMaxByteCount))
	// a field beyond the format's, a body or a subject among them, is refused, not ignored
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(args); err != nil {
		self.invalid(w, "a body that is not the route's format")
		return
	}
	if err := decoder.Decode(&json.RawMessage{}); err != io.EOF {
		self.invalid(w, "a second value after the request")
		return
	}
	if err := args.Validate(self.format); err != nil {
		// the error names the part, never its value
		self.invalid(w, err.Error())
		return
	}
	verdicts := []controller.AbuseNoticeMailVerdict{}
	for _, signature := range args.Signatures {
		verdicts = append(verdicts, controller.AbuseNoticeMailVerdict{Domain: signature.Domain, Result: signature.Result})
	}
	result, err := self.file(r.Context(), args.RecipientLocalPart, args.Root, verdicts)
	switch {
	case result != nil:
		if err != nil && glog.V(1) {
			// the pager's own error, which names no case
			glog.Infof("[internalgate]a role mail's notice is filed and its page owed: %v\n", err)
		}
	case err == nil:
	case errors.Is(err, model.ErrAbuseCaseInvalid):
		self.invalid(w, "a root that is no host name")
		return
	case errors.Is(err, controller.ErrAbuseNoticeMailDeferred):
		self.unavailable(w, "a listed sender's key did not answer")
		return
	case errors.Is(err, model.ErrAbuseUnlinked):
		self.unavailable(w, "this binary does not link MG5")
		return
	default:
		self.unavailable(w, "the store did not answer")
		return
	}
	answer := &server.AbuseNoticeGateResult{
		Notice: result != nil,
		Paged:  result != nil && result.Paged,
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(answer); err != nil {
		glog.Infof("[internalgate]could not write a role mail's notice answer\n")
	}
}
