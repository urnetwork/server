// The versioned G2 contract is a standalone instrument dependency. It needs
// only the historical scorer-one constant, not newer control-plane models.
package model

import (
	"encoding/json"
	"errors"
)

const CompetitionMinimumWorkScorerVersion = "sim-latency-score/2"

// Omission is the immutable historical 95–105% request and byte band. The
// explicit version removes both upper bounds, retaining inclusive 95% floors.
type CompetitionG2VolumePolicy string

const (
	CompetitionG2VolumePolicyLegacy      CompetitionG2VolumePolicy = ""
	CompetitionG2VolumePolicyMinimumWork CompetitionG2VolumePolicy = "minimum-work/1"
)

// Legacy is represented by omission, never a newly introduced null/empty
// field. Explicit extensions must name a supported non-legacy policy.
func (self *CompetitionG2VolumePolicy) UnmarshalJSON(content []byte) error {
	var policy string
	if err := json.Unmarshal(content, &policy); err != nil {
		return err
	}
	if CompetitionG2VolumePolicy(policy) != CompetitionG2VolumePolicyMinimumWork {
		return errors.New("explicit g2 volume policy must be minimum-work/1")
	}
	*self = CompetitionG2VolumePolicy(policy)
	return nil
}

// Policy and implementation versions must agree; unknown policy never falls
// back to legacy scoring. The score document schema itself remains unchanged.
func CompetitionScorerVersionForG2Policy(policy CompetitionG2VolumePolicy) (string, error) {
	switch policy {
	case CompetitionG2VolumePolicyLegacy:
		return CompetitionScorerVersion, nil
	case CompetitionG2VolumePolicyMinimumWork:
		return CompetitionMinimumWorkScorerVersion, nil
	default:
		return "", errors.New("unsupported g2 volume policy")
	}
}
