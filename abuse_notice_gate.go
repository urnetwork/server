package server

// The route a role mail's notice reaches api by (owner decisions 530, 612; §21.8, §21.16; QD-1070). The platform
// listener's role-mail intake (server/cli/mxedge, --role platform) checks each DKIM signature of a mail to abuse@ or
// postmaster@ on <root> and posts AbuseNoticeGatePath the recipient's local part, its root and each signature's d=
// and RFC 8601 result: never a body, a subject, a From, a sender or the message, for which the format has no field
// and api refuses any field beyond its own. api's handler (api/handlers/internal_gate_abuse_notice_handlers.go)
// matches them to abuse.yml's notice_senders and files the notice through the controller, so the page leaves the
// process that committed the case, and taskworker's clock pass pages a notice whose page a crash between the two
// left owed. Only the platform listener's class reaches the route (InternalGateCallerMxedgePlatform): its one effect
// is a page at any hour, and it believes the verdicts it is sent.
//
// The answer says whether the mail was a notice and whether the pager took its page. A notice whose page is owed
// answers 200 with paged false, since its case stands. 503 is a mail api could not judge or file now: a listed
// sender's key that did not answer, a store that did not answer, or a binary without MG5. The listener answers that
// 451 4.3.0, so the sender's MTA sends the mail again and no notice is lost to a moment's outage.
//
// AbuseNoticeGateClient is the listener's end. Nothing here logs a bearer, a domain or a root.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The route, under the internal gate's prefix.
const AbuseNoticeGatePath = InternalGatePathPrefix + "abuse-notice"

// RFC 8601's dkim results, the words a signature's verdict is sent as. Only pass verifies; temperror is a key that
// did not answer, which may verify when the sender sends the mail again.
const (
	AbuseNoticeDkimResultPass      = "pass"
	AbuseNoticeDkimResultFail      = "fail"
	AbuseNoticeDkimResultPolicy    = "policy"
	AbuseNoticeDkimResultNeutral   = "neutral"
	AbuseNoticeDkimResultTempError = "temperror"
	AbuseNoticeDkimResultPermError = "permerror"
	AbuseNoticeDkimResultNone      = "none"
)

// Whether result is one of RFC 8601's dkim words.
func AbuseNoticeDkimResultKnown(result string) bool {
	switch result {
	case AbuseNoticeDkimResultPass, AbuseNoticeDkimResultFail, AbuseNoticeDkimResultPolicy, AbuseNoticeDkimResultNeutral,
		AbuseNoticeDkimResultTempError, AbuseNoticeDkimResultPermError, AbuseNoticeDkimResultNone:
		return true
	}
	return false
}

// One DKIM signature of a role mail as the listener judged it.
type AbuseNoticeGateSignature struct {
	// d=, lowercased; empty for a signature that did not parse far enough to name one
	Domain string `json:"domain"`
	// an AbuseNoticeDkimResult word
	Result string `json:"result"`
}

// What the listener posts for one role mail and one of its role recipients. There is no field for a body, a
// subject, a From, a sender or the message.
type AbuseNoticeGateArgs struct {
	// the recipient's local part as the listener took it: abuse or postmaster
	RecipientLocalPart string `json:"recipient_local_part"`
	// the root the role address sits under, <root>, which the notice is about
	Root string `json:"root"`
	// in the order of the mail's header, at most the format's SignatureMaxCount
	Signatures []*AbuseNoticeGateSignature `json:"signatures"`
}

// What api answers.
type AbuseNoticeGateResult struct {
	// the mail was a notice, and its case is open
	Notice bool `json:"notice"`
	// the pager took the notice's page for both rota members; false beside notice is a page the clock pass owes
	Paged bool `json:"paged"`
}

// The format's limits, each the format's value (QD-702). api's handler and the listener's client each carry them,
// so each default lives here alone.
type AbuseNoticeGateFormatSettings struct {
	// The most signatures one request carries: 8, D-MAIL-14's bound on the DKIM signatures a reception checks.
	SignatureMaxCount int
	// The longest recipient local part: RFC 5321's 64 octets.
	LocalPartMaxByteCount int
	// The longest root or d=: RFC 1035's 253 octets.
	DomainMaxByteCount int
}

