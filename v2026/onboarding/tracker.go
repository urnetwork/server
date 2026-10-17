package onboarding

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// EmailTrackerWindowDays is the rolling send-cohort window exported to
// Prometheus. Detailed daily rows remain available for a longer bounded range
// through the admin API.
const EmailTrackerWindowDays = 28

// EmailTrackerEngagementWindowDays bounds product engagement attribution after
// a send. EmailTrackerDeliveryCorrectionDays is longer because provider
// delivery webhooks can arrive or be corrected after product attribution has
// closed.
const EmailTrackerEngagementWindowDays = 14
const EmailTrackerDeliveryCorrectionDays = 30

// EmailTrackerRebuildDays includes today. Rebuilding the preceding 30 send
// days as well ensures a correction arriving just inside the 30-day boundary
// can still replace its send-day aggregate; this is intentionally wider than
// the 28-day Prometheus projection.
const EmailTrackerRebuildDays = EmailTrackerDeliveryCorrectionDays + 1

// EmailTrackerMaxRangeDays bounds one research API request.
const EmailTrackerMaxRangeDays = 93

// Email tracker outcomes are a closed metric-label vocabulary. Adding an
// outcome requires its database count, exporter and dashboard panel together.
const (
	EmailOutcomeSent                 = "sent"
	EmailOutcomeDelivered            = "delivered"
	EmailOutcomeOpened               = "opened"
	EmailOutcomeClicked              = "clicked"
	EmailOutcomeLandingClicked       = "landing_clicked"
	EmailOutcomeAppOpened            = "app_opened"
	EmailOutcomeConnected            = "connected"
	EmailOutcomeWidgetAdded          = "widget_added"
	EmailOutcomeFeedbackSubmitted    = "feedback_submitted"
	EmailOutcomeProStarted           = "pro_started"
	EmailOutcomeEngaged              = "engaged"
	EmailOutcomeBounced              = "bounced"
	EmailOutcomeUnsubscribed         = "unsubscribed"
	EmailOutcomeComplained           = "complained"
	EmailOutcomeAttributionAmbiguous = "attribution_ambiguous"
)

var emailTrackerOutcomes = []string{
	EmailOutcomeSent,
	EmailOutcomeDelivered,
	EmailOutcomeOpened,
	EmailOutcomeClicked,
	EmailOutcomeLandingClicked,
	EmailOutcomeAppOpened,
	EmailOutcomeConnected,
	EmailOutcomeWidgetAdded,
	EmailOutcomeFeedbackSubmitted,
	EmailOutcomeProStarted,
	EmailOutcomeEngaged,
	EmailOutcomeBounced,
	EmailOutcomeUnsubscribed,
	EmailOutcomeComplained,
	EmailOutcomeAttributionAmbiguous,
}

func EmailTrackerOutcomes() []string {
	return append([]string(nil), emailTrackerOutcomes...)
}

// EmailTrackerKey is the stable ordering tuple used by the research API's
// keyset cursor. It contains aggregate dimensions only, never an identifier.
type EmailTrackerKey struct {
	SendDay           string `json:"d"`
	Step              string `json:"s"`
	Template          string `json:"t"`
	Variant           string `json:"v"`
	Experiment        string `json:"e"`
	ExperimentVariant string `json:"x"`
	Platform          string `json:"p"`
	Path              string `json:"h"`
}

func EncodeEmailTrackerCursor(key EmailTrackerKey) string {
	body, _ := json.Marshal(key)
	return base64.RawURLEncoding.EncodeToString(body)
}

func DecodeEmailTrackerCursor(cursor string) (EmailTrackerKey, error) {
	var key EmailTrackerKey
	body, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(cursor))
	if err != nil || len(body) == 0 || 1024 < len(body) {
		return key, errors.New("invalid onboarding email tracker cursor")
	}
	if err := json.Unmarshal(body, &key); err != nil || !validEmailTrackerKey(key) {
		return EmailTrackerKey{}, errors.New("invalid onboarding email tracker cursor")
	}
	return key, nil
}

func validEmailTrackerKey(key EmailTrackerKey) bool {
	day, err := time.Parse("2006-01-02", key.SendDay)
	if err != nil || day.Format("2006-01-02") != key.SendDay || !IsFlowStep(key.Step) {
		return false
	}
	for _, field := range []struct {
		value     string
		maxLength int
	}{
		{key.Template, 32}, {key.Variant, 32}, {key.Experiment, 64},
		{key.ExperimentVariant, 64}, {key.Platform, 16}, {key.Path, 8},
	} {
		if field.maxLength < len(field.value) || strings.ContainsRune(field.value, '\x00') {
			return false
		}
	}
	return true
}
