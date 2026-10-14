package controller

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// The lag report endpoint (lag.dev/PLAN.md A7, M8): an extender posts what
// its lag.dev sessions measured, one report per session, each the minute
// around the session's load (or its last minute without one), signed by the
// extender's identity key. The extender calls with its activation credential,
// as it does for the ping report, and reports only its own sessions.
//
// A report is stored only when its signature verifies under the identity key
// the operator holds for an extender the calling client activated, its ticket
// was issued for that extender at that track position and has no report yet,
// and its series are inside the bounds below. The browser's address it names
// becomes the track's client fields on the track's first report -- a peppered
// hash, the /24 (/48), the autonomous system and the GeoLite2 City place --
// and is then dropped: no table, log or answer carries it.

// The bounds a report's series must be inside (A7).
const (
	// pings in the minute: 4 a second
	LagReportMaxPings = 600
	// a ping's offset from the window start
	LagReportMaxPingOffsetMs = 60000
	// intervals per direction: 5 s of 250 ms intervals, twice over
	LagReportMaxIntervals = 40
	// the most a direction can carry, which an interval may exceed by the
	// slack, for an interval boundary that fell late
	LagReportMaxBitsPerSecond = 10_000_000_000
	lagReportIntervalSlack    = 1.5
)

// Sanity bounds that keep every stored value inside its column and
// meaningful. A ping is lost after 5 s (M4); a load sits inside its minute.
const (
	lagReportMaxPingMicros = 60_000_000
	lagReportMaxLoadMs     = 2 * LagReportMaxPingOffsetMs
	lagReportMaxIntervalMs = 1000
	lagReportMaxStreams    = 64
)

// The report's times are the extender's clock, which may be well off the
// operator's: an extender admits tickets with a skew allowance and serves
// sessions whatever its offset. So a window is checked against the session
// in the extender's own time, and only the report's arrival against the
// ticket on the operator's clock (lag.dev/PLAN.md A7).
const (
	// how far before the session opened its minute may start: the minute
	// starts 25 s before the download's first byte (M8), which can come
	// within 25 s of the session opening
	lagReportWindowLead = time.Minute
	// how much longer than the time from the ticket's issue to the report's
	// arrival the session may have run before its minute: the extender's and
	// the operator's clocks run at about the same rate
	lagReportSessionSlack = time.Minute
	// how far the extender's clock may be from the operator's before its
	// times mean nothing: only a clock that was never set is that far off
	lagReportMaxClockOffset = 30 * 24 * time.Hour
)

// Why a report was not stored.
const (
	LagReportRejectMalformed = "malformed"
	// the body names an extender the calling client did not activate
	LagReportRejectExtender  = "extender"
	LagReportRejectSignature = "signature"
	// no such ticket, or one for another extender, track or position
	LagReportRejectTicket = "ticket"
	LagReportRejectBounds = "bounds"
	// the ticket already has a report
	LagReportRejectDuplicate = "duplicate"
)

// The posts counted per extender client.
const lagReportRateLimitName = "report"

type ExtenderLagReportArgs struct {
	// each a serialized LagReport, base64
	Reports []string `json:"reports"`
}

type ExtenderLagReportRejection struct {
	// the report's index in the post
	Index  int    `json:"index"`
	Reason string `json:"reason"`
}

type ExtenderLagReportResult struct {
	Accepted int                           `json:"accepted"`
	Rejected []*ExtenderLagReportRejection `json:"rejected"`
}

// A post over the per-client budget. The reporter keeps its queue and retries
// with backoff (M8), so this is an http 429 rather than an answer that would
// read as delivered.
type lagReportLimitError struct {
	retryAfter time.Duration
}

func (self *lagReportLimitError) Error() string {
	return "429 Too many lag reports from this extender. Retry later."
}

func (self *lagReportLimitError) RetryAfterSeconds() int {
	return max(1, int(math.Ceil(self.retryAfter.Seconds())))
}

// Answers `POST /network/extender-lag-report` (A7).
func ExtenderLagReport(
	args *ExtenderLagReportArgs,
	clientSession *session.ClientSession,
) (*ExtenderLagReportResult, error) {
	result, err := extenderLagReport(args, clientSession, server.NowUtc(), GetLagSettings())
	// the post counts by its outcome, and each report in it (X9,
	// lag_metrics.go)
	countLagReports(result, err)
	return result, err
}

