package onboarding

import (
	"strings"
	"testing"

	"github.com/urnetwork/connect/v2026"
)

func TestEmailTrackerVocabularyAndCursor(t *testing.T) {
	connect.AssertEqual(t, []string{StepE1, StepE2, StepE3, StepE4, StepE5}, FlowSteps())
	connect.AssertEqual(t, 15, len(EmailTrackerOutcomes()))

	key := EmailTrackerKey{
		SendDay: "2026-01-02", Step: StepE3, Template: TemplateE3LastChance,
		Variant: VariantEngaged, Experiment: "synthetic_experiment",
		ExperimentVariant: "synthetic_variant", Platform: "ios", Path: PathA,
	}
	cursor := EncodeEmailTrackerCursor(key)
	decoded, err := DecodeEmailTrackerCursor(cursor)
	connect.AssertEqual(t, nil, err)
	connect.AssertEqual(t, key, decoded)

	for _, invalid := range []string{
		"",
		"not-base64",
		EncodeEmailTrackerCursor(EmailTrackerKey{SendDay: key.SendDay, Step: "e99"}),
		EncodeEmailTrackerCursor(EmailTrackerKey{SendDay: "not-a-day", Step: StepE1}),
		EncodeEmailTrackerCursor(EmailTrackerKey{SendDay: key.SendDay, Step: StepE1, Template: strings.Repeat("x", 33)}),
	} {
		_, err := DecodeEmailTrackerCursor(invalid)
		connect.AssertEqual(t, true, err != nil)
	}
}
