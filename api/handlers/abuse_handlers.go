package handlers

// §21.16's four customer routes on api (ABUSE-M5-11; D-ABUSE-5, R-ABUSE-6, I-6, I-13, D-TEAM-14), each in
// api/route_authz.go's table:
//
//	POST /abuse/report   Public           URLs and header blocks and a category, through controller.AcceptAbuseReport
//	GET  /abuse/cases    Network (read)   the caller's network's cases it was told of (model.ListNetworkAbuseCases)
//	POST /abuse/appeal   Network (admin)  an Owner's or Admin's appeal, never a client token (model.CreateAbuseAppeal)
//	GET  /abuse/status   Client           the caller's network's sidecar.AbuseStatus, on §7.4's strict_exp allowlist
//
// The report route answers the same body whether or not a report joined a URnetwork name, so it is no oracle for
// which names exist (plan-ABUSE-small-readings #7), and it reads no credential: a bearer grants a report nothing.
// Its one key is a trusted flagger's, in X-UR-Flagger-Key (ABUSE-M8-01, QD-111): a key whose digest matches an entry
// of vault's abuse-flaggers.yml under abuse.yml's list sets class trusted_flagger, so a URL the report names takes the
// machine's host kill; a wrong or absent key is a plain report, and the answer says neither.
// Each URL and each header block is its own report, since each names its own host or sender. A request counts once
// against its caller's keyed /29 or /56 address hash (server.ClientIpHashForAddr, peppered), never against its
// address, however many reports it holds (questions/abuse-report-limit-redis-count-unit.md asks whether each
// report should count), and an infrastructure address (WARP_LIMIT_EXCLUDE_SUBNETS) counts nothing. The count is
// kept in Redis through server.CheckIpRateLimitAttempt, at abuse_report.{address_<hash>}.address, so the limit
// holds across api processes (§2.8 api-14; owner decision 536, QD-895): the history holds an opaque id per
// attempt, scored by when it leaves the window, and the key expires with AbuseRouteSettings' window. No address
// reaches Redis, and nothing here logs one (R-ABUSE-6). A count Redis does not answer refuses the request as a
// store that did not answer does, so no request goes uncounted.
//
// A barred case is absent from the cases route, its appeal answers exactly as an unknown id's, and status shows
// only its standing (plan-ABUSE-small-readings #6). No read here touches a hold, so no field, status code, timing
// branch or log line differs for a held network (D-ABUSE-8). The appeal route refuses a client token twice: the
// router's Network class refuses every client credential, the same gate /smtp/appeal will take, and the handler
// refuses one that reached it anyway. The routes' logs carry no network id, case id, target or reason (I-13).

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"slices"
	"sync"
	"time"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/smtpedgeservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

// Tunables of the customer routes.
type AbuseRouteSettings struct {
	// The most bytes of a report's body: 512 KiB, ReportUrlMaxCount URLs and ReportHeaderBlockMaxCount header
	// blocks at their bounds with room for the JSON, far under the router's own cap.
	ReportBodyMaxByteCount int64
	// The most URLs one report takes: 16. Each is its own report.
	ReportUrlMaxCount int
	// The most bytes of one URL: the join's own bound (model.AbuseSettings.ReportUrlMaxByteCount), so a URL the
	// join would drop unread is refused here instead.
	ReportUrlMaxByteCount int
	// The most header blocks one report takes: 4. Each is its own report.
	ReportHeaderBlockMaxCount int
	// The most bytes of one header block: §14.8's parse's own bound (smtpedgeservice's ReportHeaderByteCount,
	// 64 KiB), past which it reads nothing.
	ReportHeaderBlockMaxByteCount int
	// One caller address hash's report requests in ReportAddressLookback: 30 an hour, a person reporting what they
	// found many times over, or an office behind one address. A refused request counts too, so a caller past the
	// limit stays there until it stops. Every api process shares the count through Redis, where a hash's history
	// expires with the lookback. Non-positive limits nothing. A lookback under a millisecond cannot be kept, so
	// with a positive limit every request is refused as one the count did not answer.
	ReportAddressLimit    int
	ReportAddressLookback time.Duration
}

