package work

// §21.6's CT watch (ABUSE-M5-13; owner decision 41, Q-ABUSE-1, §2.8 ext-1): every hour the abuse worker reads
// Certificate Transparency for each root through TLS-M5-24's two sources (sidecar.TlsCtClient: Cert Spotter and
// crt.sh), each from a checkpoint cursor, and screens every name under the root of every certificate with
// ScreenName's current lists. A name the lists refuse goes to §21.8 as a report of class ct_watch and category brand,
// which takes no machine action, only review: a CT hit is a report, never a signal for a kill (§21.6). D-TLS-8's
// watch for mis-issuance is TLS-M5-24's, on the publishing sidecars.
//
// A name <hosts>.<label>.<root> is screened as its network's would be at creation, under the network's facts
// (model.GetAbuseScanScreenFacts): its label or alias with ScreenName, each host label with ScreenHost, a wildcard
// and underscore labels skipped. A label no network holds is no abuse case (a certificate for it is D-TLS-8's
// mis-issuance), and the root's own name is no screen's. The first refusal names what is reported: the label, or
// the whole name when a host label is refused.
//
// Each root's cursor for each source rides the task's args, CT's own public ids, so a restart resumes where the last
// run stopped. A cursor moves only past a batch every refused name of which reached §21.8, so a batch that fails is
// read again. A source that cannot be read warns and acts on nothing, its cursor kept. The cursors hold under one
// version of the screen's brand list, by its SHA-256: a release that changes the list reads CT again from the
// start, so every live name meets the current lists. No log names a host or a network (I-13).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// Tunables of the CT watch.
type AbuseCtWatchSettings struct {
	// How often the watch reads CT.
	Interval time.Duration
	// How soon a run that used CallMaxCount with more to read continues.
	ContinueDelay time.Duration
	// How soon a failed run is tried again.
	RetryInterval time.Duration
	// The task runner's bound on one run.
	TaskMaxTime time.Duration
	// The most calls one run makes to one source for one root: a backlog, such as the first run's or a list
	// change's whole history, drains over runs.
	CallMaxCount int
	// The sources' client: smaller batches than a sidecar's own watch reads, since a root holds every network's
	// certificates, and crt.sh fetches each one it lists.
	Client *sidecar.TlsCtClientSettings
}

func DefaultAbuseCtWatchSettings() *AbuseCtWatchSettings {
	client := sidecar.DefaultTlsCtClientSettings()
	client.CertificateMaxCount = 256
	return &AbuseCtWatchSettings{
		Interval:      time.Hour,
		ContinueDelay: time.Minute,
		RetryInterval: 10 * time.Minute,
		TaskMaxTime:   30 * time.Minute,
		CallMaxCount:  8,
		Client:        client,
	}
}

type AbuseCtWatchArgs struct {
	// Each source's cursor for each root, keyed by "<root> <source>": the source's own, CT's public ids.
	Cursors map[string]string `json:"cursors,omitempty"`
	// The hex SHA-256 of the screen's brand list the cursors were read under.
	ListsSha256 string `json:"lists_sha256,omitempty"`
}

// What one run did, and the cursors the next takes. Counts only: no name or network.
type AbuseCtWatchResult struct {
	// no root
	Idle bool `json:"idle"`
	// a source could not be read, or a refused name could not reach §21.8
	Failed bool `json:"failed"`
	// a source used CallMaxCount with more to read
	More        bool              `json:"more"`
	Cursors     map[string]string `json:"cursors,omitempty"`
	ListsSha256 string            `json:"lists_sha256,omitempty"`
	// certificates read, names under a root screened, names refused, and of those the reports that joined
	CertificateCount int `json:"certificate_count"`
	NameCount        int `json:"name_count"`
	RefusedCount     int `json:"refused_count"`
	ReportedCount    int `json:"reported_count"`
	UnjoinedCount    int `json:"unjoined_count"`
}

func (self *AbuseCtWatchResult) outcome() string {
	switch {
	case self.Idle:
		return "idle"
	case self.Failed:
		return "failed"
	case self.More:
		return "more"
	}
	return "done"
}

// The watch's seams, which a test replaces.
type abuseCtWatchDeps struct {
	now          func() time.Time
	config       func() *server.AbuseConfig
	sources      func() ([]sidecar.TlsCtSource, error)
	labelNetwork func(ctx context.Context, root string, label string) (server.Id, bool, error)
	screenFacts  func(ctx context.Context, networkId server.Id, now time.Time) *model.NetworkScreenFacts
	accept       func(ctx context.Context, report *model.AbuseReport) (*controller.AbuseIntakeResult, error)
}

func defaultAbuseCtWatchDeps() *abuseCtWatchDeps {
	return &abuseCtWatchDeps{
		now:          server.NowUtc,
		config:       server.GetAbuseConfig,
		sources:      processAbuseCtSources,
		labelNetwork: model.GetAbuseScanLabelNetwork,
		screenFacts:  model.GetAbuseScanScreenFacts,
		accept:       controller.AcceptAbuseReport,
	}
}

