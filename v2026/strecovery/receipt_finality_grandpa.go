// GRANDPA certificates authenticate weighted votes relative to an explicit
// authority checkpoint. They cannot authenticate that checkpoint themselves.
package strecovery

import (
	"context"
	"crypto/ed25519"
	"encoding/binary"
	"encoding/hex"
	"errors"
)

const maximumGrandpaJustificationBytes = 4 * 1024 * 1024

// Reject duplicated, zero-weight and overflowing distributions before counting
// votes. A checkpoint with fabricated keys still has no external authority.
func grandpaAuthorityWeights(authorities []GrandpaAuthority) (map[string]uint64, uint64, error) {
	if len(authorities) == 0 || len(authorities) > maximumGrandpaAuthorities {
		return nil, 0, errors.New("native finality authority count differs")
	}
	weights := make(map[string]uint64, len(authorities))
	var total uint64
	for _, authority := range authorities {
		if !canonicalHex(authority.PublicKey, ed25519.PublicKeySize) || authority.Weight == 0 || weights[authority.PublicKey] != 0 || ^uint64(0)-total < authority.Weight {
			return nil, 0, errors.New("native finality authority key, weight, duplication or total overflow differs")
		}
		weights[authority.PublicKey] = authority.Weight
		total += authority.Weight
	}
	return weights, total, nil
}

// Votes retain their actual signed target; descendant ancestry is separately
// authenticated and cannot be replaced by a numeric height comparison.
type grandpaPrecommit struct {
	target    ObservedBlockIdentity
	signature []byte
	publicKey []byte
}

// The wire order is round, commit(target hash/u32/votes), ancestry headers.
// These are full justifications, not RPC-rendered compact commit summaries.
type grandpaJustification struct {
	round    uint64
	target   ObservedBlockIdentity
	votes    []grandpaPrecommit
	ancestry []*nativeFinalityHeader
}

// Bound counts before allocating; each ancestry header shares the global
// header budget with checkpoint and finalized-path headers.
func (self *nativeFinalityBudget) justification(encoded string) (*grandpaJustification, error) {
	raw, err := self.decode(encoded, maximumGrandpaJustificationBytes)
	if err != nil {
		return nil, err
	}
	reader := finalityScaleReader{data: raw}
	prefix, err := reader.take(44)
	if err != nil {
		return nil, err
	}
	result := &grandpaJustification{round: binary.LittleEndian.Uint64(prefix[:8]), target: ObservedBlockIdentity{Hash: "0x" + hex.EncodeToString(prefix[8:40]), Number: uint64(binary.LittleEndian.Uint32(prefix[40:44]))}}
	count, err := reader.compact()
	if err != nil || count == 0 || count > maximumGrandpaAuthorities {
		return nil, errors.New("native finality precommit count differs")
	}
	result.votes = make([]grandpaPrecommit, int(count))
	for index := range result.votes {
		vote, err := reader.take(132)
		if err != nil {
			return nil, err
		}
		result.votes[index] = grandpaPrecommit{target: ObservedBlockIdentity{Hash: "0x" + hex.EncodeToString(vote[:32]), Number: uint64(binary.LittleEndian.Uint32(vote[32:36]))}, signature: vote[36:100], publicKey: vote[100:132]}
	}
	count, err = reader.compact()
	if err != nil || count > uint64(maximumNativeFinalityHeaders-self.headers) {
		return nil, errors.New("native finality justification ancestry count exceeds shared bound")
	}
	for range count {
		header, err := self.readHeader(&reader)
		if err != nil {
			return nil, err
		}
		result.ancestry = append(result.ancestry, header)
	}
	if reader.offset != len(raw) {
		return nil, errors.New("native finality justification has trailing bytes")
	}
	return result, nil
}

// The result records facts about signatures and weights only, never approval
// of the source keys. Set changes are enacted after this certificate succeeds.
type GrandpaCertificateResult struct {
	Target         ObservedBlockIdentity `json:"target"`
	SetId          uint64                `json:"set_id"`
	Round          uint64                `json:"round"`
	SignerCount    int                   `json:"signer_count"`
	SignedWeight   uint64                `json:"signed_weight"`
	TotalWeight    uint64                `json:"total_weight"`
	RequiredWeight uint64                `json:"required_weight"`
}