// The prefix of the report count's Redis histories, abuse_report.{address_<hash>}.address (§2.8 api-14).
const abuseReportRateLimitKeyPrefix = "abuse_report."

// §21's bounds.
func DefaultAbuseRouteSettings() *AbuseRouteSettings {
	return &AbuseRouteSettings{
		ReportBodyMaxByteCount:        512 * 1024,
		ReportUrlMaxCount:             16,
		ReportUrlMaxByteCount:         model.DefaultAbuseSettings().ReportUrlMaxByteCount,
		ReportHeaderBlockMaxCount:     4,
		ReportHeaderBlockMaxByteCount: smtpedgeservice.DefaultSmtpEdgeSettings().ReportHeaderByteCount,
		ReportAddressLimit:            30,
		ReportAddressLookback:         time.Hour,
	}
}

// POST /abuse/report's body.
type AbuseReportArgs struct {
	// §21.4's row the reporter picks (AbusePublicReportCategories)
	Category string `json:"category"`
	// reported URLs or bare host names; of each only its host is read
	Urls []string `json:"urls"`
	// header blocks as pasted, each read by §14.8's header-only parse; nothing past a block's blank line is read
	HeaderBlocks []string `json:"header_blocks"`
}

// POST /abuse/report's answer, the same whether or not the report named a URnetwork name.
type AbuseReportResult struct {
	// always true
	Received bool `json:"received"`
}

// The categories the public form offers: §21.4's rows and other. A brand held for review is ScreenName's own
// case, never a report's.
func AbusePublicReportCategories() []model.AbuseCategory {
	return []model.AbuseCategory{
		model.AbuseCategoryPhishing,
		model.AbuseCategoryMalware,
		model.AbuseCategoryCommandAndControl,
		model.AbuseCategorySpam,
		model.AbuseCategoryCopyright,
		model.AbuseCategoryExposedPanel,
		model.AbuseCategoryCompromise,
		model.AbuseCategoryCsam,
		model.AbuseCategoryThreatToLife,
		model.AbuseCategoryOther,
	}
}

// What the routes run on. Each nil takes api's own.
type AbuseHandlersDeps struct {
	// The clock. nil: server.NowUtc.
	Now func() time.Time
	// Whether an address is infrastructure, which counts nothing. nil: server.IsRateLimitExcluded.
	RateLimitExcluded func(addr netip.Addr) bool
	// Takes one report. nil: controller.AcceptAbuseReport.
	AcceptReport func(ctx context.Context, report *model.AbuseReport) (*controller.AbuseIntakeResult, error)
	// The trusted flaggers' keys (ABUSE-M8-01). nil: vault's abuse-flaggers.yml under abuse.yml's list.
	Flaggers *model.AbuseFlaggerSource
	// The flaggers' daily kill cap, each call. nil: the defaults with abuse.yml's flagger_kills_per_day.
	FlaggerSettings func() *model.AbuseFlaggerSettings
	// The customer reads' settings, each call. nil: the defaults with abuse.yml's appeal window and link.
	CustomerSettings func() *model.AbuseCustomerSettings
	// nil: the defaults.
	Settings *AbuseRouteSettings
}

// The four routes, the file header's. Safe for concurrent use. The report count is Redis's, so instances in one
// process or many count each caller once.
type AbuseHandlers struct {
	now               func() time.Time
	rateLimitExcluded func(addr netip.Addr) bool
	acceptReport      func(ctx context.Context, report *model.AbuseReport) (*controller.AbuseIntakeResult, error)
	flaggers          *model.AbuseFlaggerSource
	flaggerSettings   func() *model.AbuseFlaggerSettings
	customerSettings  func() *model.AbuseCustomerSettings
	settings          *AbuseRouteSettings
}