// The process's CT client, made on first need.
var abuseCtWatchProcess = struct {
	stateLock sync.Mutex
	client    *sidecar.TlsCtClient
}{}

// Both sources of the process's client: keyless, dialing directly (TLS-M5-24).
func processAbuseCtSources() ([]sidecar.TlsCtSource, error) {
	abuseCtWatchProcess.stateLock.Lock()
	defer abuseCtWatchProcess.stateLock.Unlock()
	if abuseCtWatchProcess.client == nil {
		client, err := sidecar.NewTlsCtClient(&sidecar.TlsCtClientArgs{Settings: DefaultAbuseCtWatchSettings().Client})
		if err != nil {
			return nil, err
		}
		abuseCtWatchProcess.client = client
	}
	return abuseCtWatchProcess.client.Sources(), nil
}

// The hex SHA-256 of the screen's brand list, which the embedded files fix for a release.
var abuseCtScreenListsSha256 = sync.OnceValue(func() string {
	digest := sha256.New()
	for _, brand := range sidecar.ScreenBrands() {
		io.WriteString(digest, brand.Brand)
		for _, domain := range brand.Domains {
			io.WriteString(digest, " "+domain)
		}
		io.WriteString(digest, "\n")
	}
	return hex.EncodeToString(digest.Sum(nil))
})

// One run of the watch.
type abuseCtWatch struct {
	settings *AbuseCtWatchSettings
	deps     *abuseCtWatchDeps
}

// One run from args' cursors, as the file header says. The result carries the cursors the next run takes.
func (self *abuseCtWatch) run(ctx context.Context, args *AbuseCtWatchArgs) (result *AbuseCtWatchResult) {
	result = &AbuseCtWatchResult{Cursors: map[string]string{}, ListsSha256: args.ListsSha256}
	for key, cursor := range args.Cursors {
		result.Cursors[key] = cursor
	}
	defer func() {
		abuseScanRunCounter.WithLabelValues(abuseScanCtWatch, result.outcome()).Inc()
	}()

	rootNames := self.deps.config().RootNames()
	if len(rootNames) == 0 {
		result.Idle = true
		return
	}
	if listsSha256 := abuseCtScreenListsSha256(); args.ListsSha256 != listsSha256 {
		// a new watch, or new lists: every live name meets the current lists again
		result.Cursors = map[string]string{}
		result.ListsSha256 = listsSha256
	}
	sources, err := self.deps.sources()
	if err != nil || len(sources) == 0 {
		glog.Warningf("[abuse]the ct watch has no source, so it acts on nothing this pass: %v\n", err)
		result.Failed = true
		return
	}

	now := self.deps.now()
	networkIdFacts := map[server.Id]*model.NetworkScreenFacts{}
	reportedNames := map[string]bool{}
	watchedKeys := map[string]bool{}
	for _, rootName := range rootNames {
		root := model.AbuseName(rootName)
		if root == "" {
			continue
		}
		for _, source := range sources {
			key := root + " " + source.Name()
			watchedKeys[key] = true
			cursor := result.Cursors[key]
			for call := range self.settings.CallMaxCount {
				certificates, next, err := source.Issuances(ctx, root, cursor)
				if err != nil {
					glog.Warningf("[abuse]the ct watch could not read %s, so it acts on nothing from it this pass: %v\n", source.Name(), err)
					result.Failed = true
					break
				}
				if err := self.screen(ctx, root, certificates, now, networkIdFacts, reportedNames, result); err != nil {
					glog.Warningf("[abuse]a name the ct watch refused could not reach intake, so its batch is read again: %v\n", err)
					result.Failed = true
					break
				}
				if len(certificates) == 0 || next == cursor {
					break
				}
				cursor = next
				result.Cursors[key] = next
				if call == self.settings.CallMaxCount-1 {
					result.More = true
				}
			}
		}
	}
	// a root or source no longer watched takes its cursor with it
	for key := range result.Cursors {
		if !watchedKeys[key] {
			delete(result.Cursors, key)
		}
	}
	return
}

// Screens each name under root of certificates at now, reporting each refused one once a run. An error is the store
// or the intake not answering.
func (self *abuseCtWatch) screen(
	ctx context.Context,
	root string,
	certificates []*sidecar.TlsCtCertificate,
	now time.Time,
	networkIdFacts map[server.Id]*model.NetworkScreenFacts,
	reportedNames map[string]bool,
	result *AbuseCtWatchResult,
) error {
	for _, certificate := range certificates {
		result.CertificateCount += 1
		for _, dnsName := range certificate.DnsNames {
			name, err := self.refusedName(ctx, root, dnsName, now, networkIdFacts, result)
			if err != nil {
				return err
			}
			if name == "" || reportedNames[name] {
				continue
			}
			result.RefusedCount += 1
			intakeResult, err := self.deps.accept(ctx, &model.AbuseReport{
				Category:      model.AbuseCategoryBrand,
				ReporterClass: model.AbuseReporterClassCtWatch,
				Url:           name,
			})
			if err != nil {
				abuseScanHitCounter.WithLabelValues(abuseScanCtWatch, "failed").Inc()
				return err
			}
			reportedNames[name] = true
			if intakeResult.Joined {
				result.ReportedCount += 1
				abuseScanHitCounter.WithLabelValues(abuseScanCtWatch, "reported").Inc()
			} else {
				result.UnjoinedCount += 1
				abuseScanHitCounter.WithLabelValues(abuseScanCtWatch, "unjoined").Inc()
			}
		}
	}
	return nil
}

