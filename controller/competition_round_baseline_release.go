// Private producer releases retain their own closed dependency inventories.
// A new qualified release never relabels the historical control producer.
package controller

import "errors"

// This names an immutable source release, not an adjustable scoring setting.
const roundBaselineLocalFixturesRelease = "coverage-local-fixtures/1"
const roundBaselineImageBranchesRelease = "coverage-image-branches/1"

// An omitted discriminator preserves every historical dependency byte. The
// explicit new release is usable only with the prospective coverage contract.
func roundBaselinePreparationConfiguredDependencies(config roundBaselinePreparationConfig) (map[string]string, string, error) {
	dependencies, program, err := roundBaselinePreparationPolicyDependencies(config.G2VolumePolicy, config.WorkCoveragePolicy)
	if err != nil || config.ReleaseKind == "" {
		return dependencies, program, err
	}
	if (config.ReleaseKind != roundBaselineLocalFixturesRelease && config.ReleaseKind != roundBaselineImageBranchesRelease) || config.WorkCoveragePolicy == nil || program != "prepare-round-baseline-work-coverage.sh" {
		return nil, "", errors.New("unsupported explicit baseline producer release")
	}
	// All other files retain the original sealed digests. These changed assets
	// are the independently qualified local-fixture source release.
	for name, digest := range map[string]string{
		"evaluator-work-coverage.sh":     "5d2d9bef902944eb6db166628dd8bc8adc57ce668f118efd493319af63aa8802",
		"network-boundary.sh":            "ab08d9d480600c931e058756fc0c961e3ad1cb36df3d3fd7ea5ad063607f3c6d",
		"docker-id-map.sh":               "ecf319fccdd00a2ab76871ec0a29693abca83e3a99ee1ee5c508bc1b076d96a0",
		"resource-boundary.sh":           "2d8fee49fff639f7c44d0e865d299cf020e062f6c1fa35933e30d5fb6710db5a",
		"retain-failure-evidence.sh":     "3aeea5ceaabe938da85e61ed7793ef25a514d91259e25f2664a7f3bbbe778521",
		"build-submission.sh":            "7cd460da98c5014e1acd85072e5de9000390acaeb39dc3ca7a783d531cb1bddd",
		"submission-builder-identity.sh": "97281a179b680778c416f708b2d0abb95810eaa84ed15b87d2644b4467a86c49",
		"prepare-evaluation-source.sh":   "e53d989b3ef2ee2854380768558ed7acf20a35af8a985ec81edbfa63790cdd92",
		"competitionpatch":               "45f20ba5a1272216596159a28a0f4227caae2349150359c3144285309661b9b1",
		"competitionpatch.sha256":        "5495244d334d9aa8fdf5ff8e0d42672fbb21ea78b82b624f069989e7801fab53",
	} {
		dependencies[name] = digest
	}
	if config.ReleaseKind == roundBaselineImageBranchesRelease {
		// Metadata normalization lives only in the throwaway legacy candidate
		// build layer. The control producer, scorer and frozen image are unchanged.
		dependencies["Dockerfile.submission"] = "3cf1e3e7f5ddb036334106bc107468358c6c9800861d11125b4b1db7882f1c5f"
	}
	return dependencies, program, nil
}
