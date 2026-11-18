package controller

// §21.8's paged notices and review clocks (ABUSE-M5-10; D-ABUSE-3, D-ABUSE-5,
// D-OPS-9, FS-49, FS-50, Q-ABUSE-6, I-13; owner decisions 134, 143, 530).
//
// FileAbuseNotice takes a notice of a class that pages (model.AbuseNoticeClass:
// a registrar's or registry's, a law-enforcement emergency notice, an EU
// removal order, a production order), opens its case due within the hour, and
// pages both rota members at once through the process's pager.Sink, at any
// hour. The page's key holds only the case id and its text only the class, the
// time and that id (pager.PageNotice; I-13), so the person paged reads the
// rest through the staff route by case id. Who handles it is the result's
// route: removal orders and registrar or registry notices stay with the rota,
// production orders and other data requests go to abuse.yml's counsel_contact.
//
// A notice reaches here two ways and no other (owner decision 530): a person
// files it through the staff route's file_notice, or a role mail's DKIM
// verifies under a domain abuse.yml's notice_senders lists
// (FileAbuseNoticeOfMail), with no body read. The public form reaches neither,
// so a forged notice pages nothing. Nothing here kills anything: the host a
// registrar's or registry's URL names is the rota's to kill, through the staff
// route's action under the notice's case, which is §21's one action path.
//
// A lost page is the worst failure here, so nothing swallows the pager's
// error. FileAbuseNotice returns it beside the case it opened, and a caller
// retries with that case id, which pages under the same key and opens no
// second case. Past the caller, AbuseClockPass pages every notice whose case
// is still open again each minute for NoticeRepageWindow, so a crash between
// the case's commit and the pager, or a process with no pager, costs a minute
// and not the page. A page still open at the pager takes no second, since the
// key is the case's. Reviewing the case closes it and ends the paging.
//
// The same pass counts the open cases past their review clock into
// urnetwork_abuse_review_overdue, which alerts as a business-hours warning and
// never pages. A clock only reports: nothing here lifts or changes an action.
//
// A process pages only once it installs its pager (SetAbuseNoticePager). Until
// then every page is refused, the notice's case stands, and the pass of a
// process that has one pages it. api and taskworker install it at start
// (InstallAbuseNoticePager; QD-1070): a pager.RetrySink over the webhook the
// monitor pages through, read from the monitor's own vault file.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"gopkg.in/yaml.v3"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/pager"
)

// No pager is installed, so the page is owed: the notice's case stands, and
// the clock pass of a process that has a pager pages it.
var ErrAbuseNoticePagerUnavailable = errors.New("abuse notice pager unavailable")

// What a process has before it installs its pager: it refuses every page, so
// no notice reads as paged.
type unavailableAbuseNoticePager struct{}

// Refuses: see the type.
func (self *unavailableAbuseNoticePager) Trigger(ctx context.Context, page pager.Page) error {
	return ErrAbuseNoticePagerUnavailable
}

// Refuses: see the type.
func (self *unavailableAbuseNoticePager) Resolve(ctx context.Context, dedupKey string) error {
	return ErrAbuseNoticePagerUnavailable
}

// The process's pager and the notices' seams, which a test replaces.
var abuseNoticeDeps = struct {
	stateLock   sync.Mutex
	sink        pager.Sink
	now         func() time.Time
	settings    func() *model.AbuseSettings
	environment func() string
}{
	sink: &unavailableAbuseNoticePager{},
	now:  server.NowUtc,
	settings: func() *model.AbuseSettings {
		settings := model.DefaultAbuseSettings()
		model.ApplyAbuseConfig(settings, server.GetAbuseConfig())
		return settings
	},
	environment: func() string {
		env, err := server.Env()
		if err != nil {
			return ""
		}
		return env
	},
}

// The pager in force now.
func currentAbuseNoticePager() pager.Sink {
	abuseNoticeDeps.stateLock.Lock()
	defer abuseNoticeDeps.stateLock.Unlock()
	return abuseNoticeDeps.sink
}

