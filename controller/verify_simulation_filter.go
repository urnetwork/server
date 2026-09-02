package controller

// verify_simulation_filter.go contains the filesystem-only control used by
// sim-testnet to exercise an equivocation-shaped operator fault: one testnet
// operator can make a bounded provider set unavailable to one validator while
// every other validator continues through the production /verify path. The
// control is inert unless an explicitly simulator-scoped environment points at
// a private file. Mainnet and ordinary testnet processes reject the control.

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/urnetwork/server"
)

const VerifySimulationAssignmentFilterFileEnv = "URNETWORK_SIM_VERIFY_ASSIGNMENT_FILTER_FILE"

const VerifySimulationModeEnv = "URNETWORK_SIM_TESTNET"

const verifySimulationAssignmentFilterSchema = "urnetwork-sim-verify-assignment-filter-v1"

const verifySimulationAssignmentFilterMaximumBytes = 64 * 1024

type verifySimulationAssignmentFilter struct {
	Schema            string   `json:"schema"`
	ValidatorVPK      string   `json:"validator_vpk"`
	ExcludedClientIDs []string `json:"excluded_client_ids"`
}

// verifySimulationAssignmentExclusions loads one atomic simulator control
// snapshot. Missing files mean the fault is inactive. A malformed or
// incorrectly scoped file fails closed instead of silently turning a required
// release vector into the happy path.
func verifySimulationAssignmentExclusions(vpk []byte) ([]server.Id, error) {
	path := strings.TrimSpace(os.Getenv(VerifySimulationAssignmentFilterFileEnv))
	if path == "" {
		return nil, nil
	}
	if !filepath.IsAbs(path) {
		return nil, errors.New("verify simulation assignment filter path is not absolute")
	}
	if os.Getenv("URNETWORK_ST_PROFILE") != "testnet" || os.Getenv(VerifySimulationModeEnv) != "1" {
		return nil, errors.New("verify simulation assignment filter is permitted only in sim-testnet")
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("stat verify simulation assignment filter: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() <= 0 || info.Size() > verifySimulationAssignmentFilterMaximumBytes {
		return nil, errors.New("verify simulation assignment filter is not a private bounded regular file")
	}
	encoded, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read verify simulation assignment filter: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var filter verifySimulationAssignmentFilter
	if err := decoder.Decode(&filter); err != nil {
		return nil, fmt.Errorf("decode verify simulation assignment filter: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("verify simulation assignment filter has trailing JSON")
	}
	if filter.Schema != verifySimulationAssignmentFilterSchema || len(filter.ExcludedClientIDs) == 0 || len(filter.ExcludedClientIDs) > 256 {
		return nil, errors.New("verify simulation assignment filter identity is invalid")
	}
	wantVPK := hex.EncodeToString(vpk)
	if filter.ValidatorVPK != strings.ToLower(filter.ValidatorVPK) || len(filter.ValidatorVPK) != 64 {
		return nil, errors.New("verify simulation assignment filter validator key is not canonical")
	}
	if _, err := hex.DecodeString(filter.ValidatorVPK); err != nil {
		return nil, errors.New("verify simulation assignment filter validator key is invalid")
	}
	if filter.ValidatorVPK != wantVPK {
		return nil, nil
	}
	exclusions := make([]server.Id, len(filter.ExcludedClientIDs))
	prior := ""
	for index, encodedID := range filter.ExcludedClientIDs {
		clientID, err := server.ParseId(encodedID)
		if err != nil || clientID == (server.Id{}) || clientID.String() != encodedID || index > 0 && encodedID <= prior {
			return nil, errors.New("verify simulation assignment filter client ids are not unique canonical order")
		}
		exclusions[index] = clientID
		prior = encodedID
	}
	return exclusions, nil
}

func verifySimulationAssignmentExcluded(exclusions []server.Id, clientID server.Id) bool {
	index := sort.Search(len(exclusions), func(index int) bool {
		return exclusions[index].String() >= clientID.String()
	})
	return index < len(exclusions) && exclusions[index] == clientID
}