func DefaultAbuseNoticeGateFormatSettings() *AbuseNoticeGateFormatSettings {
	return &AbuseNoticeGateFormatSettings{
		SignatureMaxCount:     8,
		LocalPartMaxByteCount: 64,
		DomainMaxByteCount:    253,
	}
}

// A request the format does not take. The text names the part, never its value.
var ErrAbuseNoticeGateArgsInvalid = errors.New("abuse notice gate: request invalid")

// Whether a name is printable ASCII with no space, as a local part, a root and a d= all are on this route.
func abuseNoticeGateNameValid(name string, maxByteCount int) bool {
	if name == "" || maxByteCount < len(name) {
		return false
	}
	for index := range len(name) {
		if c := name[index]; c <= ' ' || '~' < c {
			return false
		}
	}
	return true
}

// Whether the format carries domain as a root or a d=: printable ASCII with no space, at most DomainMaxByteCount.
func (self *AbuseNoticeGateFormatSettings) DomainValid(domain string) bool {
	return abuseNoticeGateNameValid(domain, self.DomainMaxByteCount)
}

// Checks the request against the format: a local part and a root, each a bounded name, and at most
// SignatureMaxCount signatures, each a known result and a bounded d= or none.
func (self *AbuseNoticeGateArgs) Validate(settings *AbuseNoticeGateFormatSettings) error {
	switch {
	case !abuseNoticeGateNameValid(self.RecipientLocalPart, settings.LocalPartMaxByteCount):
		return fmt.Errorf("%w: the recipient's local part", ErrAbuseNoticeGateArgsInvalid)
	case !settings.DomainValid(self.Root):
		return fmt.Errorf("%w: the root", ErrAbuseNoticeGateArgsInvalid)
	case settings.SignatureMaxCount < len(self.Signatures):
		return fmt.Errorf("%w: more signatures than the format's bound", ErrAbuseNoticeGateArgsInvalid)
	}
	for _, signature := range self.Signatures {
		switch {
		case signature == nil:
			return fmt.Errorf("%w: a signature with nothing in it", ErrAbuseNoticeGateArgsInvalid)
		case !AbuseNoticeDkimResultKnown(signature.Result):
			return fmt.Errorf("%w: a signature's result is no RFC 8601 dkim result", ErrAbuseNoticeGateArgsInvalid)
		case signature.Domain != "" && !settings.DomainValid(signature.Domain):
			return fmt.Errorf("%w: a signature's domain", ErrAbuseNoticeGateArgsInvalid)
		}
	}
	return nil
}

// Tunables of the listener's client.
type AbuseNoticeGateClientSettings struct {
	// The longest one call may take: 60 s, past api's wait for the pager to take a page (model.AbuseSettings'
	// NoticePageTimeout, 15 s) and its store, so the client never gives up on a filing api is still finishing.
	RequestTimeout time.Duration
	// The most bytes of an answer read: 4 KiB, {notice, paged} many times over.
	AnswerMaxByteCount int64
	// nil: DefaultAbuseNoticeGateFormatSettings.
	Format *AbuseNoticeGateFormatSettings
}

func DefaultAbuseNoticeGateClientSettings() *AbuseNoticeGateClientSettings {
	return &AbuseNoticeGateClientSettings{
		RequestTimeout:     60 * time.Second,
		AnswerMaxByteCount: 4 * 1024,
		Format:             DefaultAbuseNoticeGateFormatSettings(),
	}
}

// What the client is made from.
type AbuseNoticeGateClientArgs struct {
	// api's base URL, http or https, with no final slash.
	ApiUrl string
	// The platform listener's bearer, LoadInternalGateCredential(InternalGateCallerMxedgePlatform). A secret: never
	// logged.
	Bearer string
	// nil: DefaultAbuseNoticeGateClientSettings.
	Settings *AbuseNoticeGateClientSettings
	// nil: a client of its own, which follows no redirect.
	HttpClient *http.Client
}

