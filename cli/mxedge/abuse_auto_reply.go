package main

// §21.8's pointer for role mail the listener drops unread (ABUSE-M5-09;
// D-MAIL-10, plan-ABUSE-auto-reply, QD-142). The owner's answer is to send
// nothing: for mail to abuse@ and postmaster@ that §14.8's header-only parse
// cannot attribute, the platform listener answers at end of DATA
//
//	550 5.7.1 Not read. Report abuse at <form url>
//
// and the sender's own MTA returns that text to the person as a bounce. So
// there is no reply to compose, no credential on the TCP 25 block, no
// backscatter, and nothing to rate-limit: no null sender or Auto-Submitted
// message can be answered, because no message is ever sent, and no sender
// hash is kept, because nothing reads MAIL FROM here. The envelope stays in
// the listener's memory (mx-4): the intake this block hands it takes the
// recipient, the arrival time and the bytes, and never a sender.
//
// What is left is which url the refusal names. Two files can hold it:
// abuse.yml's form_url, which is §21's, and mxedge.yml's platform_abuse_url,
// which the listener reads. AbuseAutoReply settles them into one, so the bounce
// a person gets and the form §21 serves cannot drift apart: a file that leaves
// its key out takes the other's, and two that disagree are refused, which
// leaves the block without an intake, so every recipient gets 451 4.3.0 and the
// sender retries, rather than a person being pointed somewhere §21 does not
// serve. A url that cannot go in a reply, one with a space or a line break or
// one too long, is the listener's to refuse (MxEdgeSettings.ValidatePlatform),
// as it is for any mxedge.yml.

import (
	"errors"
	"strings"

	"github.com/urnetwork/sidecar/mxedgeservice"
)

// The two files disagree on the url, or neither names one. The error names the
// keys and never a value.
var ErrAbuseAutoReplyRefused = errors.New("abuse auto-reply refused")

// The refusal's pointer. It holds the url and nothing else: no sender, no
// store and no clock, since nothing is sent.
type AbuseAutoReply struct {
	// §21.8's form, which the 550 5.7.1 names.
	FormUrl string
}

// The pointer for mxedge.yml's settings and abuse.yml's form_url
// (server.GetAbuseConfig().FormUrl(), "" when the file sets none).
func NewAbuseAutoReply(mxEdgeSettings *mxedgeservice.MxEdgeSettings, formUrl string) (*AbuseAutoReply, error) {
	if mxEdgeSettings == nil {
		return nil, errors.Join(ErrAbuseAutoReplyRefused, errors.New("no mxedge.yml"))
	}
	formUrl = strings.TrimSpace(formUrl)
	platformAbuseUrl := strings.TrimSpace(mxEdgeSettings.PlatformAbuseUrl)
	switch {
	case formUrl == "" && platformAbuseUrl == "":
		return nil, errors.Join(ErrAbuseAutoReplyRefused, errors.New("neither abuse.yml's form_url nor mxedge.yml's platform_abuse_url is set"))
	case formUrl == "":
		return &AbuseAutoReply{FormUrl: platformAbuseUrl}, nil
	case platformAbuseUrl == "" || platformAbuseUrl == formUrl:
		return &AbuseAutoReply{FormUrl: formUrl}, nil
	}
	return nil, errors.Join(ErrAbuseAutoReplyRefused, errors.New("abuse.yml's form_url is not mxedge.yml's platform_abuse_url"))
}

// Sets the url the listener's refusal names. Call it before the settings reach
// the listener, which reads them once.
func (self *AbuseAutoReply) Apply(mxEdgeSettings *mxedgeservice.MxEdgeSettings) {
	mxEdgeSettings.PlatformAbuseUrl = self.FormUrl
}

// The block's report intake with the refusal's pointer settled first: formUrl is abuse.yml's form_url. An
// error leaves the block without an intake and mxedge.yml's settings as they were.
func newReportIntakeWithAutoReply(mxEdgeSettings *mxedgeservice.MxEdgeSettings, formUrl string) (*platformReportIntake, error) {
	reply, err := NewAbuseAutoReply(mxEdgeSettings, formUrl)
	if err != nil {
		return nil, err
	}
	reply.Apply(mxEdgeSettings)
	return newConfiguredReportIntake(mxEdgeSettings)
}
