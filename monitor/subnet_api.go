// SIGNALS.md §22.0 bounded public Api reads for the subnet readers. Both
// enrolled origins are read separately through the injected Do seam with a
// byte cap before decoding, identity encoding, no cache and no credentials.
// Two origins can share one MinIO or cache; agreement is not independence.
package monitor

import (
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"

	"github.com/urfoundation/sn/payoutartifact"
)

// The payout artifact cap matches the §22.8a reader.
const maximumSubnetApiArtifactBytes = maximumSubnetArtifactBytes

// One bounded Get of a Json route. The ETag is returned for routes that pin
// content; every response body is closed before return.
func readSubnetApi(parent context.Context, run *subnetRun, origin, path string, query url.Values, limit int64) ([]byte, string, error) {
	select {
	case <-parent.Done():
		return nil, "", parent.Err()
	default:
	}
	ctx, cancel := context.WithTimeout(parent, run.limits.requestTimeout)
	defer cancel()
	endpoint := origin + path
	if len(query) != 0 {
		endpoint += "?" + query.Encode()
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, "", subnetReadFailure("request-invalid", err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Accept-Encoding", "identity")
	request.Header.Set("Cache-Control", "no-cache, no-store")
	response, err := doScopedGrafanaRequest(run.session.client, request)
	if response == nil || response.Body == nil {
		if parentErr := parent.Err(); parentErr != nil {
			return nil, "", parentErr
		}
		return nil, "", subnetReadFailure("api-unavailable", errors.Join(err, ctx.Err()))
	}
	defer response.Body.Close()
	if err != nil {
		return nil, "", subnetReadFailure("api-unavailable", err)
	}
	if response.StatusCode == http.StatusNotFound {
		return nil, "", subnetReadFailure("api-not-found", nil)
	}
	mediaType, _, mediaErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if response.StatusCode != http.StatusOK || response.Uncompressed || response.Header.Get("Content-Encoding") != "" || mediaErr != nil || mediaType != "application/json" || response.ContentLength > limit || len(response.Header.Values("ETag")) > 1 {
		return nil, "", subnetReadFailure("api-contract-invalid", nil)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		if parentErr := parent.Err(); parentErr != nil {
			return nil, "", parentErr
		}
		return nil, "", subnetReadFailure("api-body-unavailable", err)
	}
	if int64(len(raw)) > limit || response.ContentLength >= 0 && response.ContentLength != int64(len(raw)) {
		return nil, "", subnetReadFailure("api-body-oversized", nil)
	}
	return raw, response.Header.Get("ETag"), nil
}

// The outcome of reading one committed payout artifact at both origins.
type subnetArtifactRead struct {
	artifact     *payoutartifact.Artifact
	equivocation bool
	reason       string
}

// Reads the artifact whose content hash the chain committed. Any origin that
// serves a different decodable artifact under that name is equivocation; a
// read failure at every origin is unavailability, never an empty artifact.
func readSubnetCommittedArtifact(ctx context.Context, run *subnetRun, contentHash string) (subnetArtifactRead, error) {
	result := subnetArtifactRead{reason: "artifact-unavailable"}
	for _, origin := range run.settings.ApiOrigins {
		raw, etag, err := readSubnetApi(ctx, run, origin, "/sn/artifact", url.Values{"hash": []string{contentHash}}, maximumSubnetApiArtifactBytes)
		if canceled := subnetCanceled(ctx); canceled != nil {
			return subnetArtifactRead{}, canceled
		}
		if err != nil {
			continue
		}
		artifact, err := payoutartifact.DecodeWithContext(ctx, raw)
		if canceled := subnetCanceled(ctx); canceled != nil {
			return subnetArtifactRead{}, canceled
		}
		if err != nil {
			result.reason = "artifact-invalid"
			continue
		}
		if artifact.ContentHash != contentHash || etag != `"`+contentHash+`"` {
			return subnetArtifactRead{equivocation: true, reason: "artifact-equivocation"}, nil
		}
		if result.artifact == nil {
			result = subnetArtifactRead{artifact: artifact}
		}
	}
	return result, nil
}

// The public content-hash spelling of a committed artifact digest.
func subnetArtifactContentHash(digest [32]byte) string {
	return "sha256:" + strings.TrimPrefix(subnetHex32(digest), "0x")
}