// A call that was not answered as a notice's: api away, a refusal, an answer that does not read. The text names
// the part, never the bearer.
var ErrAbuseNoticeGate = errors.New("abuse notice gate")

// The listener's end of the route. Safe for concurrent use: it holds no state between calls.
type AbuseNoticeGateClient struct {
	routeUrl   string
	bearer     string
	settings   *AbuseNoticeGateClientSettings
	format     *AbuseNoticeGateFormatSettings
	httpClient *http.Client
}

// A client of args. An error names the part that cannot be used, never the bearer.
func NewAbuseNoticeGateClient(args *AbuseNoticeGateClientArgs) (*AbuseNoticeGateClient, error) {
	if args == nil {
		return nil, fmt.Errorf("%w: no args", ErrAbuseNoticeGate)
	}
	settings := args.Settings
	if settings == nil {
		settings = DefaultAbuseNoticeGateClientSettings()
	}
	format := settings.Format
	if format == nil {
		format = DefaultAbuseNoticeGateFormatSettings()
	}
	switch {
	case settings.RequestTimeout <= 0 || settings.AnswerMaxByteCount <= 0:
		return nil, fmt.Errorf("%w: settings", ErrAbuseNoticeGate)
	case args.Bearer == "":
		return nil, fmt.Errorf("%w: no bearer", ErrAbuseNoticeGate)
	}
	apiUrl, err := url.Parse(args.ApiUrl)
	if err != nil || (apiUrl.Scheme != "http" && apiUrl.Scheme != "https") || apiUrl.Host == "" || strings.HasSuffix(args.ApiUrl, "/") {
		return nil, fmt.Errorf("%w: api url", ErrAbuseNoticeGate)
	}
	httpClient := args.HttpClient
	if httpClient == nil {
		httpClient = &http.Client{
			Transport: &http.Transport{},
			// a redirect is answered as the refusal it is, so the bearer goes to api's own url alone
			CheckRedirect: func(request *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	return &AbuseNoticeGateClient{
		routeUrl:   args.ApiUrl + AbuseNoticeGatePath,
		bearer:     args.Bearer,
		settings:   settings,
		format:     format,
		httpClient: httpClient,
	}, nil
}

// The format the client sends in.
func (self *AbuseNoticeGateClient) Format() *AbuseNoticeGateFormatSettings {
	return self.format
}

// Posts one role mail's facts. An error is a request the format does not take, or a call api did not answer as a
// notice's (ErrAbuseNoticeGate), after which the listener asks the sender to send the mail again.
func (self *AbuseNoticeGateClient) FileNotice(ctx context.Context, args *AbuseNoticeGateArgs) (*AbuseNoticeGateResult, error) {
	if err := args.Validate(self.format); err != nil {
		return nil, err
	}
	body, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, self.settings.RequestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, self.routeUrl, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%w: the request could not be made", ErrAbuseNoticeGate)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+self.bearer)
	response, err := self.httpClient.Do(request)
	if err != nil {
		// a transport error can quote the url, which holds no secret, but nothing of the request's headers or body
		return nil, fmt.Errorf("%w: api did not answer", ErrAbuseNoticeGate)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: api answered %d", ErrAbuseNoticeGate, response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, self.settings.AnswerMaxByteCount+1))
	if err != nil || self.settings.AnswerMaxByteCount < int64(len(data)) {
		return nil, fmt.Errorf("%w: api's answer could not be read", ErrAbuseNoticeGate)
	}
	result := &AbuseNoticeGateResult{}
	if err := json.Unmarshal(data, result); err != nil {
		return nil, fmt.Errorf("%w: api's answer is not a notice's", ErrAbuseNoticeGate)
	}
	if result.Paged && !result.Notice {
		return nil, fmt.Errorf("%w: api's answer paged no notice", ErrAbuseNoticeGate)
	}
	return result, nil
}
