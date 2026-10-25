// Required metadata failures remain independent of unobserved exact-edge contracts.
package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SIGNALS.md §19.1 maps to signal_association_files.go and
// signal_association_files_test.go. The probe bypasses DNS and CDN selection
// while retaining ur.io SNI, then validates the platform contracts rather
// than accepting an arbitrary HTTP 200 body.
func NewAssociationFilesSignal() Signal {
	return &signalAdapter{
		number: "19.1",
		key:    "association-files",
		name:   "Mobile association files",
		probe:  associationFilesProbe{},
	}
}

type associationFilesProbe struct{}

func (associationFilesProbe) id() string             { return "synthetic/web-association-files" }
func (associationFilesProbe) tier() string           { return tierWarn }
func (associationFilesProbe) cadence() time.Duration { return 5 * time.Minute }

const (
	associationOutputMarker   = "monitor_association_metadata_end"
	associationAndroidPackage = "com.bringyour.network"
	associationAppleAppID     = "6BGU69Q742.network.ur"
)

var associationFingerprintPattern = regexp.MustCompile(`(?i)^[0-9a-f]{2}(?::[0-9a-f]{2}){31}$`)

type associationDocument struct {
	kind string
	path string
}

var associationDocuments = []associationDocument{
	{kind: "android", path: "/.well-known/assetlinks.json"},
	{kind: "apple", path: "/.well-known/apple-app-site-association"},
}

type associationHTTPResult struct {
	body     string
	values   map[string]string
	output   string
	err      error
	parseErr error
}

type associationProbeResult struct {
	host       *host
	configured EdgeIPv6InterfaceSettings
	document   associationDocument
	public     associationHTTPResult
	validation error
}

func (associationFilesProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	hostname := strings.TrimSpace(env.cfg.websiteDomain)
	if hostname == "" {
		return nil, nil
	}

	tasks := []associationProbeResult{}
	for _, target := range env.cfg.hosts {
		for _, configured := range target.edgeIPv6 {
			for _, document := range associationDocuments {
				tasks = append(tasks, associationProbeResult{
					host: target, configured: configured, document: document,
				})
			}
		}
	}
	if len(tasks) == 0 {
		return nil, nil
	}

	results := make(chan associationProbeResult, len(tasks))
	semaphore := make(chan struct{}, 8)
	var wait sync.WaitGroup
	for _, queued := range tasks {
		task := queued
		wait.Add(1)
		go func() {
			defer wait.Done()
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-ctx.Done():
				task.public.err = ctx.Err()
				results <- task
				return
			}
			task.public = runAssociationHTTPS(
				ctx,
				env.runner,
				hostname,
				task.configured.Address,
				task.document.path,
			)
			if associationHTTPHealthy(task.public) {
				task.validation = validateAssociationDocument(
					task.document.kind,
					task.public.body,
					task.public.values["monitor_content_type"],
				)
			}
			results <- task
		}()
	}
	wait.Wait()
	close(results)

	ordered := make([]associationProbeResult, 0, len(tasks))
	for result := range results {
		ordered = append(ordered, result)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].host.name != ordered[j].host.name {
			return ordered[i].host.name < ordered[j].host.name
		}
		if ordered[i].configured.Interface != ordered[j].configured.Interface {
			return ordered[i].configured.Interface < ordered[j].configured.Interface
		}
		return ordered[i].document.path < ordered[j].document.path
	})

	return associationFilesFindings(hostname, ordered), nil
}

func runAssociationHTTPS(
	ctx context.Context,
	runner probeRunner,
	hostname string,
	address string,
	path string,
) associationHTTPResult {
	resolve := fmt.Sprintf("%s:443:[%s]", hostname, address)
	writeOut := "\n" + associationOutputMarker + "\n" +
		"monitor_http_code=%{http_code}\n" +
		"monitor_exitcode=%{exitcode}\n" +
		"monitor_remote_ip=%{remote_ip}\n" +
		"monitor_content_type=%{content_type}\n" +
		"monitor_time_total=%{time_total}\n"
	output, err := runner.local(ctx, "curl",
		"--ipv6", "--http1.1", "--silent", "--show-error",
		"--connect-timeout", "3", "--max-time", "5", "--max-filesize", "1048576",
		"--noproxy", "*", "--resolve", resolve,
		"--header", "Accept: application/json",
		"--write-out", writeOut,
		"https://"+hostname+path,
	)
	result := associationHTTPResult{output: output, err: err}
	marker := "\n" + associationOutputMarker + "\n"
	markerAt := strings.LastIndex(output, marker)
	if markerAt < 0 {
		result.values = parseKeyValueLines(output)
		result.parseErr = fmt.Errorf("curl output omitted metadata marker")
		return result
	}
	result.body = strings.TrimSpace(output[:markerAt])
	result.values = parseKeyValueLines(output[markerAt+len(marker):])
	return result
}