// The report as the operator's clock reads now, under settings.
func extenderLagReport(
	args *ExtenderLagReportArgs,
	clientSession *session.ClientSession,
	now time.Time,
	settings *LagSettings,
) (*ExtenderLagReportResult, error) {
	if clientSession.ByJwt == nil || clientSession.ByJwt.ClientId == nil {
		// the route requires a client jwt; this guards a caller that reaches
		// the controller another way
		return nil, fmt.Errorf("401 A lag report requires an extender's client.")
	}
	result := &ExtenderLagReportResult{
		Rejected: []*ExtenderLagReportRejection{},
	}
	if args == nil || len(args.Reports) == 0 {
		return result, nil
	}
	if settings.ReportMaxCount < len(args.Reports) {
		// the reporter batches at most 32 (M8); a larger post is no batch
		return nil, fmt.Errorf("400 A lag report post carries at most %d reports.", settings.ReportMaxCount)
	}
	ctx := clientSession.Ctx
	reporterClientId := *clientSession.ByJwt.ClientId

	limit, err := model.CheckLagClientRateLimit(
		ctx,
		lagReportRateLimitName,
		reporterClientId,
		settings.ReportLimit,
		settings.ReportLimitWindow,
	)
	if err != nil {
		return nil, err
	}
	if !limit.Allowed {
		return nil, &lagReportLimitError{retryAfter: limit.RetryAfter}
	}

	// the extenders the reports name, by hex key, read at most once a post
	extenders := map[string]*model.NetworkExtender{}
	extenderByKey := func(publicKey []byte) *model.NetworkExtender {
		keyHex := fmt.Sprintf("%x", publicKey)
		if extender, ok := extenders[keyHex]; ok {
			return extender
		}
		extender := model.GetNetworkExtenderByPublicKey(ctx, publicKey)
		extenders[keyHex] = extender
		return extender
	}

	for index, reportText := range args.Reports {
		reason := storeLagReport(clientSession, reporterClientId, reportText, now, settings, extenderByKey)
		if reason == "" {
			result.Accepted += 1
		} else {
			result.Rejected = append(result.Rejected, &ExtenderLagReportRejection{
				Index:  index,
				Reason: reason,
			})
		}
	}
	return result, nil
}

// The serialized report of its text: base64, standard or url alphabet, with
// or without padding.
func decodeLagReportText(text string) ([]byte, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, fmt.Errorf("empty report")
	}
	var err error
	for _, encoding := range []*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	} {
		var reportBytes []byte
		reportBytes, err = encoding.DecodeString(text)
		if err == nil {
			return reportBytes, nil
		}
	}
	return nil, err
}

// Verifies, checks and stores one report, and answers why not, or "" when it
// was stored.
func storeLagReport(
	clientSession *session.ClientSession,
	reporterClientId server.Id,
	reportText string,
	now time.Time,
	settings *LagSettings,
	extenderByKey func(publicKey []byte) *model.NetworkExtender,
) (reason string) {
	ctx := clientSession.Ctx

	reportBytes, err := decodeLagReportText(reportText)
	if err != nil {
		return LagReportRejectMalformed
	}
	report := &protocol.LagReport{}
	if err := proto.Unmarshal(reportBytes, report); err != nil {
		return LagReportRejectMalformed
	}
	// The body is read before it is verified only to find the key to verify
	// it under; nothing else of it is used until it verifies, and the
	// verification checks the body names that same key.
	claimed := &protocol.LagReportBody{}
	if err := proto.Unmarshal(report.Body, claimed); err != nil {
		return LagReportRejectMalformed
	}
	if len(claimed.ExtenderPublicKey) != ed25519.PublicKeySize {
		return LagReportRejectMalformed
	}
	extender := extenderByKey(claimed.ExtenderPublicKey)
	if extender == nil || extender.ClientId != reporterClientId {
		// an extender reports only its own sessions
		return LagReportRejectExtender
	}
	body, err := connect.VerifyLagReport(report, ed25519.PublicKey(extender.PublicKey))
	if err != nil {
		return LagReportRejectSignature
	}

	ticketId, err := server.IdFromBytes(body.TicketId)
	if err != nil {
		return LagReportRejectMalformed
	}
	trackId, err := server.IdFromBytes(body.TrackId)
	if err != nil {
		return LagReportRejectMalformed
	}
	ticket := model.GetLagTicket(ctx, ticketId)
	if ticket == nil ||
		ticket.ExtenderId != extender.ExtenderId ||
		ticket.TrackId != trackId ||
		uint32(ticket.Position) != body.TrackPosition {
		return LagReportRejectTicket
	}
	// a track past its retention is gone with everything of it, and a late
	// report does not bring any of it back (A5)
	if model.GetLagTrack(ctx, ticket.TrackId) == nil {
		return LagReportRejectTicket
	}

	// a report names the browser's address family and nothing of its address
	// (amendment 9); the track's client fields were set when it was issued
	if body.IpFamily != 4 && body.IpFamily != 6 {
		return LagReportRejectMalformed
	}

	if !lagReportInBounds(body, ticket, now, settings) {
		return LagReportRejectBounds
	}

	result := newLagResult(body, ticket, now)
	if !model.AddLagResult(ctx, result) {
		return LagReportRejectDuplicate
	}
	return ""
}

