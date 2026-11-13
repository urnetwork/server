// SPDX-License-Identifier: MPL-2.0

package onboarding

// The Brevo tags of a campaign send, onboarding's and re-engagement's. The
// first three are the program, the template and its variant, in the
// positions the webhook reads (brevoWebhookCampaignTemplate). After them come
// the experiment and the arm the network was assigned, when one covers the
// send, so Brevo's aggregated report, which counts a send under each of its
// tags (checked 2026-11-13), can be pulled per experiment and arm. With
// anonymous tracking on, that report is the only place spam reports and
// unsubscribes can be told apart by arm: the webhook's events name no
// recipient. The prefixes keep them apart from the template and variant
// tags, which both programs share ("default").

import (
	"strings"
)

// The prefixes of the experiment and arm tags.
const (
	experimentTagPrefix = "exp-"
	armTagPrefix        = "arm-"
)

// ExperimentTag is the Brevo tag of every send an experiment covers.
func ExperimentTag(experimentId string) string {
	return experimentTagPrefix + experimentId
}

// ArmTag is the Brevo tag of every send to one arm of an experiment.
func ArmTag(experimentId string, arm string) string {
	return armTagPrefix + experimentId + "-" + arm
}

// CampaignTags are a send's tags: program, template, variant, then the
// experiment and arm tags when the send is assigned to both.
func CampaignTags(program string, template string, variant string, experimentId string, arm string) []string {
	tags := []string{program, template, variant}
	experimentId, arm = strings.TrimSpace(experimentId), strings.TrimSpace(arm)
	if experimentId != "" && arm != "" {
		tags = append(tags, ExperimentTag(experimentId), ArmTag(experimentId, arm))
	}
	return tags
}
