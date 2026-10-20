// Each redirect remains inside one bounded URL attempt and provider client.
package egresshealth

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

// A production transport grants only this request's exact redirect target.
// Synthetic clients need no transport capability and never trigger a fallback.
type providerUrlProbeTransport interface {
	ProviderUrlProbeContext(context.Context, *url.URL, bool) (context.Context, error)
}

// Uses the caller's transport unchanged; no redirect may use the host network.
func fetchUrlProbe(ctx context.Context, client *http.Client, destination Destination, timeout time.Duration, profile RequestProfile, opts Options) CheckResult {
	policy := opts.urlProbePolicy()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	isolated := *client
	isolated.Jar = nil
	isolated.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	target, err := url.Parse(destination.Url)
	if err != nil {
		return CheckResult{Name: destination.Name, Class: destination.Class, Err: err.Error(), FailureStage: "request_build"}
	}
	origin := target.Host
	security := []UrlProbeSecurityEvent{}
	for redirects := 0; ; redirects++ {
		result := CheckResult{Name: destination.Name, Class: destination.Class, RedirectCount: redirects, UrlProbeSecurity: security}
		if err := validateUrlProbeTarget(target); err != nil {
			result.Err, result.FailureStage = err.Error(), "redirect_policy"
			return result
		}
		hopCtx := ctx
		if authorizer, ok := client.Transport.(providerUrlProbeTransport); ok {
			hopCtx, err = authorizer.ProviderUrlProbeContext(ctx, target, redirects > 0)
			if err != nil {
				result.Err, result.FailureStage = err.Error(), "redirect_policy"
				return result
			}
		}
		start := opts.now()
		hopCtx, progress := traceRequestProgressAt(hopCtx, opts.now)
		request, err := http.NewRequestWithContext(hopCtx, http.MethodGet, target.String(), nil)
		if err != nil {
			result.Err, result.FailureStage = err.Error(), "request_build"
			return result
		}
		applyHeaders(request, profile, destination.Headers)
		// Identity keeps the usual path's byte count physical. If a server
		// still sends gzip, count encoded bytes separately while decoding a
		// bounded body for content inspection. Decoded size is never bandwidth.
		request.Header.Set("Accept-Encoding", "identity")
		if !strings.EqualFold(origin, target.Host) {
			// Neither the source's configured credentials nor any server-set
			// cookie may follow a redirect to a different origin.
			request.Header.Del("Authorization")
			request.Header.Del("Proxy-Authorization")
			request.Header.Del("Cookie")
			request.Header.Del("Cookie2")
		}
		response, err := isolated.Do(request)
		targetDestination := destination
		targetDestination.Url = target.String()
		if !strings.EqualFold(origin, target.Host) {
			// A retained failed-hop snapshot is a future initial request. Do not
			// carry the original site's credentials into that recheck either.
			targetDestination.Headers = map[string]string{}
			for name, value := range destination.Headers {
				switch http.CanonicalHeaderKey(name) {
				case "Authorization", "Proxy-Authorization", "Cookie", "Cookie2":
				default:
					targetDestination.Headers[name] = value
				}
			}
		}
		if err != nil {
			progress.recordTiming(&result, start, opts.now())
			result.Err, result.FailureStage = err.Error(), echoRequestStage(err)
			if result.FailureStage == "request_timeout" {
				result.FailureStage = progress.timeoutStage()
			}
			result.TlsAuthenticationFailure = isTlsAuthenticationFailure(err)
			if result.TlsAuthenticationFailure {
				result.UrlProbeSecurity = append(security, UrlProbeSecurityEvent{Destination: targetDestination, TlsFailure: true, MeasuredAt: opts.now()})
			}
			if response == nil && !result.TlsAuthenticationFailure && result.FailureStage != "policy" && progress.phase.Load() < requestPhaseAfterDial {
				if unavailable, ok := client.Transport.(interface{ ProviderMeasurementUnavailable() bool }); ok && unavailable.ProviderMeasurementUnavailable() {
					result.NotMeasured, result.FailureStage = true, "local_control_registration"
				} else if unavailable, ok := client.Transport.(interface{ ProviderContractAcquisitionUnavailable() bool }); ok && unavailable.ProviderContractAcquisitionUnavailable() {
					result.NotMeasured, result.FailureStage = true, "local_contract_acquisition"
				} else if unavailable, ok := client.Transport.(interface{ ProviderLocalWriteUnavailable() bool }); ok && unavailable.ProviderLocalWriteUnavailable() {
					result.NotMeasured, result.FailureStage = true, "local_transport_admission"
				}
			}
			return result
		}
		result.StatusCode = response.StatusCode
		progress.recordFirstByte(opts.now())
		security = append(security, UrlProbeSecurityEvent{Destination: targetDestination, MeasuredAt: opts.now(), TlsAuthenticated: true})
		result.UrlProbeSecurity = security
		if isUrlRedirect(response.StatusCode) {
			response.Body.Close()
			progress.recordTiming(&result, start, opts.now())
			if redirects >= policy.MaxRedirects {
				result.Err, result.FailureStage = "URL redirect limit exceeded", "redirect_limit"
				return result
			}
			location := response.Header.Get("Location")
			if location == "" {
				result.Err, result.FailureStage = "URL redirect has no Location", "redirect_location"
				return result
			}
			next, parseErr := target.Parse(location)
			if parseErr != nil {
				result.Err, result.FailureStage = "URL redirect Location is invalid", "redirect_location"
				return result
			}
			target = next
			continue
		}
		// The first actual wire body byte starts the transfer clock. Its wait
		// after headers is separate, and that first byte is not in the timed
		// numerator. Gzip header reads go through this same wire reader.
		bodyStartedAt := opts.now()
		wire := &urlProbeWireReader{reader: response.Body, now: opts.now}
		bodyReader := io.Reader(io.LimitReader(wire, int64(policy.MaxBodyBytes)))
		encoding := strings.ToLower(strings.TrimSpace(response.Header.Get("Content-Encoding")))
		var decoder *gzip.Reader
		if encoding == "gzip" {
			decoder, err = gzip.NewReader(bodyReader)
			if err == nil {
				bodyReader = decoder
			}
		} else if encoding != "" && encoding != "identity" || response.Uncompressed {
			err = fmt.Errorf("URL response encoding cannot be measured as wire bytes")
			result.NotMeasured = true
		}
		if err != nil {
			response.Body.Close()
			result.WireByteCount = wire.bytes
			recordUrlProbeBodyTiming(&result, progress, start, bodyStartedAt, opts.now(), wire)
			result.Err, result.FailureStage = err.Error(), "response_encoding"
			return result
		}
		// A read ceiling is not a required transfer: at the accepted 100kbps,
		// 1MiB takes about 84 seconds. Keep a useful content prefix plus a
		// meaningful wire sample, then release the tunnel intentionally.
		body, complete, sampled, readErr := readUrlProbeSample(bodyReader, policy.MaxBodyBytes, func(body []byte) bool {
			return len(body) >= 64*1024 && wire.bytes-1 >= int64(policy.MinThroughputBytes) &&
				len(bytes.TrimSpace(body)) > 0 && destination.Verify.check(body) == nil
		})
		if decoder != nil {
			decoder.Close()
		}
		if complete && response.ContentLength > wire.bytes {
			complete, readErr = false, io.ErrUnexpectedEOF
		}
		response.Body.Close()
		result.ByteCount = int64(len(body))
		result.WireByteCount = wire.bytes
		result.BodyComplete = complete && wire.eof && response.StatusCode != http.StatusPartialContent && response.Header.Get("Content-Range") == ""
		result.BodySampled = sampled
		recordUrlProbeBodyTiming(&result, progress, start, bodyStartedAt, opts.now(), wire)
		if readErr != nil {
			result.Err, result.FailureStage = readErr.Error(), "response_body"
			return result
		}
		classification, err := judgeUrlProbeContent(destination, response, body)
		result.ContentClassification = classification
		if err != nil {
			result.Err, result.FailureStage = err.Error(), "response_content"
			return result
		}
		if err := judgeUrlProbePerformance(&result, policy); err != nil {
			result.Err, result.FailureStage = err.Error(), "response_performance"
			return result
		}
		result.Ok = true
		if destination.Class == ClassConnectivity && destination.Verify.Kind == BodyCheckIpText {
			if ip := sampledExitWithPolicy(string(body), opts.exitAddressAllowed); ip != "" {
				result.ObservedExitIp, result.ExitObservedAt = ip, opts.now()
			}
		}
		return result
	}
}