// The routes on deps, nil taking api's own.
func NewAbuseHandlers(deps *AbuseHandlersDeps) *AbuseHandlers {
	if deps == nil {
		deps = &AbuseHandlersDeps{}
	}
	self := &AbuseHandlers{
		now:               deps.Now,
		rateLimitExcluded: deps.RateLimitExcluded,
		acceptReport:      deps.AcceptReport,
		flaggers:          deps.Flaggers,
		flaggerSettings:   deps.FlaggerSettings,
		customerSettings:  deps.CustomerSettings,
		settings:          deps.Settings,
	}
	if self.now == nil {
		self.now = server.NowUtc
	}
	if self.rateLimitExcluded == nil {
		self.rateLimitExcluded = server.IsRateLimitExcluded
	}
	if self.acceptReport == nil {
		self.acceptReport = controller.AcceptAbuseReport
	}
	if self.flaggerSettings == nil {
		self.flaggerSettings = func() *model.AbuseFlaggerSettings {
			return model.AbuseFlaggerSettingsOf(server.GetAbuseConfig())
		}
	}
	if self.flaggers == nil {
		self.flaggers = model.NewAbuseFlaggerSource(nil, self.flaggerSettings())
	}
	if self.customerSettings == nil {
		self.customerSettings = func() *model.AbuseCustomerSettings {
			settings := model.DefaultAbuseCustomerSettings()
			model.ApplyAbuseCustomerConfig(settings, server.GetAbuseConfig())
			return settings
		}
	}
	if self.settings == nil {
		self.settings = DefaultAbuseRouteSettings()
	}
	return self
}

// api's routes, made on first use.
var defaultAbuseHandlers = sync.OnceValue(func() *AbuseHandlers {
	return NewAbuseHandlers(nil)
})

// POST /abuse/report on api's own routes.
func AbuseReport(w http.ResponseWriter, r *http.Request) {
	defaultAbuseHandlers().Report(w, r)
}

// GET /abuse/cases on api's own routes.
func AbuseCases(w http.ResponseWriter, r *http.Request) {
	defaultAbuseHandlers().Cases(w, r)
}

// POST /abuse/appeal on api's own routes.
func AbuseAppeal(w http.ResponseWriter, r *http.Request) {
	defaultAbuseHandlers().Appeal(w, r)
}

// GET /abuse/status on api's own routes.
func AbuseStatus(w http.ResponseWriter, r *http.Request) {
	defaultAbuseHandlers().Status(w, r)
}

// The answer to a report past its caller's limit, with when to try again.
type abuseReportLimitError struct {
	retryAfterSeconds int
}

func (self *abuseReportLimitError) Error() string {
	return fmt.Sprintf("%d Too many reports from this network address. Try again later.", http.StatusTooManyRequests)
}

func (self *abuseReportLimitError) RetryAfterSeconds() int {
	return self.retryAfterSeconds
}

// The fixed refusals of a report, which say what was wrong and never echo it.
var (
	errAbuseReportCategory = fmt.Errorf("%d The report needs one of the listed categories.", http.StatusBadRequest)
	errAbuseReportEmpty    = fmt.Errorf("%d The report needs a URL or a header block.", http.StatusBadRequest)
	errAbuseReportTooLarge = fmt.Errorf("%d The report holds too many or too long URLs or header blocks.", http.StatusBadRequest)
	errAbuseReportClosed   = fmt.Errorf("%d Reports cannot be taken right now. Try again later.", http.StatusServiceUnavailable)
)

// The reports args holds, one a URL and one a header block, each its own, as reporterClass with flaggerId, or a
// refusal of the whole body.
func (self *AbuseHandlers) abuseReportsOf(args *AbuseReportArgs, reporterClass model.AbuseReporterClass, flaggerId string) ([]*model.AbuseReport, error) {
	category := model.AbuseCategory(args.Category)
	if !slices.Contains(AbusePublicReportCategories(), category) {
		return nil, errAbuseReportCategory
	}
	if len(args.Urls) == 0 && len(args.HeaderBlocks) == 0 {
		return nil, errAbuseReportEmpty
	}
	if self.settings.ReportUrlMaxCount < len(args.Urls) || self.settings.ReportHeaderBlockMaxCount < len(args.HeaderBlocks) {
		return nil, errAbuseReportTooLarge
	}
	reports := []*model.AbuseReport{}
	for _, url := range args.Urls {
		if url == "" || self.settings.ReportUrlMaxByteCount < len(url) {
			return nil, errAbuseReportTooLarge
		}
		reports = append(reports, &model.AbuseReport{
			Category:      category,
			ReporterClass: reporterClass,
			Url:           url,
			FlaggerId:     flaggerId,
		})
	}
	for _, headerBlock := range args.HeaderBlocks {
		if headerBlock == "" || self.settings.ReportHeaderBlockMaxByteCount < len(headerBlock) {
			return nil, errAbuseReportTooLarge
		}
		reports = append(reports, &model.AbuseReport{
			Category:      category,
			ReporterClass: reporterClass,
			HeaderBlock:   []byte(headerBlock),
			FlaggerId:     flaggerId,
		})
	}
	return reports, nil
}