// Installs the process's pager for notices, returning what puts the previous
// one back. A sink that queues, as pager.RetrySink does, should also offer
// Flush: filing then waits for the pager to take the page before it answers
// paged.
func SetAbuseNoticePager(sink pager.Sink) (restore func()) {
	abuseNoticeDeps.stateLock.Lock()
	defer abuseNoticeDeps.stateLock.Unlock()
	previous := abuseNoticeDeps.sink
	abuseNoticeDeps.sink = sink
	return func() {
		abuseNoticeDeps.stateLock.Lock()
		defer abuseNoticeDeps.stateLock.Unlock()
		abuseNoticeDeps.sink = previous
	}
}

// The vault file holding the pager's endpoint: the monitor's, so api and
// taskworker page through the webhook the monitor pages through (QD-1070).
const AbuseNoticePagerFileName = "monitor.yml"

// The one key of monitor.yml a notice's pager reads.
type abuseNoticePagerYaml struct {
	Alerts struct {
		PagerWebhookUrl string `yaml:"pager_webhook_url"`
	} `yaml:"alerts"`
}

// The pager's sink is not built. The text names the part that failed and never
// the endpoint, which is a credential.
var ErrAbuseNoticePagerRefused = errors.New("abuse notice pager not built")

// A retrying sink over the webhook monitor.yml names (QD-1070). An error is a
// file that does not read or an endpoint pager.NewWebhookSink refuses: none,
// or, while plan-OPS-pager-service names no service, one off loopback.
func newAbuseNoticePagerSink(monitorYml []byte) (*pager.RetrySink, error) {
	pagerYaml := &abuseNoticePagerYaml{}
	// a decode error can quote the line it failed on, so only the verdict is kept
	if err := yaml.Unmarshal(monitorYml, pagerYaml); err != nil {
		return nil, fmt.Errorf("%w: %s does not parse", ErrAbuseNoticePagerRefused, AbuseNoticePagerFileName)
	}
	webhookSettings := pager.DefaultWebhookSinkSettings()
	webhookSettings.Url = pagerYaml.Alerts.PagerWebhookUrl
	webhookSink, err := pager.NewWebhookSink(webhookSettings)
	if err != nil {
		// the webhook sink's errors are fixed words, never the endpoint
		return nil, fmt.Errorf("%w: %w", ErrAbuseNoticePagerRefused, err)
	}
	retrySettings := pager.DefaultRetrySinkSettings()
	retrySettings.Logf = func(format string, args ...any) {
		glog.Errorf("[abuse]notice pager: "+format+"\n", args...)
	}
	return pager.NewRetrySink(webhookSink, retrySettings), nil
}

// Installs this process's notice pager from the monitor's vault file, in a
// binary that links MG5: api's and taskworker's startup (QD-1070). A pager
// that cannot be built installs nothing and says why, so every page stays owed
// and the clock pass pages it once a process has one. Returns what puts the
// previous pager back and closes the sink.
func InstallAbuseNoticePager() (restore func()) {
	if !server.SidecarMg5Linked() {
		return func() {}
	}
	monitorYml, err := func() ([]byte, error) {
		resource, err := server.Vault.SimpleResource(AbuseNoticePagerFileName)
		if err != nil {
			return nil, err
		}
		return resource.BytesE()
	}()
	if err != nil {
		glog.Errorf("[abuse]no notice pager: %s is not mounted, so every notice's page is owed\n", AbuseNoticePagerFileName)
		return func() {}
	}
	sink, err := newAbuseNoticePagerSink(monitorYml)
	if err != nil {
		glog.Errorf("[abuse]no notice pager, so every notice's page is owed: %s\n", err)
		return func() {}
	}
	restorePager := SetAbuseNoticePager(sink)
	return func() {
		restorePager()
		ctx, cancel := context.WithTimeout(context.Background(), pager.DefaultRetrySinkSettings().SendTimeout)
		defer cancel()
		sink.Close(ctx)
	}
}

// A sink whose Trigger only queues: Flush answers once the pager took what
// was queued.
type abuseNoticeFlusher interface {
	Flush(ctx context.Context) error
}

// Counts notices by class and by what became of their page: paged, owed (the
// pager refused or is not installed), repaged and repage_owed for the clock
// pass's, and refused for a notice of a class that does not page. No label
// carries a name or an id.
var abuseNoticeCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "abuse",
		Name:      "notices_total",
		Help:      "Paged abuse notices by class and by what became of the page",
	},
	[]string{"class", "result"},
)

