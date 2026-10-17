package controller

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// A track's tickets (lag.dev/PLAN.md A6, M0, M3). A track is the 8 dots
// lag.dev measures together, with up to 4 spares; every session of the track
// is admitted by its own ticket, and all of them come from one call so the
// track shares one id. A ticket admits one session on one extender, at its
// measurement name, whichever address family the browser reaches it on (N3):
// it must open within ticket_ttl, and once open it pings for as long as the
// browser keeps it, with at most one load under the ticket's caps. The
// extender enforces its own session and load limits (M3); the limits here
// are per authenticated client and per /24 (/48) of the caller, on tracks.
// A claimed agent browser or a hosting network is conservatively flagged
// automated and kept out of every aggregate (lag_automated.go).
//
// A dot whose session dropped reconnects within its track (L4): the same call
// with the track's id and the dot's extender and position reissues a ticket
// with the same track and position, so the new session's report lines up
// with the rest of the track (A7). A reissue is not a track and does not
// count toward the track limits. A track gets one reissued ticket per
// position, as a dot reconnects once, and at most track_reissue_max in all,
// and only while it is younger than track_reissue_window; that caps how many
// valid tickets a client can mint for an extender of its choosing.

// The tracks counted per authenticated client and per /24 (/48).
const lagTrackRateLimitName = "track"

// The header the lagdev command line and MCP server name themselves with,
// `cli` or `mcp`. A browser sends none and needs none: no header is web, as
// is any other value. It is not among the API's allowed CORS headers, so a
// page cannot set it.
const lagClientHeader = "X-Lag-Client"

// One extender a track asks for, in track order: the dots, then the spares.
type LagTrackExtenderArgs struct {
	ExtenderId server.Id `json:"extender_id"`
	// a reissue's position of the dot in its track; ignored for a new track,
	// whose positions are the request order
	Position *int `json:"position,omitempty"`
}

type LagTrackArgs struct {
	// set to reissue tickets in a track the caller was issued
	TrackId   *server.Id              `json:"track_id,omitempty"`
	Extenders []*LagTrackExtenderArgs `json:"extenders"`
}

// One admitted session.
type LagTrackTicket struct {
	ExtenderId server.Id `json:"extender_id"`
	// the extender's measurement name (N1), which the session connects to
	Host string `json:"host"`
	// the dot's place in the track: 0-7 for the dots, 8-11 for the spares
	Position int `json:"position"`
	// the ticket text (M4): base64url, unpadded, of the signed LagTicket
	Ticket string `json:"ticket"`
}

// The track's id and its tickets. A ticket whose extender does not serve the
// measurement api now is left out, so a position can be missing.
type LagTrackResult struct {
	TrackId *server.Id        `json:"track_id,omitempty"`
	Tickets []*LagTrackTicket `json:"tickets,omitempty"`
	// RFC 3339, whole seconds, utc: every session must open before this
	ExpiresAt string    `json:"expires_at,omitempty"`
	Error     *LagError `json:"error,omitempty"`
}

// Answers `POST /lag/track` (A6).
func LagTrack(args *LagTrackArgs, clientSession *session.ClientSession) (*LagTrackResult, error) {
	result, err := lagTrack(args, clientSession, server.NowUtc(), GetLagSettings())
	// every answer counts by its outcome, a reissue apart from a new track
	// (X9, lag_metrics.go)
	countLagTrack(args != nil && args.TrackId != nil, result, err)
	return result, err
}

func lagTrackLimited(limit model.LagRateLimitResult, message string) *LagTrackResult {
	return &LagTrackResult{
		Error: &LagError{
			Code:         LagErrorCodeLimit,
			Message:      message,
			RetryAfterMs: limit.RetryAfter.Milliseconds(),
		},
	}
}

func lagTrackUnavailable(message string) *LagTrackResult {
	return &LagTrackResult{
		Error: &LagError{
			Code:    LagErrorCodeUnavailable,
			Message: message,
		},
	}
}

// One dot to sign a ticket for.
type lagTrackDot struct {
	extender *lagServingExtender
	position int
}