// Counts one request against the caller's keyed address hash in Redis, refusing past the limit. An infrastructure
// address counts nothing and touches no store, and a caller whose address cannot be read is not counted: refusing
// it would deny every caller behind an unreadable proxy chain while teaching nothing.
func (self *AbuseHandlers) limitReport(clientSession *session.ClientSession) error {
	if self.settings.ReportAddressLimit <= 0 {
		return nil
	}
	clientIp, _, err := server.SplitClientAddress(clientSession.ClientAddress)
	if err != nil {
		if glog.V(1) {
			glog.Infof("[abuse]a report's caller address could not be read, so it is not counted\n")
		}
		return nil
	}
	addr, err := netip.ParseAddr(clientIp)
	if err != nil {
		if glog.V(1) {
			glog.Infof("[abuse]a report's caller address could not be read, so it is not counted\n")
		}
		return nil
	}
	addr = addr.Unmap()
	if self.rateLimitExcluded(addr) {
		return nil
	}
	// the history is the keyed hash's alone: the address leaves this function only as that hash
	_, allowed, err := server.CheckIpRateLimitAttempt(
		clientSession.Ctx,
		server.NewStoredRateLimitClient(server.ClientIpHashForAddr(addr)),
		"",
		self.now(),
		server.IpRateLimitAttemptSettings{
			KeyPrefix:       abuseReportRateLimitKeyPrefix,
			AddressLookback: self.settings.ReportAddressLookback,
			// the history holds this attempt and refuses at its limit; with no identity scope there is no global
			// history, though the settings still need its window and count
			AddressLimit:   self.settings.ReportAddressLimit + 1,
			GlobalLookback: self.settings.ReportAddressLookback,
			GlobalLimit:    self.settings.ReportAddressLimit + 1,
		},
	)
	if err != nil {
		// a count not kept refuses the request rather than let it pass uncounted; the cause names no caller
		if glog.V(1) {
			glog.Infof("[abuse]a report's count was not kept, so it is refused: %s\n", err)
		}
		return errAbuseReportClosed
	}
	if !allowed {
		return &abuseReportLimitError{retryAfterSeconds: int(self.settings.ReportAddressLookback / time.Second)}
	}
	return nil
}

// POST /abuse/report: the file header's report route.
func (self *AbuseHandlers) Report(w http.ResponseWriter, r *http.Request) {
	if !server.SidecarMg5Linked() {
		// no case can be kept, so every report alike is answered 503 before its body is read
		server.WriteGateClosed(w, 0)
		return
	}
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, self.settings.ReportBodyMaxByteCount)
	}
	// a trusted flagger's key (ABUSE-M8-01): a match sets its class unless it spent its day's kills, and a wrong or
	// absent key is a plain report, which the answer never tells apart
	flaggerKey := r.Header.Get(model.AbuseFlaggerKeyHeader)
	impl := func(args AbuseReportArgs, clientSession *session.ClientSession) (*AbuseReportResult, error) {
		if err := self.limitReport(clientSession); err != nil {
			return nil, err
		}
		reporterClass, flaggerId := model.AbuseReporterClassPublic, ""
		if flaggerKey != "" {
			now := self.now()
			if id, ok := self.flaggers.Verifier(now).Authenticate(flaggerKey); ok {
				reporterClass, flaggerId = model.AbuseFlaggerReportClass(clientSession.Ctx, id, now, self.flaggerSettings())
			}
		}
		reports, err := self.abuseReportsOf(&args, reporterClass, flaggerId)
		if err != nil {
			return nil, err
		}
		for _, report := range reports {
			if _, err := self.acceptReport(clientSession.Ctx, report); err != nil {
				// a store that did not answer, or a refusal no public report reaches: the same answer for every
				// report, joined or not, and the cause by its kind alone
				if errors.Is(err, model.ErrAbuseReportInvalid) {
					return nil, errAbuseReportCategory
				}
				if glog.V(1) {
					glog.Infof("[abuse]a report was not taken: %s\n", err)
				}
				return nil, errAbuseReportClosed
			}
		}
		return &AbuseReportResult{Received: true}, nil
	}
	router.WrapWithInputNoAuth(router.PrivateErrorsWithInput(impl), w, r)
}

