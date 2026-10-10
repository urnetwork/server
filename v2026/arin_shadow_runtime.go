package server

import (
	"context"
	"sync"
	"time"
)

// A protected config is explicit operator authority for one capture run. No
// socket or candidate reader is opened without an explicit private config.
// Vault is the normal deployment path; an absolute env path supports an
// isolated operator/test mount without placing a credential in the image.
type ArinShadowRuntimeConfig struct {
	RunId             Id        `json:"run_id"`
	KeyHex            string    `json:"key_hex"`
	Directory         string    `json:"directory"`
	ExpiresAt         time.Time `json:"expires_at"`
	ActivePath        string    `json:"active_path"`
	ActiveSHA256      string    `json:"active_sha256"`
	CandidatePath     string    `json:"candidate_path"`
	CandidateSHA256   string    `json:"candidate_sha256"`
	Capacity          int       `json:"capacity"`
	ActiveResource    string    `json:"active_resource,omitempty"`
	CandidateResource string    `json:"candidate_resource,omitempty"`
}

type ArinShadowRuntime struct {
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

func (r *ArinShadowRuntime) Close() {
	if r == nil {
		return
	}
	r.once.Do(r.cancel)
	<-r.done
}