// What signs a track's tickets: the operator's root key and network host.
type lagTicketSigner struct {
	rootPrivateKey ed25519.PrivateKey
	networkHost    string
}

// The signer of the configured extender network, or a refusal when tickets
// cannot be signed now.
func newLagTicketSigner() (*lagTicketSigner, *LagTrackResult) {
	config, err := EnvExtenderConfig()
	if err != nil {
		return nil, lagTrackUnavailable("The extender network is not configured.")
	}
	rootPrivateKey, err := config.RootPrivateKey()
	if err != nil || strings.TrimSpace(config.NetworkHost) == "" {
		glog.Errorf("[lag]no tickets can be signed: %v\n", err)
		return nil, lagTrackUnavailable("Tickets cannot be issued now.")
	}
	return &lagTicketSigner{
		rootPrivateKey: rootPrivateKey,
		networkHost:    config.NetworkHost,
	}, nil
}

// Signs one ticket per dot in the track, under the root key that signs
// records (M0), and answers the tickets to store with the answer to give.
func (self *lagTicketSigner) sign(
	clientSession *session.ClientSession,
	trackId server.Id,
	dots []*lagTrackDot,
	now time.Time,
	settings *LagSettings,
) ([]*model.LagTicket, *LagTrackResult, error) {
	clientId := *clientSession.ByJwt.ClientId
	// whole seconds, so the expiry answered is the one signed
	expiresAt := now.Add(settings.TicketTtl).Truncate(time.Second)
	tickets := []*model.LagTicket{}
	result := &LagTrackResult{
		TrackId:   &trackId,
		Tickets:   []*LagTrackTicket{},
		ExpiresAt: expiresAt.UTC().Format(time.RFC3339),
	}
	for _, dot := range dots {
		ticketId := server.NewId()
		ticket, err := connect.SignLagTicket(&protocol.LagTicketBody{
			TicketId:          ticketId.Bytes(),
			ClientId:          clientId.Bytes(),
			ExtenderPublicKey: dot.extender.PublicKey,
			NetworkHost:       self.networkHost,
			IssueTimeMs:       uint64(now.UnixMilli()),
			ExpireTimeMs:      uint64(expiresAt.UnixMilli()),
			TrackId:           trackId.Bytes(),
			TrackPosition:     uint32(dot.position),
			PingIntervalMs:    uint32(settings.PingIntervalMs),
			DownloadMs:        uint32(settings.DownloadMs),
			UploadMs:          uint32(settings.UploadMs),
			MaxStreams:        uint32(settings.MaxStreams),
			MaxDownloadBytes:  uint64(settings.MaxDownloadBytes),
			MaxUploadBytes:    uint64(settings.MaxUploadBytes),
		}, self.rootPrivateKey)
		if err != nil {
			return nil, nil, err
		}
		ticketText, err := connect.EncodeLagTicket(ticket)
		if err != nil {
			return nil, nil, err
		}
		tickets = append(tickets, &model.LagTicket{
			TicketId:   ticketId,
			TrackId:    trackId,
			Position:   dot.position,
			ClientId:   clientId,
			NetworkId:  clientSession.ByJwt.NetworkId,
			ExtenderId: dot.extender.ExtenderId,
			IssuedAt:   now,
			ExpiresAt:  expiresAt,
		})
		result.Tickets = append(result.Tickets, &LagTrackTicket{
			ExtenderId: dot.extender.ExtenderId,
			Host:       dot.extender.Name,
			Position:   dot.position,
			Ticket:     ticketText,
		})
	}
	return tickets, result, nil
}