// The open cases past their review clock, as the last clock pass counted them.
// Its alert is a business-hours warning and never a page (§21.8).
var abuseReviewOverdueGauge = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Namespace: "urnetwork",
		Subsystem: "abuse",
		Name:      "review_overdue",
		Help:      "Open abuse cases past their review clock at the last clock pass",
	},
)

// Registers the notices' metrics with the default registry.
func init() {
	prometheus.MustRegister(abuseNoticeCounter)
	prometheus.MustRegister(abuseReviewOverdueGauge)
}

// The default-level reports of pages owed and of reviews overdue.
var (
	abuseNoticePageFailures  = newBoundedReport(abuseActionReportInterval)
	abuseReviewOverdueReport = newBoundedReport(abuseActionReportInterval)
)

// One notice to file.
type AbuseNoticeArgs struct {
	Class model.AbuseNoticeClass
	// the name the notice is about: a root, or a host under one
	Target string
	// §21.4's row, when the filer knows it; empty is other
	Category model.AbuseCategory
	// a sealing order bars every notice to the network (§21.8)
	Barred bool
	// A notice filed before, whose page is to be sent again: its case, which
	// must be an open notice case of Class. Nil opens a case.
	CaseId *server.Id
}

// What became of a notice.
type AbuseNoticeResult struct {
	CaseId server.Id
	// false when CaseId named a case filed before
	Opened bool
	// the pager took the page, for both rota members at once
	Paged bool
	// who handles the notice within the hour
	Route model.AbuseNoticeRoute
	// abuse.yml's counsel_contact when the route is counsel, "" when the file
	// names none. It never enters the page.
	CounselContact string
	// the paged hour's end
	ReviewDueTime time.Time
}

// Files one notice, the path in the file header. An error with a nil result
// is the notice's refusal (model.ErrAbuseCaseInvalid for a class that does
// not page or a target that is no host name, model.ErrAbuseCaseNotFound for a
// retry that names no open notice case of the class), a binary that does not
// link MG5 (model.ErrAbuseUnlinked) or a store that did not answer. An error
// beside a result is the pager's own: the case stands, the page is owed, and
// the caller retries with the result's case id.
func FileAbuseNotice(ctx context.Context, args *AbuseNoticeArgs) (result *AbuseNoticeResult, returnErr error) {
	defer func() {
		// the store raises a failed read or write
		if r := recover(); r != nil {
			if server.IsDoneError(r) {
				panic(r)
			}
			result = nil
			returnErr = errors.New("abuse notice: the store did not answer")
			if glog.V(1) {
				glog.Infof("[abuse]filing a notice failed: %v\n", r)
			}
		}
	}()
	if !server.SidecarMg5Linked() {
		return nil, model.ErrAbuseUnlinked
	}
	if !args.Class.Pages() {
		// the label is a fixed word, never the class a caller sent
		abuseNoticeCounter.WithLabelValues("none", "refused").Inc()
		return nil, fmt.Errorf("%w: a notice of a class that does not page", model.ErrAbuseCaseInvalid)
	}
	settings := abuseNoticeDeps.settings()
	now := abuseNoticeDeps.now()

	result = &AbuseNoticeResult{Route: args.Class.Route()}
	if result.Route == model.AbuseNoticeRouteCounsel {
		result.CounselContact = server.GetAbuseConfig().CounselContact()
	}
	var noticeTime time.Time
	if args.CaseId != nil {
		abuseCase := model.GetAbuseCase(ctx, *args.CaseId)
		if abuseCase == nil || abuseCase.State != model.AbuseCaseStateOpen || model.AbuseNoticeClassOfCase(abuseCase) != args.Class {
			return nil, model.ErrAbuseCaseNotFound
		}
		result.CaseId = abuseCase.CaseId
		noticeTime = abuseCase.CreateTime
		if abuseCase.ReviewDueTime != nil {
			result.ReviewDueTime = *abuseCase.ReviewDueTime
		}
	} else {
		noticeCase, err := model.OpenAbuseNoticeCase(ctx, &model.AbuseNoticeCaseArgs{
			Class:    args.Class,
			Target:   args.Target,
			Category: args.Category,
			Barred:   args.Barred,
		}, now, settings)
		if err != nil {
			return nil, err
		}
		result.CaseId = noticeCase.CaseId
		result.Opened = true
		result.ReviewDueTime = noticeCase.ReviewDueTime
		noticeTime = now
	}

	err := pageAbuseNotice(ctx, args.Class, result.CaseId, noticeTime, settings)
	if err != nil {
		abuseNoticeCounter.WithLabelValues(string(args.Class), "owed").Inc()
		reportAbuseNoticePageFailure(err)
		return result, err
	}
	abuseNoticeCounter.WithLabelValues(string(args.Class), "paged").Inc()
	result.Paged = true
	return result, nil
}