func associationHTTPHealthy(result associationHTTPResult) bool {
	return result.err == nil && result.parseErr == nil &&
		result.values["monitor_exitcode"] == "0" &&
		result.values["monitor_http_code"] == "200"
}

func validateAssociationDocument(kind, body, contentType string) error {
	mediaType := strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	if mediaType != "application/json" && !strings.HasSuffix(mediaType, "+json") {
		return fmt.Errorf("content type %q is not JSON", contentType)
	}
	if body == "" {
		return fmt.Errorf("response body is empty")
	}
	switch kind {
	case "android":
		return validateAndroidAssociation(body)
	case "apple":
		return validateAppleAssociation(body)
	default:
		return fmt.Errorf("unknown association document kind %q", kind)
	}
}

func validateAndroidAssociation(body string) error {
	var statements []struct {
		Relation []string `json:"relation"`
		Target   struct {
			Namespace              string   `json:"namespace"`
			PackageName            string   `json:"package_name"`
			SHA256CertFingerprints []string `json:"sha256_cert_fingerprints"`
		} `json:"target"`
	}
	if err := json.Unmarshal([]byte(body), &statements); err != nil {
		return fmt.Errorf("invalid assetlinks JSON: %w", err)
	}
	for _, statement := range statements {
		if !containsString(statement.Relation, "delegate_permission/common.handle_all_urls") ||
			statement.Target.Namespace != "android_app" ||
			statement.Target.PackageName != associationAndroidPackage ||
			len(statement.Target.SHA256CertFingerprints) == 0 {
			continue
		}
		for _, fingerprint := range statement.Target.SHA256CertFingerprints {
			if !associationFingerprintPattern.MatchString(fingerprint) {
				return fmt.Errorf("assetlinks contains a malformed SHA-256 certificate fingerprint")
			}
		}
		return nil
	}
	return fmt.Errorf("assetlinks does not authorize %s to handle all URLs", associationAndroidPackage)
}