// The track as the clock reads now, under settings.
func lagTrack(
	args *LagTrackArgs,
	clientSession *session.ClientSession,
	now time.Time,
	settings *LagSettings,
) (*LagTrackResult, error) {
	if clientSession.ByJwt == nil || clientSession.ByJwt.ClientId == nil {
		// the route requires a client jwt; this guards a caller that reaches
		// the controller another way
		return nil, fmt.Errorf("401 A track requires a client.")
	}
	if args == nil || len(args.Extenders) == 0 {
		return nil, fmt.Errorf("400 A track names at least one extender.")
	}
	if settings.TrackMaxExtenders < len(args.Extenders) {
		return nil, fmt.Errorf("400 A track names at most %d extenders.", settings.TrackMaxExtenders)
	}
	if args.TrackId != nil {
		return lagTrackReissue(args, clientSession, now, settings)
	}
	ctx := clientSession.Ctx
	clientId := *clientSession.ByJwt.ClientId

	clientLimit, err := model.CheckLagClientRateLimit(
		ctx,
		lagTrackRateLimitName,
		clientId,
		settings.TrackClientLimit,
		settings.TrackLimitWindow,
	)
	if err != nil {
		return nil, err
	}
	if !clientLimit.Allowed {
		return lagTrackLimited(clientLimit, "Too many tracks from this session. Try again later."), nil
	}
	addr, _, err := clientSession.ParseClientIpPort()
	if err != nil {
		// the parse error would quote the address, so it goes no further
		// (amendment 9)
		return nil, errors.New("could not read the caller address")
	}
	prefixLimit, err := model.CheckLagPrefixRateLimit(
		ctx,
		lagTrackRateLimitName,
		addr,
		settings.TrackPrefixLimit,
		settings.TrackLimitWindow,
	)
	if err != nil {
		return nil, err
	}
	if !prefixLimit.Allowed {
		return lagTrackLimited(
			prefixLimit,
			"Too many tracks from your network. The limit is shared by everyone on it, so this may not be your own activity.",
		), nil
	}
	header := http.Header(clientSession.Header)
	// Signature-Agent is an unverified caller claim, not an authenticated
	// identity. It may conservatively flag automation below, but must neither
	// spend another agent's quota nor grant relief from the caller's limits.

	signer, refusal := newLagTicketSigner()
	if refusal != nil {
		return refusal, nil
	}

	set := getLagServing(ctx, now, settings.ExtenderSampleCache)
	dots := []*lagTrackDot{}
	issued := map[server.Id]bool{}
	for position, extenderArgs := range args.Extenders {
		if extenderArgs == nil {
			continue
		}
		extender := set.get(extenderArgs.ExtenderId)
		if extender == nil {
			// the extender does not serve the api now
			continue
		}
		if issued[extender.ExtenderId] {
			// one session per extender in a track
			continue
		}
		issued[extender.ExtenderId] = true
		dots = append(dots, &lagTrackDot{extender: extender, position: position})
	}
	dots = lagDotsStillServing(ctx, dots, now)
	if len(dots) == 0 {
		return lagTrackUnavailable("None of the extenders can be measured now."), nil
	}

	trackId := server.NewId()
	tickets, result, err := signer.sign(clientSession, trackId, dots, now, settings)
	if err != nil {
		return nil, err
	}
	// the address goes no further than these lookups (amendment 9)
	client := newLagTrackClient(addr)
	model.CreateLagTrack(
		ctx,
		&model.LagTrack{
			TrackId:         trackId,
			NetworkId:       clientSession.ByJwt.NetworkId,
			ClientId:        clientId,
			StartedAt:       now,
			Client:          client,
			AutomatedReason: lagTrackAutomatedReason(header, client.Asn),
			Source:          lagTrackSource(header),
		},
		tickets,
	)
	return result, nil
}

// The client that ran a track, from its X-Lag-Client header: cli or mcp when
// it names one of them, otherwise web.
func lagTrackSource(header http.Header) string {
	switch strings.ToLower(strings.TrimSpace(header.Get(lagClientHeader))) {
	case model.LagSourceCli:
		return model.LagSourceCli
	case model.LagSourceMcp:
		return model.LagSourceMcp
	default:
		return model.LagSourceWeb
	}
}

// The track's client fields (A5): the autonomous system and the city-level
// place of the address the track is issued to, looked up in memory, with the
// city's own point when the place names a city (lagClientPlace); a place
// resolved only to a region or country has none. The address itself, a
// prefix of it, a hash of it or its block's coordinates are never kept
// (amendment 9). A lookup that cannot answer leaves its fields empty.
func newLagTrackClient(addr netip.Addr) *model.LagTrackClient {
	addr = addr.Unmap().WithZone("")
	client := &model.LagTrackClient{}
	if asn := GetIpAsn(addr); asn != nil {
		number := int64(asn.Number)
		client.Asn = &number
		client.AsnOrg = asn.Organization
	}
	if place := lagClientPlace(addr); place != nil {
		client.CountryCode = place.CountryCode
		client.Region = place.Region
		client.City = place.City
		client.Latitude = place.Latitude
		client.Longitude = place.Longitude
	}
	return client
}