// Files the notice a role mail is, if it is one (owner decision 530): a mail
// to abuse@ or postmaster@ whose DKIM verified under a domain abuse.yml's
// notice_senders lists pages as that domain's class, about the root its role
// address sits under. Any other mail is no notice: a nil result and a nil
// error, with nothing opened and nothing paged. No body is an argument here.
func FileAbuseNoticeOfMail(ctx context.Context, mail *model.AbuseNoticeMail, recipientDomain string) (*AbuseNoticeResult, error) {
	class := model.ClassifyAbuseNotice(mail, server.GetAbuseConfig())
	if !class.Pages() {
		return nil, nil
	}
	return FileAbuseNotice(ctx, &AbuseNoticeArgs{Class: class, Target: recipientDomain})
}

// Pages both rota members for one notice under its case's key. The error is
// the pager's, unchanged: a refusal, a hand-off the pager did not take within
// NoticePageTimeout, or ErrAbuseNoticePagerUnavailable.
func pageAbuseNotice(
	ctx context.Context,
	class model.AbuseNoticeClass,
	caseId server.Id,
	noticeTime time.Time,
	settings *model.AbuseSettings,
) error {
	sink := currentAbuseNoticePager()
	err := pager.PageNotice(ctx, sink, pager.Notice{
		// the classes are the pager's kinds, word for word
		Kind:        pager.NoticeKind(class),
		CaseId:      caseId,
		Time:        noticeTime,
		Environment: abuseNoticeDeps.environment(),
	})
	if err != nil {
		return err
	}
	if flusher, ok := sink.(abuseNoticeFlusher); ok {
		// the sink only queued the page; wait for the pager to take it
		flushCtx, cancel := context.WithTimeout(ctx, settings.NoticePageTimeout)
		defer cancel()
		if err := flusher.Flush(flushCtx); err != nil {
			return fmt.Errorf("abuse notice: the pager has not taken the page: %w", err)
		}
	}
	return nil
}

// Reports a page that is owed at most once per interval, naming no case.
func reportAbuseNoticePageFailure(err error) {
	if suppressedCount, ok := abuseNoticePageFailures.Allow(time.Now()); ok {
		unavailable := errors.Is(err, ErrAbuseNoticePagerUnavailable)
		glog.Errorf("[abuse]a notice's page is owed (pager installed: %t), and the clock pass pages it again; %d more since the last report\n", !unavailable, suppressedCount)
	}
	if glog.V(1) {
		glog.Infof("[abuse]a notice's page failed: %s\n", err)
	}
}

// What one clock pass found.
type AbuseClockPassResult struct {
	// open cases past their review clock
	OverdueCount int64
	// open notices paged again
	NoticePagedCount int
	// open notices whose page the pager did not take
	NoticeOwedCount int
}