// Response-byte TTFB and the subsequent wire-body sampling clock are distinct.
func recordUrlProbeBodyTiming(result *CheckResult, progress *requestProgress, start, bodyStartedAt, end time.Time, wire *urlProbeWireReader) {
	progress.recordTiming(result, start, end)
	result.BodyDuration = 0
	result.BodyBytesPerSecond = 0
	result.WireSampleByteCount = max(0, wire.bytes-1)
	if wire.bytes > 0 {
		result.BodyFirstByteWait = max(0, wire.firstByteAt.Sub(bodyStartedAt))
		result.BodyDuration = max(0, wire.lastByteAt.Sub(wire.firstByteAt))
	}
	if result.BodyDuration > 0 {
		result.BodyBytesPerSecond = float64(result.WireSampleByteCount) / result.BodyDuration.Seconds()
	}
}

type urlProbeWireReader struct {
	reader      io.Reader
	bytes       int64
	eof         bool
	now         func() time.Time
	firstByteAt time.Time
	lastByteAt  time.Time
}

func (self *urlProbeWireReader) Read(buffer []byte) (int, error) {
	if self.bytes == 0 && len(buffer) > 1 {
		buffer = buffer[:1]
	}
	n, err := self.reader.Read(buffer)
	if n > 0 {
		at := self.now()
		if self.bytes == 0 {
			self.firstByteAt = at
		}
		self.lastByteAt = at
	}
	self.bytes += int64(n)
	self.eof = self.eof || err == io.EOF
	return n, err
}