// Reissues tickets in a track the caller was issued within the reissue
// window, for dots the track already holds (A6, L4). An unknown track,
// another client's, one past the window, or a dot the track does not hold is
// unavailable; a dot already reissued, or a track out of reissues, is
// limited. A dot whose extender no longer serves the api is left out, as in
// a new track.
func lagTrackReissue(
	args *LagTrackArgs,
	clientSession *session.ClientSession,
	now time.Time,
	settings *LagSettings,
) (*LagTrackResult, error) {
	ctx := clientSession.Ctx
	clientId := *clientSession.ByJwt.ClientId
	trackId := *args.TrackId
	for _, extenderArgs := range args.Extenders {
		if extenderArgs == nil || extenderArgs.Position == nil {
			return nil, fmt.Errorf("400 A reissue names each extender with its position in the track.")
		}
	}

	track := model.GetLagTrack(ctx, trackId)
	if track == nil || track.ClientId != clientId || track.StartedAt.Before(now.Add(-settings.TrackReissueWindow)) {
		return lagTrackUnavailable("The track cannot be reissued."), nil
	}
	held := map[lagTrackDotKey]bool{}
	for _, ticket := range model.GetLagTrackTickets(ctx, trackId) {
		held[lagTrackDotKey{extenderId: ticket.ExtenderId, position: ticket.Position}] = true
	}

	signer, refusal := newLagTicketSigner()
	if refusal != nil {
		return refusal, nil
	}
	set := getLagServing(ctx, now, settings.ExtenderSampleCache)
	dots := []*lagTrackDot{}
	reissued := map[lagTrackDotKey]bool{}
	for _, extenderArgs := range args.Extenders {
		key := lagTrackDotKey{extenderId: extenderArgs.ExtenderId, position: *extenderArgs.Position}
		if !held[key] {
			return lagTrackUnavailable("The track holds no such dot."), nil
		}
		if reissued[key] {
			continue
		}
		reissued[key] = true
		extender := set.get(key.extenderId)
		if extender == nil {
			// the extender does not serve the api now
			continue
		}
		dots = append(dots, &lagTrackDot{extender: extender, position: key.position})
	}
	dots = lagDotsStillServing(ctx, dots, now)
	if len(dots) == 0 {
		return lagTrackUnavailable("None of the extenders can be measured now."), nil
	}

	tickets, result, err := signer.sign(clientSession, trackId, dots, now, settings)
	if err != nil {
		return nil, err
	}
	if !model.AddLagTrackReissue(ctx, trackId, tickets, settings.TrackReissueMax) {
		return &LagTrackResult{
			Error: &LagError{
				Code:    LagErrorCodeLimit,
				Message: "A dot of the track has reconnected already, or the track has been reissued as often as it can be.",
			},
		}, nil
	}
	return result, nil
}

// The dots whose extender still serves, read at `now` (lagStillServing), so
// a withdrawal or an opt-out within the serving set's cache gets no ticket,
// and nor does an extender yet to read its certificate.
func lagDotsStillServing(ctx context.Context, dots []*lagTrackDot, now time.Time) []*lagTrackDot {
	extenders := make([]*lagServingExtender, 0, len(dots))
	for _, dot := range dots {
		extenders = append(extenders, dot.extender)
	}
	serving := map[server.Id]bool{}
	for _, extender := range lagStillServing(ctx, extenders, now) {
		serving[extender.ExtenderId] = true
	}
	kept := []*lagTrackDot{}
	for _, dot := range dots {
		if serving[dot.extender.ExtenderId] {
			kept = append(kept, dot)
		}
	}
	return kept
}

// A dot of a track: an extender at a position.
type lagTrackDotKey struct {
	extenderId server.Id
	position   int
}
