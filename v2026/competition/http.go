package competition

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/router"
)

const hardRequestBodyLimit = 2 << 20

func HealthHandler(w http.ResponseWriter, r *http.Request) {
	writeCompetitionJson(w, http.StatusOK, DefaultService().Health())
}

func ReadyHandler(w http.ResponseWriter, r *http.Request) {
	service := DefaultService()
	principal, ok := requirePrincipal(w, r, service, true)
	if !ok || principal == nil {
		return
	}
	result, evalError := service.Ready(r.Context())
	if evalError != nil {
		writeCompetitionJson(w, http.StatusServiceUnavailable, evalError)
		return
	}
	writeCompetitionJson(w, http.StatusOK, result)
}

func InfoHandler(w http.ResponseWriter, r *http.Request) {
	result, evalError := DefaultService().Info(r.Context())
	if evalError != nil {
		writeCompetitionJson(w, http.StatusServiceUnavailable, evalError)
		return
	}
	writeCompetitionJson(w, http.StatusOK, result)
}

func GetRoundWorkloadHandler(w http.ResponseWriter, r *http.Request) {
	values := router.GetPathValues(r)
	if len(values) != 1 {
		writeCompetitionJson(w, http.StatusBadRequest, submissionError("invalid_round_id", "round id is missing"))
		return
	}
	roundId, err := server.ParseId(values[0])
	if err != nil {
		writeCompetitionJson(w, http.StatusBadRequest, submissionError("invalid_round_id", "round id is malformed"))
		return
	}
	providers, digest, status, evalError := DefaultService().GetRoundWorkload(r.Context(), roundId)
	if evalError != nil {
		writeCompetitionJson(w, status, evalError)
		return
	}
	defer clear(providers)
	w.Header().Set("Content-Type", "application/yaml")
	w.Header().Set("Content-Disposition", `attachment; filename="providers.yml"`)
	w.Header().Set("Content-Length", strconv.Itoa(len(providers)))
	w.Header().Set("ETag", `"`+digest+`"`)
	w.Header().Set("X-Content-SHA256", digest)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(providers)
}

func GenerateRoundHandler(w http.ResponseWriter, r *http.Request) {
	service := DefaultService()
	if _, ok := requirePrincipal(w, r, service, true); !ok {
		return
	}
	var args GenerateRoundArgs
	if evalError := decodeCompetitionBody(w, r, &args, 16*1024); evalError != nil {
		writeCompetitionJson(w, http.StatusBadRequest, evalError)
		return
	}
	result, evalError := service.GenerateRound(r.Context(), args)
	if evalError != nil {
		status := http.StatusBadRequest
		if evalError.Code == "round_overlap" {
			status = http.StatusConflict
		} else if evalError.Kind == "infrastructure" {
			status = http.StatusServiceUnavailable
		}
		writeCompetitionJson(w, status, evalError)
		return
	}
	writeCompetitionJson(w, http.StatusCreated, result)
}

func SubmitScoreHandler(w http.ResponseWriter, r *http.Request) {
	service := DefaultService()
	principal, ok := requirePrincipal(w, r, service, false)
	if !ok {
		return
	}
	var args ScoreArgs
	limit := int64(hardRequestBodyLimit)
	if service.settings != nil && service.settings.PatchPolicy.MaxPatchBytes*6+4096 < hardRequestBodyLimit {
		// JSON may escape every source character. The decoded patch is checked
		// against MaxPatchBytes again by the structural validator.
		limit = int64(service.settings.PatchPolicy.MaxPatchBytes*6 + 4096)
	}
	if evalError := decodeCompetitionBody(w, r, &args, limit); evalError != nil {
		status := http.StatusBadRequest
		if evalError.Code == "request_too_large" {
			status = http.StatusRequestEntityTooLarge
		}
		writeCompetitionJson(w, status, evalError)
		return
	}
	result, status, evalError := service.Submit(r.Context(), args, principal)
	if evalError != nil {
		if status == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", "30")
		}
		writeCompetitionJson(w, status, evalError)
		return
	}
	writeCompetitionJson(w, status, result)
}

func GetScoreHandler(w http.ResponseWriter, r *http.Request) {
	service := DefaultService()
	principal, ok := requirePrincipal(w, r, service, false)
	if !ok {
		return
	}
	values := router.GetPathValues(r)
	if len(values) != 1 {
		writeCompetitionJson(w, http.StatusBadRequest, submissionError("invalid_job_id", "job id is missing"))
		return
	}
	jobId, err := server.ParseId(values[0])
	if err != nil {
		writeCompetitionJson(w, http.StatusBadRequest, submissionError("invalid_job_id", "job id is malformed"))
		return
	}
	result, status, evalError := service.GetScore(r.Context(), jobId, principal)
	if evalError != nil {
		writeCompetitionJson(w, status, evalError)
		return
	}
	writeCompetitionJson(w, status, result)
}

func requirePrincipal(w http.ResponseWriter, r *http.Request, service *Service, operator bool) (*Principal, bool) {
	settings, err := service.Settings()
	if err != nil {
		writeCompetitionJson(w, http.StatusServiceUnavailable, infrastructureError("configuration_unavailable", "competition configuration is not ready"))
		return nil, false
	}
	principal, ok := Authenticate(r, settings)
	if !ok || operator && principal.Role != "operator" {
		w.Header().Set("WWW-Authenticate", `Bearer realm="competition"`)
		writeCompetitionJson(w, http.StatusUnauthorized, &CompetitionError{
			Kind: "auth", Code: "unauthorized", Message: "missing or invalid competition bearer token", Retriable: false,
		})
		return nil, false
	}
	return principal, true
}

func decodeCompetitionBody(w http.ResponseWriter, r *http.Request, value any, limit int64) *CompetitionError {
	if contentType := r.Header.Get("Content-Type"); contentType != "" && !strings.HasPrefix(strings.ToLower(contentType), "application/json") {
		return submissionError("invalid_content_type", "Content-Type must be application/json")
	}
	if r.Body == nil {
		return submissionError("invalid_json", "request body is required")
	}
	reader := http.MaxBytesReader(w, r.Body, limit)
	body, err := io.ReadAll(reader)
	if err != nil {
		var maxBytes *http.MaxBytesError
		if errors.As(err, &maxBytes) {
			return submissionError("request_too_large", "request body exceeds the published limit")
		}
		return submissionError("invalid_json", "request body could not be read")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return submissionError("invalid_json", "request body is not valid for this endpoint")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return submissionError("invalid_json", "request body must contain one JSON value")
	}
	return nil
}

func writeCompetitionJson(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(true)
	if err := encoder.Encode(value); err != nil {
		return
	}
}