func validateAppleAssociation(body string) error {
	var document struct {
		AppLinks struct {
			Details []struct {
				AppID  string   `json:"appID"`
				AppIDs []string `json:"appIDs"`
			} `json:"details"`
		} `json:"applinks"`
		WebCredentials struct {
			Apps []string `json:"apps"`
		} `json:"webcredentials"`
	}
	if err := json.Unmarshal([]byte(body), &document); err != nil {
		return fmt.Errorf("invalid apple-app-site-association JSON: %w", err)
	}
	appLinksAuthorized := false
	for _, detail := range document.AppLinks.Details {
		if detail.AppID == associationAppleAppID || containsString(detail.AppIDs, associationAppleAppID) {
			appLinksAuthorized = true
			break
		}
	}
	if !appLinksAuthorized {
		return fmt.Errorf("apple association does not authorize app links for %s", associationAppleAppID)
	}
	if !containsString(document.WebCredentials.Apps, associationAppleAppID) {
		return fmt.Errorf("apple association does not authorize web credentials for %s", associationAppleAppID)
	}
	return nil
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

// Preserve observed response/content failures and mark skipped contracts unknown.
// Transport ownership stays separate from the static-file product incident.
func associationFilesFindings(hostname string, results []associationProbeResult) []finding {
	failures := []string{}
	missing := 0
	invalid := 0
	responseFailures := 0
	checked := 0
	unobserved := 0
	for _, result := range results {
		exitCode := result.public.values["monitor_exitcode"]
		// A dead exact-address transport is already localized by §18.1. It
		// must not create a second static-file incident with no new evidence.
		if exitCode == "7" || exitCode == "28" {
			unobserved++
			continue
		}
		checked++
		code := result.public.values["monitor_http_code"]
		problem := ""
		switch {
		case result.public.parseErr != nil:
			problem = "response-metadata-invalid"
			responseFailures++
		case result.public.err != nil:
			problem = "request-failed"
			responseFailures++
		case exitCode != "0":
			problem = "curl-exit-nonzero"
			responseFailures++
		case code == "404":
			problem = "required-file-not-found"
			missing++
		case code != "200":
			problem = "unexpected-http-status"
			responseFailures++
		case result.validation != nil:
			problem = associationPublicDocumentProblem(result)
			invalid++
		}
		if problem == "" {
			continue
		}
		failures = append(failures, fmt.Sprintf(
			"edge=%s interface=%s address=%s path=%s http=%s exit=%s content_type=%s problem=%s",
			result.host.name,
			result.configured.Interface,
			result.configured.Address,
			result.document.path,
			associationPublicNumber(code, 100, 599),
			associationPublicNumber(exitCode, 0, 255),
			associationPublicContentType(result.public.values["monitor_content_type"]),
			problem,
		))
	}
	findings := []finding{}
	if unobserved > 0 {
		unknown := cannotObserveFinding("association-files/"+hostname, fmt.Errorf("required association metadata was not observed"))
		unknown.mechanism = "Required association documents were not observed on every enabled exact edge/path. Transport ownership remains with the edge IPv6 signal; this metadata execution cannot establish contract recovery."
		unknown.observed = fmt.Sprintf("expected=%d checked=%d unobserved=%d", len(results), checked, unobserved)
		unknown.action = "Retain prior metadata incidents, preserve independent response/content findings, and use the separately owned transport evidence before choosing a repair. Do not infer a missing static file or synthesize fallback metadata."
		unknown.verify = "Every enabled exact edge/path returns a complete association response that can be classified and semantically validated."
		unknown.playbook = "SIGNALS.md §19.1 and §18.1"
		findings = append(findings, unknown)
	}
	if len(failures) == 0 {
		return findings
	}

	mechanism := "At least one exact edge/path check did not return usable Android/Apple ownership metadata. A failed HTTP or transport response leaves the responding component and bundle contents unknown; ordinary page health cannot substitute for the required contract."
	action := "Identify the responding component and running artifact for each failed edge/path using bounded existing request, upstream-status, and effective configuration evidence. For HTTP 429, distinguish request admission, connection admission, and an upstream rejection before choosing a repair. Preserve successful paths as controls."
	if missing > 0 {
		mechanism += fmt.Sprintf(" %d required paths returned HTTP 404; that response alone does not identify a packaging defect, wrong serving route, or missing deployed asset.", missing)
		action += " For 404 responses, compare the tracked files, Astro dist output, staged tree, and running artifact. Check ancestry of the historical mmm/ur.io 72190198 repair for the `mv dist/*` dotfile omission; an already-current artifact needs a new discriminator. Deploy a Web correction only after identifying the boundary that loses the required documents. Do not edit live platform files or synthesize fallback JSON."
	}
	if invalid > 0 {
		mechanism += fmt.Sprintf(" %d HTTP 200 responses independently failed the metadata content contract.", invalid)
		action += " For invalid HTTP 200 responses, compare the returned media type and semantic authorization with the tracked document and its serving artifact before selecting the content or route repair."
	}

	return append(findings, finding{
		probeId: "synthetic/web-association-files", tier: tierWarn,
		class: "web-association-files", target: hostname, sustain: 1,
		symptom:   fmt.Sprintf("%s mobile association metadata fails on %d exact edge/path checks", hostname, len(failures)),
		mechanism: mechanism,
		baseline:  fmt.Sprintf("Every enabled edge returns HTTP 200 application/json for both association paths; assetlinks authorizes %s and the Apple document authorizes %s for app links and web credentials.", associationAndroidPackage, associationAppleAppID),
		observed:  fmt.Sprintf("checked=%d failed=%d missing_404=%d invalid_contract=%d other_response_failures=%d", checked, len(failures), missing, invalid, responseFailures),
		evidence:  strings.Join(failures, "\n"),
		context:   "Android's release manifest declares an auto-verified ur.io HTTPS intent filter. These are platform ownership contracts, not optional crawler assets; ordinary homepage and process health cannot substitute for them.",
		action:    action,
		verify:    "Require both paths to return HTTP 200 application/json with the expected semantic authorization on every enabled exact edge address and through the canonical CDN hostname. Then require zero exact-path nginx ENOENT lines for ten minutes.",
		playbook:  "SIGNALS.md §19.1",
	})
}

// Only canonical bounded protocol numbers are public; malformed metadata
// must not become arbitrary text in a diagnostic field.
func associationPublicNumber(raw string, minimum, maximum int) string {
	value, err := strconv.Atoi(raw)
	if err != nil || value < minimum || maximum < value || strconv.Itoa(value) != raw {
		return "unknown"
	}
	return strconv.Itoa(value)
}

// Retain a useful media-type category without exposing header parameters or
// arbitrary server-provided strings.
func associationPublicContentType(raw string) string {
	mediaType := strings.ToLower(strings.TrimSpace(strings.Split(raw, ";")[0]))
	switch {
	case mediaType == "":
		return "missing"
	case mediaType == "application/json" || strings.HasSuffix(mediaType, "+json"):
		return "json"
	case mediaType == "text/html":
		return "html"
	default:
		return "other"
	}
}

// Project an already-failed private validator into closed diagnostic reasons.
// Validation and failure counting still use the original private result.
func associationPublicDocumentProblem(result associationProbeResult) string {
	if associationPublicContentType(result.public.values["monitor_content_type"]) != "json" {
		return "content-type-not-json"
	}
	if result.public.body == "" {
		return "document-empty"
	}
	if !json.Valid([]byte(result.public.body)) {
		return "document-json-invalid"
	}
	switch result.document.kind {
	case "android":
		return "android-contract-invalid"
	case "apple":
		return "apple-contract-invalid"
	default:
		return "document-contract-invalid"
	}
}
