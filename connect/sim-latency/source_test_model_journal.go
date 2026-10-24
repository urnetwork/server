// Persist the independent test agent's original plan and raw provider streams
// before interpreting them. Completed malformed/uncertain output cannot reroll.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

type sourceTestModelReservation struct {
	Schema         int       `json:"schema"`
	PlanSha256     string    `json:"plan_sha256"`
	SettingsSha256 string    `json:"settings_sha256"`
	StartedAt      time.Time `json:"started_at"`
	Deadline       time.Time `json:"deadline"`
}

type sourceTestProviderReservation struct {
	Schema        int    `json:"schema"`
	RequestSha256 string `json:"request_sha256"`
}

type sourceTestProviderObservation struct {
	Schema        int    `json:"schema"`
	RequestSha256 string `json:"request_sha256"`
	Stream        []byte `json:"stream"`
	PrivateError  string `json:"private_error"`
}

// Byte slices use base64 in journal JSON, preserving exact opaque provider
// bytes without accepting their nested keys as local authority fields.
func sourceTestProviderRecordLimit(settings sourceTestModelSettings) int64 {
	return 2*settings.MaximumStreamBytes + settings.MaximumInputBytes
}

// A replacement process inherits the original total deadline. No transport,
// model error, or changed installed policy can grant a new three-hour window.
func sourceTestModelJournalContext(ctx context.Context, plan sourceTestPlan, settings sourceTestModelSettings) (context.Context, context.CancelFunc, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	directory := settings.ResponseJournalDirectory
	if directory == "" {
		return ctx, func() {}, nil
	}
	if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory || filepath.Base(directory) != "independent-model-journal" || filepath.Base(filepath.Dir(directory)) != plan.Owner {
		return nil, nil, errors.New("independent model journal requires its exact build owner")
	}
	parent := filepath.Dir(directory)
	canonical, err := filepath.EvalSymlinks(parent)
	info, statErr := os.Lstat(parent)
	if err != nil || statErr != nil || canonical != parent || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return nil, nil, errors.New("independent model journal parent is not private and canonical")
	}
	if err := os.Mkdir(directory, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, nil, err
	}
	canonical, err = filepath.EvalSymlinks(directory)
	info, statErr = os.Lstat(directory)
	if err != nil || statErr != nil || canonical != directory || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return nil, nil, errors.New("independent model journal was redirected or exposed")
	}
	parentFile, err := os.Open(parent)
	if err != nil {
		return nil, nil, err
	}
	syncErr, closeErr := parentFile.Sync(), parentFile.Close()
	if err := errors.Join(syncErr, closeErr); err != nil {
		return nil, nil, err
	}
	planBytes, err := json.Marshal(plan)
	if err != nil {
		return nil, nil, err
	}
	planDigest := sha256.Sum256(planBytes)
	settings.ResponseJournalDirectory = ""
	settingsBytes, err := json.Marshal(settings)
	if err != nil {
		return nil, nil, err
	}
	settingsDigest := sha256.Sum256(settingsBytes)
	planSha, settingsSha := hex.EncodeToString(planDigest[:]), hex.EncodeToString(settingsDigest[:])
	path := filepath.Join(directory, "plan.json")
	var reservation sourceTestModelReservation
	limit := sourceTestProviderRecordLimit(settings)
	if err := readSourceTestRecord(path, limit, &reservation); errors.Is(err, os.ErrNotExist) {
		// A missing plan cannot reset authority while any phase evidence exists.
		for _, phase := range []string{"invocation", "completion"} {
			for _, suffix := range []string{"-reservation.json", "-observation.json"} {
				if _, err := os.Lstat(filepath.Join(directory, phase+suffix)); !errors.Is(err, os.ErrNotExist) {
					return nil, nil, errors.New("independent provider evidence has no original model plan")
				}
			}
		}
		deadline, ok := ctx.Deadline()
		now := time.Now().UTC()
		if !ok || !now.Before(deadline) {
			return nil, nil, errors.New("independent model plan requires a live original deadline")
		}
		reservation = sourceTestModelReservation{Schema: 1, PlanSha256: planSha, SettingsSha256: settingsSha, StartedAt: now, Deadline: deadline.UTC()}
		content, err := json.Marshal(reservation)
		if err != nil {
			return nil, nil, err
		}
		if err := publishSourceTestRecord(path, content); errors.Is(err, os.ErrExist) {
			if err := readSourceTestRecord(path, limit, &reservation); err != nil {
				return nil, nil, err
			}
		} else if err != nil {
			return nil, nil, err
		}
	} else if err != nil {
		return nil, nil, err
	}
	if reservation.Schema != 1 || reservation.PlanSha256 != planSha || reservation.SettingsSha256 != settingsSha || reservation.StartedAt.IsZero() || !reservation.StartedAt.Before(reservation.Deadline) {
		return nil, nil, errors.New("independent model retry changed its original plan or policy")
	}
	bounded, cancel := context.WithDeadline(ctx, reservation.Deadline)
	if err := bounded.Err(); err != nil {
		cancel()
		return nil, nil, err
	}
	return bounded, cancel, nil
}

