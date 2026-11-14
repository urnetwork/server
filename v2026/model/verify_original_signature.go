// Original verification checks bind every retained wire signature to the
// receipt's exact trail. The historical server key must come from caller trust.
package model

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"

	"github.com/urnetwork/connect/v2026"
)

// Require one response object; the receipt retains its exact original bytes.
func decodeVerifyOriginalResponse(encoded string, response any) error {
	decoder := json.NewDecoder(bytes.NewBufferString(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(response); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("trailing original response bytes")
	}
	return nil
}

// Verify both the metadata receipt and unchanged wire domains. A server receipt
// cannot substitute for the original verifier signature or confirm a new path.
func ValidateVerifyOriginal(original *VerifyOriginalTransition, publicKey ed25519.PublicKey) (*VerifyOriginalBody, error) {
	body, err := DecodeVerifyOriginal(original)
	if err != nil {
		return nil, err
	}
	invalid := func() (*VerifyOriginalBody, error) {
		return nil, errors.New("verification original signature or path mismatch")
	}
	if !VerifyOriginalSignature(original, publicKey) {
		return invalid()
	}
	trail := body.Trail
	if trail.M < connect.VerifyMMin || trail.M > connect.VerifyMMax || len(trail.Hops) != body.PreviousDepth+1 || len(trail.Vpk) != ed25519.PublicKeySize || len(trail.ServerNonce) != connect.VerifyNonceSize {
		return invalid()
	}
	ids := make([]connect.Id, len(trail.Hops))
	seen := map[connect.Id]bool{}
	for index, hop := range trail.Hops {
		if hop == nil || hop.Seed != (index == 0) || hop.ConfirmedMs == 0 || (index > 0 && (hop.AssignedMs == 0 || hop.ConfirmedMs < hop.AssignedMs)) {
			return invalid()
		}
		ids[index] = connect.Id(hop.ClientId)
		if seen[ids[index]] {
			return invalid()
		}
		seen[ids[index]] = true
	}
	if !connect.VerifyVerifyMessageSignature(trail.Vpk, body.RequestMessage, body.RequestSignature) {
		return invalid()
	}
	if body.PreviousDepth == 0 {
		prefix := append([]byte(connect.VerifyCtx), connect.VerifyMsgTypeSeed)
		prefix = append(prefix, trail.Vpk...)
		if len(body.RequestMessage) != len(prefix)+connect.VerifyNonceSize+1 || !bytes.HasPrefix(body.RequestMessage, prefix) {
			return invalid()
		}
		requested := int(body.RequestMessage[len(body.RequestMessage)-1])
		if requested < connect.VerifyMMin {
			requested = connect.VerifyMMin
		}
		if requested > connect.VerifyMMax {
			requested = connect.VerifyMMax
		}
		if requested != trail.M {
			return invalid()
		}
	} else {
		message, err := connect.BuildVerifyExtendMessage(connect.Id(trail.TrailId), trail.ServerNonce, trail.Vpk, byte(trail.M), ids)
		if err != nil || !bytes.Equal(message, body.RequestMessage) {
			return invalid()
		}
	}
	if trail.Status == VerifyTrailStatusExpired {
		if trail.Pending != nil || body.ResponseJson != "" || body.PreviousDepth == 0 {
			return invalid()
		}
		return body, nil
	}
	var response struct {
		Assign *connect.VerifyAssignResult `json:"assign,omitempty"`
		Final  *connect.VerifyFinalResult  `json:"final,omitempty"`
	}
	if err := decodeVerifyOriginalResponse(body.ResponseJson, &response); err != nil {
		return nil, err
	}
	if trail.Status == VerifyTrailStatusActive {
		assign := response.Assign
		if assign == nil || response.Final != nil || trail.Pending == nil || len(ids) >= trail.M || trail.Pending.AssignedMs == 0 || trail.Pending.AssignN < 1 || seen[connect.Id(trail.Pending.ClientId)] || assign.TrailId != connect.Id(trail.TrailId) || !bytes.Equal(assign.ServerNonce, trail.ServerNonce) || assign.M != trail.M || assign.ServerKeyId != trail.ServerKeyId || assign.NextHop != connect.Id(trail.Pending.ClientId) || len(assign.Trail) != len(ids) {
			return invalid()
		}
		for index, id := range ids {
			if assign.Trail[index] != id {
				return invalid()
			}
		}
		message, err := connect.BuildVerifyAssignMessage(trail.ServerKeyId, connect.Id(trail.TrailId), trail.ServerNonce, trail.Vpk, byte(trail.M), append(ids, assign.NextHop))
		if err != nil || !connect.VerifyVerifyMessageSignature(publicKey, message, assign.AssignSig) {
			return invalid()
		}
	} else if trail.Status == VerifyTrailStatusComplete {
		if response.Final == nil || response.Assign != nil || response.Final.Proof == nil || trail.Pending != nil || len(ids) != trail.M {
			return invalid()
		}
		proof := response.Final.Proof
		if response.Final.Status != connect.VerifyStatusComplete || proof.Header.TrailId != connect.Id(trail.TrailId) || !bytes.Equal(proof.Header.ServerNonce, trail.ServerNonce) || !bytes.Equal(proof.Header.Vpk, trail.Vpk) || proof.Header.M != trail.M || proof.ServerKeyId != trail.ServerKeyId || proof.Coverage != uint64(trail.M-1) || !bytes.Equal(proof.VerifierSig, body.RequestSignature) || len(proof.Hops) != len(ids) {
			return invalid()
		}
		for index, hop := range proof.Hops {
			if hop.ClientId != ids[index] || hop.TimeMs != trail.Hops[index].ConfirmedMs || hop.EgressIpHash != trail.Hops[index].EgressIpHash {
				return invalid()
			}
		}
		message, err := connect.BuildVerifyFinalMessage(trail.ServerKeyId, connect.Id(trail.TrailId), trail.ServerNonce, trail.Vpk, byte(trail.M), proof.Hops)
		if err != nil || !connect.VerifyVerifyMessageSignature(publicKey, message, proof.FinalSig) {
			return invalid()
		}
	} else {
		return invalid()
	}
	return body, nil
}