// Unlike LimitReader, this preserves whether the peer actually ended the body.
// Reaching our cap is not an EOF proof and cannot trigger the tiny-page bypass.
func readUrlProbeBody(reader io.Reader, limit int) ([]byte, bool, error) {
	body, complete, _, err := readUrlProbeSample(reader, limit, nil)
	return body, complete, err
}

// Intentional sampling is distinct from peer EOF, a read error or a timeout.
// The caller only stops early after sufficient wire bytes and usable content.
func readUrlProbeSample(reader io.Reader, limit int, sufficient func([]byte) bool) ([]byte, bool, bool, error) {
	body := make([]byte, 0, min(limit, 32*1024))
	buffer := make([]byte, min(limit, 32*1024))
	emptyReads := 0
	for len(body) < limit {
		n, err := reader.Read(buffer[:min(len(buffer), limit-len(body))])
		body = append(body, buffer[:n]...)
		if err == io.EOF {
			return body, true, false, nil
		}
		if err != nil {
			return body, false, false, err
		}
		if sufficient != nil && sufficient(body) {
			return body, false, true, nil
		}
		if n == 0 {
			emptyReads++
			if emptyReads >= 100 {
				return body, false, false, io.ErrNoProgress
			}
		} else {
			emptyReads = 0
		}
	}
	return body, false, true, nil
}

// Final request-written TTFB excludes DNS/TCP/TLS and all preceding redirects.
// A truly small complete document passes without claiming a bandwidth estimate.
func judgeUrlProbePerformance(result *CheckResult, policy UrlProbePolicy) error {
	if !result.RequestWritten {
		result.PerformanceClassification = "ttfb_not_measured"
		result.NotMeasured = true
		return fmt.Errorf("final request-written timestamp was not observed")
	}
	if time.Duration(policy.MaxTtfbMillis)*time.Millisecond < result.RequestTimeToFirstByte {
		result.PerformanceClassification = "ttfb_slow"
		return fmt.Errorf("final URL response exceeded the time-to-first-byte limit")
	}
	if result.WireSampleByteCount < int64(policy.MinThroughputBytes) {
		result.PerformanceClassification = "insufficient_sample"
		if result.BodyComplete {
			return nil
		}
		return fmt.Errorf("partial URL content cannot establish throughput")
	}
	if result.BodyDuration <= 0 {
		result.PerformanceClassification = "throughput_not_measured"
		result.NotMeasured = true
		return fmt.Errorf("final URL body transfer duration was not observed")
	}
	// Compare counts against elapsed time before dividing: a quotient such as
	// 65536 / 5.24288 rounds just below 12500 and must not reject the exact bar.
	if float64(result.WireSampleByteCount)*8*float64(time.Second) < float64(policy.MinThroughputBps)*float64(result.BodyDuration) {
		result.PerformanceClassification = "throughput_slow"
		return fmt.Errorf("final URL response was below the throughput limit")
	}
	result.PerformanceClassification = "passed"
	return nil
}

// A redirect can widen the exact hostname, never the scheme or credentials.
func validateUrlProbeTarget(target *url.URL) error {
	if target == nil || target.Scheme != "https" || target.Hostname() == "" || target.User != nil || (target.Port() != "" && target.Port() != "443") {
		return fmt.Errorf("URL probe target must be credential-free HTTPS on port 443")
	}
	if ip, err := netip.ParseAddr(target.Hostname()); err == nil && !publicExitAddress(ip) {
		return fmt.Errorf("URL probe target is not a public address")
	}
	return nil
}

func isUrlRedirect(status int) bool {
	return status == 301 || status == 302 || status == 303 || status == 307 || status == 308
}

// Conservative challenge signatures preserve a reason, without inventing a
// malicious-provider finding. Ordinary text mentioning CAPTCHA is not enough.
func judgeUrlProbeContent(destination Destination, response *http.Response, body []byte) (string, error) {
	page := inspectUrlProbeHtml(body)
	if strings.EqualFold(response.Header.Get("Cf-Mitigated"), "challenge") ||
		strings.EqualFold(response.Header.Get("X-Amzn-Waf-Action"), "captcha") ||
		strings.EqualFold(response.Header.Get("X-Amzn-Waf-Action"), "challenge") ||
		page.humanGate {
		return "captcha", fmt.Errorf("URL returned a CAPTCHA or browser challenge")
	}
	if response.StatusCode == http.StatusNetworkAuthenticationRequired ||
		(page.form && page.portalTitle) {
		return "portal", fmt.Errorf("URL returned network-access portal content")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "unexpected_status", fmt.Errorf("final URL status %d is not successful content", response.StatusCode)
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return "empty", fmt.Errorf("final URL response has no content")
	}
	if err := destination.Verify.check(body); err != nil {
		return "body_contract", fmt.Errorf("final URL body did not match its configured contract: %w", err)
	}
	return "content", nil
}