// At-most-once dispatch is deliberately conservative. An unfinished transport
// or missing observation is uncertainty, not permission to sample again. The
// qualifier must reconcile it explicitly; completed streams replay byte-for-byte.
func journalSourceTestProviderStream(ctx context.Context, phase string, payload []byte, settings sourceTestModelSettings, send func() ([]byte, error)) ([]byte, error) {
	if settings.ResponseJournalDirectory == "" {
		return send()
	}
	if phase != "invocation" && phase != "completion" || send == nil {
		return nil, errors.New("independent provider journal phase is invalid")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	digest := sha256.Sum256(payload)
	requestSha := hex.EncodeToString(digest[:])
	reservationPath := filepath.Join(settings.ResponseJournalDirectory, phase+"-reservation.json")
	observationPath := filepath.Join(settings.ResponseJournalDirectory, phase+"-observation.json")
	limit := sourceTestProviderRecordLimit(settings)
	var reservation sourceTestProviderReservation
	owned := false
	if err := readSourceTestRecord(reservationPath, limit, &reservation); errors.Is(err, os.ErrNotExist) {
		if _, err := os.Lstat(observationPath); !errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("independent provider observation has no dispatch reservation")
		}
		reservation = sourceTestProviderReservation{Schema: 1, RequestSha256: requestSha}
		content, err := json.Marshal(reservation)
		if err != nil {
			return nil, err
		}
		if err := publishSourceTestRecord(reservationPath, content); err == nil {
			owned = true
		} else if errors.Is(err, os.ErrExist) {
			if err := readSourceTestRecord(reservationPath, limit, &reservation); err != nil {
				return nil, err
			}
		} else {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	if reservation.Schema != 1 || reservation.RequestSha256 != requestSha {
		return nil, errors.New("independent provider retry changed its exact request")
	}
	var observation sourceTestProviderObservation
	if owned {
		stream, sendErr := send()
		observation = sourceTestProviderObservation{Schema: 1, RequestSha256: requestSha, Stream: stream}
		if sendErr != nil || int64(len(stream)) > settings.MaximumStreamBytes {
			observation.Stream = nil
			observation.PrivateError = "Provider transport did not yield bounded complete evidence; reconcile before recovery."
		}
		content, err := json.Marshal(observation)
		if err != nil || int64(len(content)) > limit {
			return nil, errors.New("independent provider observation exceeds its evidence budget")
		}
		if err := publishSourceTestRecord(observationPath, content); err != nil {
			return nil, errors.Join(sendErr, err)
		}
	} else if err := readSourceTestRecord(observationPath, limit, &observation); errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("independent provider dispatch is active or interrupted; reconcile its actual owner")
	} else if err != nil {
		return nil, err
	}
	if observation.Schema != 1 || observation.RequestSha256 != requestSha || observation.PrivateError != "" || len(observation.Stream) == 0 || int64(len(observation.Stream)) > settings.MaximumStreamBytes {
		return nil, errors.New("independent provider evidence is missing, changed or unresolved; do not reroll")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return observation.Stream, nil
}