// What a certificate's name reports when the lists refuse it: its label under root, or the whole name for a refused
// host label. "" when the lists allow it or it is no screen's: the root itself, a name under another root, a
// malformed name, or a label no network holds.
func (self *abuseCtWatch) refusedName(
	ctx context.Context,
	root string,
	dnsName string,
	now time.Time,
	networkIdFacts map[server.Id]*model.NetworkScreenFacts,
	result *AbuseCtWatchResult,
) (string, error) {
	name := strings.TrimPrefix(model.AbuseName(dnsName), "*.")
	under, ok := strings.CutSuffix(name, "."+root)
	if !ok || under == "" {
		return "", nil
	}
	labels := strings.Split(under, ".")
	for _, label := range labels {
		if label == "" {
			return "", nil
		}
	}
	result.NameCount += 1
	label := labels[len(labels)-1]
	networkId, ok, err := self.deps.labelNetwork(ctx, root, label)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", nil
	}
	facts, ok := networkIdFacts[networkId]
	if !ok {
		facts = self.deps.screenFacts(ctx, networkId, now)
		networkIdFacts[networkId] = facts
	}
	if sidecar.ScreenName(label, facts.For(label)) != sidecar.ScreenVerdictAllow {
		return label + "." + root, nil
	}
	for _, hostLabel := range labels[:len(labels)-1] {
		if hostLabel == "*" || strings.HasPrefix(hostLabel, "_") {
			continue
		}
		if sidecar.ScreenHost(hostLabel, facts.For(hostLabel)) != sidecar.ScreenVerdictAllow {
			return name, nil
		}
	}
	return "", nil
}

// Schedules the watch's next run one Interval from now with no cursor, once: a pending run keeps its own args. A
// binary that does not link MG5 schedules nothing.
func ScheduleAbuseCtWatch(clientSession *session.ClientSession, tx server.PgTx) {
	if !server.SidecarMg5Linked() {
		return
	}
	scheduleAbuseCtWatch(clientSession, tx, &AbuseCtWatchArgs{}, server.NowUtc().Add(DefaultAbuseCtWatchSettings().Interval))
}

func scheduleAbuseCtWatch(clientSession *session.ClientSession, tx server.PgTx, args *AbuseCtWatchArgs, runAt time.Time) {
	task.ScheduleTaskInTx(
		tx,
		AbuseCtWatch,
		args,
		clientSession,
		task.RunOnce("abuse_ct_watch"),
		task.RunAt(runAt),
		task.MaxTime(DefaultAbuseCtWatchSettings().TaskMaxTime),
	)
}

// One run of the watch from its args' cursors.
func AbuseCtWatch(abuseCtWatchArgs *AbuseCtWatchArgs, clientSession *session.ClientSession) (*AbuseCtWatchResult, error) {
	if !server.SidecarMg5Linked() {
		return &AbuseCtWatchResult{Idle: true, Cursors: abuseCtWatchArgs.Cursors, ListsSha256: abuseCtWatchArgs.ListsSha256}, nil
	}
	watch := &abuseCtWatch{settings: DefaultAbuseCtWatchSettings(), deps: defaultAbuseCtWatchDeps()}
	result := watch.run(clientSession.Ctx, abuseCtWatchArgs)
	if 0 < result.RefusedCount {
		glog.Infof("[abuse]the ct watch found %d names the screen's lists refuse: %d reported\n", result.RefusedCount, result.ReportedCount)
	}
	return result, nil
}

// Schedules the next run with the cursors this one reached: soon with more to read, after RetryInterval for a
// failed run, else after Interval.
func AbuseCtWatchPost(
	abuseCtWatchArgs *AbuseCtWatchArgs,
	abuseCtWatchResult *AbuseCtWatchResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	if !server.SidecarMg5Linked() {
		return nil
	}
	settings := DefaultAbuseCtWatchSettings()
	now := server.NowUtc()
	runAt := now.Add(settings.Interval)
	switch {
	case abuseCtWatchResult.More:
		runAt = now.Add(settings.ContinueDelay)
	case abuseCtWatchResult.Failed:
		runAt = now.Add(settings.RetryInterval)
	}
	scheduleAbuseCtWatch(clientSession, tx, &AbuseCtWatchArgs{
		Cursors:     abuseCtWatchResult.Cursors,
		ListsSha256: abuseCtWatchResult.ListsSha256,
	}, runAt)
	return nil
}