// Whether a verified report is inside the bounds (A7): at most 600 pings, each
// offset within the minute, at most 40 intervals per direction, and no
// interval above what 10 Gbps allows plus slack; every value inside its
// column; the minute inside its session in the extender's own time; and the
// report arriving within report_max_age of its ticket's issue on the
// operator's clock.
func lagReportInBounds(body *protocol.LagReportBody, ticket *model.LagTicket, now time.Time, settings *LagSettings) bool {
	if len(body.PingOffsetMs) != len(body.PingMicros) || LagReportMaxPings < len(body.PingOffsetMs) {
		return false
	}
	for i, offsetMs := range body.PingOffsetMs {
		if LagReportMaxPingOffsetMs < offsetMs || lagReportMaxPingMicros < body.PingMicros[i] {
			return false
		}
	}

	// the operator's clock: the report arrives within report_max_age of its
	// ticket's issue
	sinceIssue := now.Sub(ticket.IssuedAt)
	if settings.ReportMaxAge < sinceIssue {
		return false
	}
	// the extender's clock: the minute starts at most a minute before the
	// session opened, and no later than the session could have run, which is
	// from the ticket's issue to the report's arrival
	maxClockMs := uint64(now.Add(lagReportMaxClockOffset).UnixMilli())
	if body.SessionStartTimeMs == 0 || body.WindowStartTimeMs == 0 ||
		maxClockMs < body.SessionStartTimeMs || maxClockMs < body.WindowStartTimeMs {
		return false
	}
	sessionStartTime := time.UnixMilli(int64(body.SessionStartTimeMs))
	windowStartTime := time.UnixMilli(int64(body.WindowStartTimeMs))
	if windowStartTime.Before(sessionStartTime.Add(-lagReportWindowLead)) ||
		max(sinceIssue, 0)+lagReportSessionSlack < windowStartTime.Sub(sessionStartTime) {
		return false
	}
	// a clock that was never set: the times would mean nothing
	if lagReportMaxClockOffset < max(windowStartTime.Sub(now), now.Sub(windowStartTime)) {
		return false
	}

	for _, loadMs := range []uint32{body.DownloadStartMs, body.DownloadMs, body.UploadStartMs, body.UploadMs} {
		if lagReportMaxLoadMs < loadMs {
			return false
		}
	}
	if lagReportMaxStreams < body.DownloadStreams || lagReportMaxStreams < body.UploadStreams {
		return false
	}

	series := [][]uint64{body.DownloadReceivedBytes, body.DownloadSentBytes, body.UploadBytes}
	hasIntervals := false
	for _, values := range series {
		if LagReportMaxIntervals < len(values) {
			return false
		}
		hasIntervals = hasIntervals || 0 < len(values)
	}
	if lagReportMaxIntervalMs < body.IntervalMs || hasIntervals && body.IntervalMs == 0 {
		return false
	}
	maxIntervalBytes := uint64(float64(LagReportMaxBitsPerSecond) / 8 * float64(body.IntervalMs) / 1000 * lagReportIntervalSlack)
	for _, values := range series {
		for _, value := range values {
			if maxIntervalBytes < value {
				return false
			}
		}
	}
	return true
}

// The name an end reason is stored under: the enum name without its prefix,
// in lower case (`complete`, `no_load`), as the control channel spells it
// (M4), or the number for a reason newer than this server.
func lagEndReasonName(endReason protocol.LagEndReason) string {
	if name, ok := protocol.LagEndReason_name[int32(endReason)]; ok {
		return strings.ToLower(strings.TrimPrefix(name, "LAG_END_"))
	}
	return strconv.Itoa(int(endReason))
}

// The p-th percentile of values by nearest rank (S5), or nil for none.
// Sorts values.
func lagNearestRank(values []float64, p float64) *float32 {
	if len(values) == 0 {
		return nil
	}
	slices.Sort(values)
	rank := int(math.Ceil(p / 100 * float64(len(values))))
	index := min(max(rank-1, 0), len(values)-1)
	value := float32(values[index])
	return &value
}

// The intervals of a series that fall inside its load: the first
// ceil(loadMs / intervalMs), or as many as there are. An interval past the
// window, from a buggy or hostile browser, is stored with the series but
// never counted in its statistics (A5).
func lagLoadIntervals(byteCounts []uint64, loadMs uint32, intervalMs uint32) []uint64 {
	if loadMs == 0 || intervalMs == 0 {
		return []uint64{}
	}
	count := int((uint64(loadMs) + uint64(intervalMs) - 1) / uint64(intervalMs))
	return byteCounts[:min(count, len(byteCounts))]
}