// GET /abuse/cases: the caller's network's cases it was told of.
func (self *AbuseHandlers) Cases(w http.ResponseWriter, r *http.Request) {
	impl := func(clientSession *session.ClientSession) (*model.AbuseNetworkCasesResult, error) {
		if clientSession.ByJwt.ClientId != nil {
			return nil, fmt.Errorf("%d %s", http.StatusForbidden, router.ClientCredentialRefusedMessage)
		}
		return model.ListNetworkAbuseCases(clientSession.Ctx, clientSession.ByJwt.NetworkId, self.customerSettings()), nil
	}
	router.WrapRequireAuth(router.PrivateErrors(impl), w, r)
}

// POST /abuse/appeal: an Owner's or Admin's appeal of a case of the caller's network.
func (self *AbuseHandlers) Appeal(w http.ResponseWriter, r *http.Request) {
	impl := func(args model.AbuseAppealArgs, clientSession *session.ClientSession) (*model.AbuseAppealResult, error) {
		if clientSession.ByJwt.ClientId != nil {
			return nil, fmt.Errorf("%d %s", http.StatusForbidden, router.ClientCredentialRefusedMessage)
		}
		settings := self.customerSettings()
		appeal, _, err := model.CreateAbuseAppeal(
			clientSession.Ctx,
			clientSession.ByJwt.NetworkId,
			clientSession.ByJwt.UserId,
			&args,
			self.now(),
			settings,
		)
		switch {
		case err == nil:
			return appeal.Result(), nil
		case errors.Is(err, model.ErrAbuseCaseNotFound), errors.Is(err, model.ErrAbuseUnlinked):
			// an unknown id, another network's case and a barred case answer alike
			return nil, fmt.Errorf("%d Unknown case.", http.StatusNotFound)
		case errors.Is(err, model.ErrAbuseAppealInvalid):
			return nil, fmt.Errorf("%d The appeal needs a reason of at most %d bytes of text.", http.StatusBadRequest, settings.AppealReasonMaxByteCount)
		case errors.Is(err, model.ErrAbuseAppealWindowClosed):
			return nil, fmt.Errorf("%d The appeal window of this case has closed.", http.StatusConflict)
		case errors.Is(err, model.ErrAbuseAppealDecided):
			return nil, fmt.Errorf("%d The appeal of this case was decided.", http.StatusConflict)
		}
		return nil, err
	}
	router.WrapWithInputRequireAuth(router.PrivateErrorsWithInput(impl), w, r)
}

// GET /abuse/status: the caller's own network's status, for a client token or the network's own credential.
func (self *AbuseHandlers) Status(w http.ResponseWriter, r *http.Request) {
	impl := func(clientSession *session.ClientSession) (*sidecar.AbuseStatus, error) {
		status, found := model.GetNetworkAbuseStatus(clientSession.Ctx, clientSession.ByJwt.NetworkId, self.now(), self.customerSettings())
		if !found {
			return nil, fmt.Errorf("%d Unknown network.", http.StatusNotFound)
		}
		return status, nil
	}
	router.WrapRequireAuth(router.PrivateErrors(impl), w, r)
}