// This profile admits non-equivocating commits and exact precommit-GHOST
// targets. Each authority contributes once; signatures bind message type,
// target, round and set ID. Unused ancestry and unknown voters are refused.
func verifyGrandpaCertificate(ctx context.Context, justification *grandpaJustification, target *nativeFinalityHeader, setId uint64, authorities []GrandpaAuthority, pending *GrandpaScheduledChange) (GrandpaCertificateResult, error) {
	result := GrandpaCertificateResult{}
	if justification.target != target.identity {
		return result, errors.New("native finality certificate target differs from the retained native header")
	}
	weights, total, err := grandpaAuthorityWeights(authorities)
	if err != nil {
		return result, err
	}
	// Equivalent to floor(2*total/3)+1 without multiplying into overflow.
	threshold := total - (total-1)/3
	headerHashes := make(map[string]*nativeFinalityHeader, len(justification.ancestry))
	for _, header := range justification.ancestry {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if header.identity.Number <= target.identity.Number || header.identity.Number-target.identity.Number > maximumNativeFinalityHeaders || headerHashes[header.identity.Hash] != nil {
			return result, errors.New("native finality vote ancestry is duplicated or outside the descendant bound")
		}
		if pending != nil && header.identity.Number > pending.EnactmentNumber {
			return result, errors.New("native finality precommit crosses the authority enactment boundary")
		}
		change, err := header.scheduledChange()
		if err != nil {
			return result, err
		}
		if change != nil {
			return result, errors.New("native finality vote ancestry contains an unfinalized authority signal; obtain a certificate at that signal")
		}
		headerHashes[header.identity.Hash] = header
	}
	seenVoters := make(map[string]bool, len(justification.votes))
	usedHeaders := make(map[string]bool, len(headerHashes))
	childWeights := map[string]uint64{}
	var signedWeight uint64
	for _, vote := range justification.votes {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		key := "0x" + hex.EncodeToString(vote.publicKey)
		weight := weights[key]
		if weight == 0 || seenVoters[key] {
			return result, errors.New("native finality precommit has an unknown or duplicate/equivocating authority")
		}
		seenVoters[key] = true
		if vote.target.Number < target.identity.Number || vote.target.Number-target.identity.Number > maximumNativeFinalityHeaders || (pending != nil && vote.target.Number > pending.EnactmentNumber) {
			return result, errors.New("native finality precommit is outside its target or authority boundary")
		}
		// SCALE Message::Precommit index 1, H256, u32, u64 round, u64 set ID.
		payload := make([]byte, 1, 53)
		payload[0] = 1
		hash, _ := hex.DecodeString(vote.target.Hash[2:])
		payload = append(payload, hash...)
		payload = binary.LittleEndian.AppendUint32(payload, uint32(vote.target.Number))
		payload = binary.LittleEndian.AppendUint64(payload, justification.round)
		payload = binary.LittleEndian.AppendUint64(payload, setId)
		if !ed25519.Verify(vote.publicKey, payload, vote.signature) {
			return result, errors.New("native finality GRANDPA precommit signature or domain differs")
		}
		cursor := vote.target
		child := ""
		for cursor.Number > target.identity.Number {
			if err := ctx.Err(); err != nil {
				return result, err
			}
			header := headerHashes[cursor.Hash]
			if header == nil || header.identity != cursor {
				return result, errors.New("native finality precommit ancestry is missing or has a false height")
			}
			usedHeaders[cursor.Hash] = true
			child = cursor.Hash
			cursor = ObservedBlockIdentity{Hash: header.parent, Number: cursor.Number - 1}
		}
		if cursor != target.identity {
			return result, errors.New("native finality precommit ancestry does not reach the exact target")
		}
		if child != "" {
			childWeights[child] += weight
		}
		signedWeight += weight
	}
	if signedWeight < threshold {
		return result, errors.New("native finality GRANDPA weighted quorum is insufficient")
	}
	for _, weight := range childWeights {
		if weight >= threshold {
			return result, errors.New("native finality commit target is below the precommit GHOST")
		}
	}
	if len(usedHeaders) != len(headerHashes) {
		return result, errors.New("native finality certificate has unused ancestry headers")
	}
	return GrandpaCertificateResult{Target: target.identity, SetId: setId, Round: justification.round, SignerCount: len(seenVoters), SignedWeight: signedWeight, TotalWeight: total, RequiredWeight: threshold}, nil
}