// The Mbps of each interval of a series.
func lagIntervalMbps(byteCounts []uint64, intervalMs uint32) []float64 {
	mbps := []float64{}
	if intervalMs == 0 {
		return mbps
	}
	for _, byteCount := range byteCounts {
		mbps = append(mbps, float64(byteCount)*8/(float64(intervalMs)*1000))
	}
	return mbps
}

// The row of a verified, bounded report, with what is derived at insert
// (A5): latency p50/p95 outside the session's own load and during it, over
// the pings that came back; download and upload p50/p95 in Mbps over the
// intervals inside each direction's load, the download from what the
// browser reported receiving; and the lost pings. What depends on the load
// running to its end, the own-load latency and both speeds, is derived only
// for a complete load: a load the browser gave up, or one cut short by an
// error, leaves them null rather than speeds from a fraction of it. The
// latency outside the load stands either way. Of the browser's address the
// row keeps only the family the report names, the family the browser
// reached the extender's name on (N3, amendment 9).
func newLagResult(
	body *protocol.LagReportBody,
	ticket *model.LagTicket,
	now time.Time,
) *model.LagResult {
	result := &model.LagResult{
		LagResultId:      server.NewId(),
		TrackId:          ticket.TrackId,
		TrackPosition:    ticket.Position,
		TicketId:         ticket.TicketId,
		ExtenderId:       ticket.ExtenderId,
		SessionStartTime: time.UnixMilli(int64(body.SessionStartTimeMs)).UTC(),
		WindowStartTime:  time.UnixMilli(int64(body.WindowStartTimeMs)).UTC(),
		PingOffsetMs:     make([]int32, len(body.PingOffsetMs)),
		PingMicros:       make([]int32, len(body.PingMicros)),
		DownloadStartMs:  int(body.DownloadStartMs),
		DownloadMs:       int(body.DownloadMs),
		UploadStartMs:    int(body.UploadStartMs),
		UploadMs:         int(body.UploadMs),
		IntervalMs:       int(body.IntervalMs),
		DownloadStreams:  int(body.DownloadStreams),
		UploadStreams:    int(body.UploadStreams),
		UploadVerified:   body.UploadVerified,
		EndReason:        lagEndReasonName(body.EndReason),
		ExtenderIpFamily: int(body.IpFamily),
		CreatedAt:        now,
	}
	toInt64s := func(values []uint64) []int64 {
		out := make([]int64, len(values))
		for i, value := range values {
			out[i] = int64(value)
		}
		return out
	}
	result.DownloadReceivedBytes = toInt64s(body.DownloadReceivedBytes)
	result.DownloadSentBytes = toInt64s(body.DownloadSentBytes)
	result.UploadBytes = toInt64s(body.UploadBytes)

	// the session's own load: the download window, then the upload window
	inOwnLoad := func(offsetMs uint32) bool {
		if 0 < body.DownloadMs && body.DownloadStartMs <= offsetMs && offsetMs < body.DownloadStartMs+body.DownloadMs {
			return true
		}
		if 0 < body.UploadMs && body.UploadStartMs <= offsetMs && offsetMs < body.UploadStartMs+body.UploadMs {
			return true
		}
		return false
	}
	ownLoadMs := []float64{}
	outsideLoadMs := []float64{}
	for i, offsetMs := range body.PingOffsetMs {
		micros := body.PingMicros[i]
		result.PingOffsetMs[i] = int32(offsetMs)
		result.PingMicros[i] = int32(micros)
		if micros == 0 {
			result.LostCount += 1
			continue
		}
		ms := float64(micros) / 1000
		if inOwnLoad(offsetMs) {
			ownLoadMs = append(ownLoadMs, ms)
		} else {
			outsideLoadMs = append(outsideLoadMs, ms)
		}
	}
	result.OutsideLoadLatencyP50Ms = lagNearestRank(outsideLoadMs, 50)
	result.OutsideLoadLatencyP95Ms = lagNearestRank(outsideLoadMs, 95)

	if body.EndReason != protocol.LagEndReason_LAG_END_COMPLETE {
		return result
	}
	result.OwnLoadLatencyP50Ms = lagNearestRank(ownLoadMs, 50)
	result.OwnLoadLatencyP95Ms = lagNearestRank(ownLoadMs, 95)
	downloadMbps := lagIntervalMbps(lagLoadIntervals(body.DownloadReceivedBytes, body.DownloadMs, body.IntervalMs), body.IntervalMs)
	result.DownloadP50Mbps = lagNearestRank(downloadMbps, 50)
	result.DownloadP95Mbps = lagNearestRank(downloadMbps, 95)
	uploadMbps := lagIntervalMbps(lagLoadIntervals(body.UploadBytes, body.UploadMs, body.IntervalMs), body.IntervalMs)
	result.UploadP50Mbps = lagNearestRank(uploadMbps, 50)
	result.UploadP95Mbps = lagNearestRank(uploadMbps, 95)
	return result
}