// One clock pass at now (see the file header): counts the open cases past
// their review clock into urnetwork_abuse_review_overdue, then pages again
// each notice whose case is still open and was filed inside
// NoticeRepageWindow, oldest first, up to NoticeRepageBatchSize. A pager that
// is not installed ends the paging for the pass, since each further page
// would be refused the same way. The pass writes nothing: a clock never lifts
// an action. A binary that does not link MG5 does nothing.
func RunAbuseClockPass(ctx context.Context, now time.Time, settings *model.AbuseSettings) *AbuseClockPassResult {
	result := &AbuseClockPassResult{}
	if !server.SidecarMg5Linked() {
		return result
	}
	startTime := time.Now()

	result.OverdueCount = model.CountOverdueAbuseReviews(ctx, now)
	abuseReviewOverdueGauge.Set(float64(result.OverdueCount))
	if 0 < result.OverdueCount {
		if _, ok := abuseReviewOverdueReport.Allow(time.Now()); ok {
			glog.Infof("[abuse]%d open cases are past their review clock\n", result.OverdueCount)
		}
	}

	if settings.NoticeRepageWindow <= 0 {
		return result
	}
	cases := model.ListOpenAbuseNoticeCases(ctx, now.Add(-settings.NoticeRepageWindow), settings.NoticeRepageBatchSize)
	for i, abuseCase := range cases {
		if ctx.Err() != nil || settings.ClockPassMaxTime <= time.Since(startTime) {
			result.NoticeOwedCount += len(cases) - i
			return result
		}
		class := model.AbuseNoticeClassOfCase(abuseCase)
		if !class.Pages() {
			// a key this build does not know as a class: it cannot be paged
			// here, and it stays open for a person
			result.NoticeOwedCount += 1
			continue
		}
		err := pageAbuseNotice(ctx, class, abuseCase.CaseId, abuseCase.CreateTime, settings)
		if err == nil {
			abuseNoticeCounter.WithLabelValues(string(class), "repaged").Inc()
			result.NoticePagedCount += 1
			continue
		}
		abuseNoticeCounter.WithLabelValues(string(class), "repage_owed").Inc()
		reportAbuseNoticePageFailure(err)
		result.NoticeOwedCount += 1
		if errors.Is(err, ErrAbuseNoticePagerUnavailable) {
			result.NoticeOwedCount += len(cases) - i - 1
			return result
		}
	}
	return result
}

// file_notice's body on the staff route.
type abuseStaffFileNoticeArgs struct {
	Kind  string `json:"kind"`
	Class string `json:"class"`
	// the name the notice is about: a root, or a host under one
	Target   string `json:"target"`
	Category string `json:"category,omitempty"`
	Barred   bool   `json:"barred,omitempty"`
	// a notice filed before, to page again
	CaseId *server.Id `json:"case_id,omitempty"`
}

// file_notice's answer.
type AbuseStaffNoticeResult struct {
	CaseId server.Id `json:"case_id"`
	Opened bool      `json:"opened"`
	// false when the pager did not take the page: the case stands, the clock
	// pass pages it again within the minute, and the filer may file again with
	// case_id
	Paged bool `json:"paged"`
	// rota or counsel
	Route          string `json:"route"`
	CounselContact string `json:"counsel_contact,omitempty"`
	ReviewDueTime  int64  `json:"review_due_time_ms"`
}

// The staff route's notice verb.
func init() {
	RegisterAbuseStaffKind(&AbuseStaffKind{
		Name:  "file_notice",
		Roles: []AbuseStaffRole{AbuseStaffRoleReviewer, AbuseStaffRoleLegal},
		Run: func(ctx context.Context, staff *AbuseStaff, body []byte) (any, error) {
			args := &abuseStaffFileNoticeArgs{}
			if err := decodeAbuseStaffArgs(body, args); err != nil {
				return nil, err
			}
			result, err := FileAbuseNotice(ctx, &AbuseNoticeArgs{
				Class:    model.AbuseNoticeClass(args.Class),
				Target:   args.Target,
				Category: model.AbuseCategory(args.Category),
				Barred:   args.Barred,
				CaseId:   args.CaseId,
			})
			switch {
			case errors.Is(err, model.ErrAbuseCaseInvalid):
				return nil, fmt.Errorf("%w: a class that does not page, a target that is no host name or an unknown category", ErrAbuseStaffRequestInvalid)
			case result == nil:
				return nil, err
			}
			// a page the pager did not take is answered, not failed: the filer
			// needs the case id to file again, and the answer says the page is
			// owed
			return &AbuseStaffNoticeResult{
				CaseId:         result.CaseId,
				Opened:         result.Opened,
				Paged:          result.Paged,
				Route:          string(result.Route),
				CounselContact: result.CounselContact,
				ReviewDueTime:  result.ReviewDueTime.UnixMilli(),
			}, nil
		},
	})
}
