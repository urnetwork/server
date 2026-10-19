package handlers

import (
	"net/http"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"
)

// Invalid URL-policy receipts are rejected before accessing model storage.
func TestProviderUrlProbeRejectsOmittedFutureOrIncompleteSuccessPolicy(t *testing.T) {
	const secret = "synthetic-operator-policy-secret"
	defer withStubOperatorIngestSecret(secret)()
	for _, version := range []int{0, 1, 2} {
		body := map[string]any{
			"client_id": server.NewId(), "run_id": server.NewId(), "ok_count": 1, "total_count": 1,
			"class_results": map[string]any{"site": map[string]any{"ok": 1, "total": 1}},
			"url_probe_evidence": &egresshealth.UrlProbeEvidence{
				PolicyVersion: version, Policy: egresshealth.DefaultUrlProbePolicy(),
				Destination: egresshealth.Destination{Name: "synthetic-document", Class: egresshealth.ClassSite, Url: "https://content.example/"},
				MeasuredAt:  time.Now().UTC(), ContentMatcherVersion: 1,
			},
		}
		w := postEgressHealth(t, secret, body)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("incomplete version %d success accepted: status=%d", version, w.Code)
		}
	}
}
