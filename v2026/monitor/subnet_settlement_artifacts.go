// §22.7 payout root reproduction. Every committed root names an artifact by
// content hash; the artifact's verified canonical body commits to its root,
// domain and coordinates, so a verified artifact whose root, deployment,
// chain, policy, epoch or operator differs from the chain is a definitive
// counterexample. Unavailable or unverifiable bytes leave the root unknown;
// serving integrity across origins is the §22.8 reader's recurring check.
package monitor

import (
	"context"
	"encoding/hex"
	"net/url"
	"sort"
	"strings"
	"sync"

	"github.com/urfoundation/sn/v2026/payoutartifact"
)

// Content hashes this watcher already reproduced, with the root each
// reproduced. Each artifact is fetched once per hash; availability is
// rechecked by §22.8. Safe for concurrent use.
type subnetSettlementVerified struct {
	stateLock sync.Mutex
	roots     map[[32]byte][32]byte
}

func newSubnetSettlementVerified() *subnetSettlementVerified {
	return &subnetSettlementVerified{roots: map[[32]byte][32]byte{}}
}

// True when the hash already reproduced exactly this root.
func (self *subnetSettlementVerified) Known(artifactHash, root [32]byte) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	known, ok := self.roots[artifactHash]
	return ok && known == root
}

// Records one reproduced artifact.
func (self *subnetSettlementVerified) Record(artifactHash, root [32]byte) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.roots[artifactHash] = root
}

// One committed root to reproduce.
type subnetSettlementCommitted struct {
	entry        *subnetSettlementEntry
	artifactHash [32]byte
	root         [32]byte
}

// Reproduces the newest committed roots that are not yet verified. Returns
// findings and the frames of roots left unknown.
func reproduceSubnetSettlementArtifacts(ctx context.Context, client grafanaHTTPClient, origins []string, pin *SubnetReplayPin, state *subnetSettlementState, verified *subnetSettlementVerified, settings *SubnetReplaySettings, scope subnetReplayScope) ([]finding, error) {
	committed := []subnetSettlementCommitted{}
	for _, entry := range state.entries {
		switch {
		case entry.entitlement.Status == subnetSettlementFinalized || entry.entitlement.Status == subnetSettlementCarried:
			committed = append(committed, subnetSettlementCommitted{entry: entry, artifactHash: entry.entitlement.ArtifactHash, root: entry.entitlement.PayoutRoot})
		case entry.root.PayoutRoot != [32]byte{}:
			committed = append(committed, subnetSettlementCommitted{entry: entry, artifactHash: entry.root.ArtifactHash, root: entry.root.PayoutRoot})
		}
	}
	sort.SliceStable(committed, func(i, j int) bool { return committed[j].entry.epoch < committed[i].entry.epoch })
	findings := []finding{}
	reads := 0
	for _, candidate := range committed {
		if candidate.artifactHash == [32]byte{} || candidate.root == [32]byte{} || verified.Known(candidate.artifactHash, candidate.root) {
			continue
		}
		target := subnetSettlementOperatorTarget(candidate.entry.noId)
		frame := subnetSettlementEpochFrame(candidate.entry.epoch, "artifact-reproduction")
		if settings.ArtifactMaximumReads <= reads {
			findings = append(findings, subnetReplayUndecided(subnetSettlementProbeId, "subnet-payout-root", target, frame, scope, subnetSettlementPlaybook, "artifact-read-bound", 2))
			continue
		}
		reads += 1
		contentHash := "sha256:" + hex.EncodeToString(candidate.artifactHash[:])
		reason := "artifact-unavailable"
		var artifact *payoutartifact.Artifact
		for _, origin := range origins {
			response, err := readSubnetReplayApi(ctx, client, origin, "/sn/artifact", url.Values{"hash": []string{contentHash}}, settings)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			default:
			}
			if err != nil || response.status != 200 {
				continue
			}
			if response.contentType != "application/json" || response.etag != `"`+contentHash+`"` {
				reason = "artifact-contract-invalid"
				continue
			}
			decoded, err := payoutartifact.DecodeWithContext(ctx, response.body)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			default:
			}
			if err != nil || decoded.ContentHash != contentHash {
				reason = "artifact-unverifiable"
				continue
			}
			artifact = decoded
			break
		}
		if artifact == nil {
			findings = append(findings, subnetReplayUndecided(subnetSettlementProbeId, "subnet-payout-root", target, frame, scope, subnetSettlementPlaybook, reason, 2))
			continue
		}
		mismatch := ""
		policyHash := "0x" + hex.EncodeToString(candidate.entry.clock.policy.PolicyHash[:])
		switch {
		case artifact.PayoutRoot != candidate.root:
			mismatch = "payout-root"
		case artifact.DeploymentID != pin.deploymentId || artifact.ChainID != pin.chainId || artifact.Netuid != pin.netuid || !strings.EqualFold(artifact.GenesisHash, pin.genesisHash.Hex()):
			mismatch = "deployment-domain"
		case artifact.Coordinator != pin.coordinator || artifact.SettlementVault != pin.settlementVault:
			mismatch = "contract-domain"
		case artifact.Epoch != candidate.entry.epoch || artifact.NoID != candidate.entry.noId:
			mismatch = "coordinates"
		case !strings.EqualFold(artifact.PolicyHash, policyHash):
			mismatch = "policy"
		case len(artifact.Leaves) != 0 && artifact.SharesTotalBPS != subnetSettlementBps:
			mismatch = "allocation"
		}
		if mismatch == "" {
			verified.Record(candidate.artifactHash, candidate.root)
			continue
		}
		findings = append(findings, subnetReplayFinding(subnetSettlementProbeId, "subnet-payout-root", target, frame, scope, subnetSettlementPlaybook, subnetReplayIssue{
			tier: tierPage, sustain: 1, invariant: "committed-root-reproduces-its-artifact",
			expected: "artifact-" + mismatch + "-matches-chain", observed: "artifact-" + mismatch + "-differs",
			symptom:   "A committed payout root does not reproduce its own signed artifact",
			mechanism: "The artifact content hash commits to its root, deployment, policy, epoch, operator and exact 10,000-bps allocation; a verified artifact that differs from the chain proves a wrong, wrong-domain or equivocating root.",
			baseline:  "Each authorized operator root and artifact hash reproduce the canonical signed payout list for exactly that deployment, policy, epoch and operator.",
			action:    "Preserve the artifact bytes by hash and the root commitment receipt; route to the operator's root-signing owner and custody review. Custody caps do not make a wrong list correct.",
			verify:    "Independent replay attributes the mismatch and a recorded disposition closes it.",
		}))
	}
	return findings, nil
}
