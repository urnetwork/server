package monitor

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

const (
	providerJSONLimit       = 8 << 20
	providerSegmentLimit    = 64 << 20
	providerExpandedLimit   = 128 << 20
	providerRequestAttempts = 3
	providerRetryMaximum    = 30 * time.Second
)

type providerHTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

type providerRetryWait func(context.Context, time.Duration) error

const (
	providerAuthenticationClass = "provider-authentication"
	providerAPIClass            = "provider-api"
	providerDataClass           = "provider-data-invalid"
)

type providerVisibilityError struct {
	class   string
	message string
}

func (e *providerVisibilityError) Error() string { return e.message }

func (e *providerVisibilityError) monitorVisibilityClass() string { return e.class }

func newProviderVisibilityError(class, message string) error {
	return &providerVisibilityError{class: class, message: message}
}

func providerDataError(label string, err error) error {
	if err == nil {
		return nil
	}
	var classified interface{ monitorVisibilityClass() string }
	if errors.As(err, &classified) {
		return err
	}
	return newProviderVisibilityError(providerDataClass, label+": "+providerErrorText(err))
}

type providerPagination struct {
	seen  map[string]struct{}
	pages int
	limit int
}

// next validates opaque page tokens before another provider request. It
// catches repeated tokens and places a hard bound on a provider pagination
// loop without attempting to interpret the token itself.
func (p *providerPagination) next(token string) (bool, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return false, nil
	}
	if p.limit <= 0 {
		p.limit = 100
	}
	if p.pages >= p.limit {
		return false, fmt.Errorf("provider pagination exceeded %d pages", p.limit)
	}
	if p.seen == nil {
		p.seen = map[string]struct{}{}
	}
	if _, exists := p.seen[token]; exists {
		return false, fmt.Errorf("provider pagination repeated a page token")
	}
	p.seen[token] = struct{}{}
	p.pages++
	return true, nil
}

// providerHTTP is the reusable bounded external-report transport. It retries
// only idempotent report reads and query POSTs, never includes a URL (which may
// be a signed Apple segment URL) in an error, and caps every response body.
type providerHTTP struct {
	doer providerHTTPDoer
	wait providerRetryWait
}

func newProviderHTTP(doer providerHTTPDoer) *providerHTTP {
	if doer == nil {
		doer = &http.Client{Timeout: 30 * time.Second}
	}
	return &providerHTTP{doer: doer, wait: waitProviderRetry}
}

func providerSameOriginRedirect(request *http.Request, via []*http.Request) error {
	if len(via) == 0 || len(via) >= 10 {
		return fmt.Errorf("provider redirect chain is invalid or too long")
	}
	initial := via[0].URL
	redirect := request.URL
	if redirect.User != nil || redirect.Fragment != "" ||
		!strings.EqualFold(redirect.Scheme, initial.Scheme) || !strings.EqualFold(redirect.Host, initial.Host) {
		return fmt.Errorf("provider redirect changed origin")
	}
	return nil
}

func providerHTTPSDownloadRedirect(request *http.Request, via []*http.Request) error {
	if len(via) == 0 || len(via) >= 10 {
		return fmt.Errorf("provider download redirect chain is invalid or too long")
	}
	if request.URL.Scheme != "https" || request.URL.Host == "" || request.URL.User != nil || request.URL.Fragment != "" {
		return fmt.Errorf("provider download redirect is not a safe HTTPS URL")
	}
	return nil
}

func waitProviderRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (c *providerHTTP) json(
	ctx context.Context,
	method string,
	endpoint string,
	label string,
	headers http.Header,
	body []byte,
	out any,
) error {
	data, err := c.bytes(ctx, method, endpoint, label, headers, body, providerJSONLimit)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(out); err != nil {
		return newProviderVisibilityError(providerDataClass, fmt.Sprintf("%s: decode JSON: %s", label, providerErrorText(err)))
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return newProviderVisibilityError(providerDataClass, fmt.Sprintf("%s: trailing JSON value", label))
		}
		return newProviderVisibilityError(providerDataClass, fmt.Sprintf("%s: trailing JSON data: %s", label, providerErrorText(err)))
	}
	return nil
}

func (c *providerHTTP) bytes(
	ctx context.Context,
	method string,
	endpoint string,
	label string,
	headers http.Header,
	body []byte,
	limit int64,
) ([]byte, error) {
	if c == nil || c.doer == nil {
		return nil, newProviderVisibilityError(providerAPIClass, fmt.Sprintf("%s: HTTP client is unavailable", label))
	}
	if limit <= 0 {
		return nil, fmt.Errorf("%s: invalid response limit", label)
	}
	var lastErr error
	for attempt := 1; attempt <= providerRequestAttempts; attempt++ {
		request, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, newProviderVisibilityError(providerDataClass, fmt.Sprintf("%s: construct request: %s", label, providerErrorText(err)))
		}
		for name, values := range headers {
			for _, value := range values {
				request.Header.Add(name, value)
			}
		}
		response, err := c.doer.Do(request)
		if err != nil {
			if ctx.Err() != nil {
				return nil, fmt.Errorf("%s: request failed: %w", label, ctx.Err())
			}
			class := providerAPIClass
			lower := strings.ToLower(err.Error())
			if strings.Contains(lower, "oauth") || strings.Contains(lower, "token") || strings.Contains(lower, "sign app store") {
				class = providerAuthenticationClass
			}
			lastErr = newProviderVisibilityError(class, fmt.Sprintf("%s: request failed: %s", label, providerErrorText(err)))
			if attempt == providerRequestAttempts {
				return nil, lastErr
			}
			if err := c.retryWait(ctx, time.Duration(attempt)*time.Second); err != nil {
				return nil, fmt.Errorf("%s: retry interrupted: %w", label, err)
			}
			continue
		}

		data, readErr := io.ReadAll(io.LimitReader(response.Body, limit+1))
		closeErr := response.Body.Close()
		if readErr != nil {
			return nil, newProviderVisibilityError(providerAPIClass, fmt.Sprintf("%s: read response: %s", label, providerErrorText(readErr)))
		}
		if closeErr != nil {
			return nil, newProviderVisibilityError(providerAPIClass, fmt.Sprintf("%s: close response: %s", label, providerErrorText(closeErr)))
		}
		if int64(len(data)) > limit {
			return nil, newProviderVisibilityError(providerDataClass, fmt.Sprintf("%s: response exceeds %d bytes", label, limit))
		}
		if 200 <= response.StatusCode && response.StatusCode < 300 {
			return data, nil
		}

		class := providerAPIClass
		if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
			class = providerAuthenticationClass
		}
		lastErr = newProviderVisibilityError(class, fmt.Sprintf("%s: HTTP status %d", label, response.StatusCode))
		if attempt == providerRequestAttempts || !providerRetryableStatus(response.StatusCode) {
			return nil, lastErr
		}
		delay := providerRetryDelay(response.Header.Get("Retry-After"), time.Duration(attempt)*time.Second)
		if err := c.retryWait(ctx, delay); err != nil {
			return nil, fmt.Errorf("%s: retry interrupted: %w", label, err)
		}
	}
	return nil, lastErr
}

func (c *providerHTTP) retryWait(ctx context.Context, delay time.Duration) error {
	if c.wait == nil {
		return waitProviderRetry(ctx, delay)
	}
	return c.wait(ctx, delay)
}

func providerRetryableStatus(status int) bool {
	return status == http.StatusTooManyRequests || status == http.StatusRequestTimeout || status >= 500
}

func providerRetryDelay(value string, fallback time.Duration) time.Duration {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.Atoi(value); err == nil && seconds >= 0 {
		return min(time.Duration(seconds)*time.Second, providerRetryMaximum)
	}
	if when, err := http.ParseTime(value); err == nil {
		return min(max(time.Until(when), 0), providerRetryMaximum)
	}
	return min(fallback, providerRetryMaximum)
}

// providerNextURL accepts Apple JSON:API pagination links only when they stay
// on the configured API origin. Signed report downloads use a separate path
// and never inherit the authorization header.
func providerNextURL(baseURL, next string) (string, error) {
	if strings.TrimSpace(next) == "" {
		return "", nil
	}
	base, err := url.Parse(baseURL)
	if err != nil || base.User != nil || base.Fragment != "" {
		return "", fmt.Errorf("invalid provider base URL")
	}
	candidate, err := base.Parse(next)
	if err != nil || candidate.User != nil || candidate.Fragment != "" {
		return "", fmt.Errorf("invalid provider pagination link")
	}
	if candidate.Scheme != base.Scheme || candidate.Host != base.Host {
		return "", fmt.Errorf("provider pagination link changed origin")
	}
	return candidate.String(), nil
}

func verifyProviderChecksum(data []byte, expected string) error {
	expected = strings.ToLower(strings.TrimSpace(expected))
	var actual string
	switch len(expected) {
	case md5.Size * 2:
		digest := md5.Sum(data) // #nosec G401 -- provider-supplied integrity checksum, not security authentication.
		actual = hex.EncodeToString(digest[:])
	case sha256.Size * 2:
		digest := sha256.Sum256(data)
		actual = hex.EncodeToString(digest[:])
	default:
		return fmt.Errorf("unsupported provider checksum length %d", len(expected))
	}
	if actual != expected {
		return fmt.Errorf("provider checksum mismatch")
	}
	return nil
}

func expandProviderGzip(compressed []byte) ([]byte, error) {
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, fmt.Errorf("open gzip report: %w", err)
	}
	data, readErr := io.ReadAll(io.LimitReader(reader, providerExpandedLimit+1))
	closeErr := reader.Close()
	if readErr != nil {
		return nil, fmt.Errorf("read gzip report: %w", readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close gzip report: %w", closeErr)
	}
	if len(data) > providerExpandedLimit {
		return nil, fmt.Errorf("expanded report exceeds %d bytes", providerExpandedLimit)
	}
	return data, nil
}

type providerStateEnvelope struct {
	Version int             `json:"version"`
	Data    json.RawMessage `json:"data"`
}

type providerStateLock struct {
	file *os.File
}

// lockProviderState serializes a provider's complete read/query/write cycle
// across monitor processes. This matters during safe watcher promotion: an
// overlapping old and new process must not both read the same watermark and
// emit the same newly observed crash report.
func lockProviderState(ctx context.Context, stateDir, key string) (*providerStateLock, error) {
	if stateDir == "" {
		return &providerStateLock{}, nil
	}
	if !providerStateKeyPattern.MatchString(key) {
		return nil, fmt.Errorf("lock provider cursor: invalid key")
	}
	dir := filepath.Join(stateDir, "provider-reports")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("lock %s cursor: %w", key, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("lock %s cursor directory permissions: %w", key, err)
	}
	file, err := os.OpenFile(filepath.Join(dir, key+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("lock %s cursor: %w", key, err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("lock %s cursor permissions: %w", key, err)
	}
	for {
		err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return &providerStateLock{file: file}, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			_ = file.Close()
			return nil, fmt.Errorf("lock %s cursor: %w", key, err)
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			_ = file.Close()
			return nil, fmt.Errorf("lock %s cursor: %w", key, ctx.Err())
		case <-timer.C:
		}
	}
}

func (lock *providerStateLock) Close() error {
	if lock == nil || lock.file == nil {
		return nil
	}
	unlockErr := unix.Flock(int(lock.file.Fd()), unix.LOCK_UN)
	closeErr := lock.file.Close()
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}

func loadProviderState(stateDir, key string, version int, value any) (bool, error) {
	if stateDir == "" {
		return false, nil
	}
	if !providerStateKeyPattern.MatchString(key) {
		return false, fmt.Errorf("load provider cursor: invalid key")
	}
	path := filepath.Join(stateDir, "provider-reports", key+".json")
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("load %s cursor: %w", key, err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, providerJSONLimit+1))
	if err != nil {
		return false, fmt.Errorf("load %s cursor: %w", key, err)
	}
	if len(data) > providerJSONLimit {
		return false, fmt.Errorf("load %s cursor: exceeds %d bytes", key, providerJSONLimit)
	}
	var envelope providerStateEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return false, fmt.Errorf("load %s cursor: %w", key, err)
	}
	if envelope.Version != version {
		return false, fmt.Errorf("load %s cursor: unsupported version %d", key, envelope.Version)
	}
	if err := json.Unmarshal(envelope.Data, value); err != nil {
		return false, fmt.Errorf("load %s cursor data: %w", key, err)
	}
	return true, nil
}

func saveProviderState(stateDir, key string, version int, value any) error {
	if stateDir == "" {
		return nil
	}
	if !providerStateKeyPattern.MatchString(key) {
		return fmt.Errorf("save provider cursor: invalid key")
	}
	dir := filepath.Join(stateDir, "provider-reports")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("save %s cursor: %w", key, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("save %s cursor directory permissions: %w", key, err)
	}
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("save %s cursor data: %w", key, err)
	}
	envelope, err := json.Marshal(providerStateEnvelope{Version: version, Data: data})
	if err != nil {
		return fmt.Errorf("save %s cursor envelope: %w", key, err)
	}
	if len(envelope) > providerJSONLimit {
		return fmt.Errorf("save %s cursor: exceeds %d bytes", key, providerJSONLimit)
	}
	temporary, err := os.CreateTemp(dir, "."+key+".*")
	if err != nil {
		return fmt.Errorf("save %s cursor: %w", key, err)
	}
	temporaryPath := temporary.Name()
	keep := false
	defer func() {
		if !keep {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("save %s cursor permissions: %w", key, err)
	}
	if _, err := temporary.Write(append(envelope, '\n')); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("save %s cursor: %w", key, err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("save %s cursor: %w", key, err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("save %s cursor: %w", key, err)
	}
	if err := os.Rename(temporaryPath, filepath.Join(dir, key+".json")); err != nil {
		return fmt.Errorf("save %s cursor: %w", key, err)
	}
	keep = true
	return nil
}

var (
	providerStateKeyPattern   = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	providerResourceIDPattern = regexp.MustCompile(`^[A-Za-z0-9._~-]+$`)
	providerEmailPattern      = regexp.MustCompile(`(?i)\b[A-Z0-9._%+\-]+@[A-Z0-9.\-]+\.[A-Z]{2,}\b`)
	providerJWTPattern        = regexp.MustCompile(`\b[A-Za-z0-9_-]{16,}\.[A-Za-z0-9_-]{16,}\.[A-Za-z0-9_-]{16,}\b`)
	providerBearerPattern     = regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/=\-]+`)
	providerGoogleToken       = regexp.MustCompile(`\bya29\.[A-Za-z0-9._~\-]+`)
	providerHexPattern        = regexp.MustCompile(`(?i)\b[0-9a-f]{24,}\b`)
	providerURLPattern        = regexp.MustCompile(`https?://[^\s<>()]+`)
)

func providerErrorText(err error) string {
	if err == nil {
		return "unknown provider error"
	}
	value := strings.ToValidUTF8(err.Error(), "�")
	value = providerURLPattern.ReplaceAllString(value, "[redacted-provider-url]")
	value = providerBearerPattern.ReplaceAllString(value, "Bearer [redacted-token]")
	value = providerGoogleToken.ReplaceAllString(value, "[redacted-token]")
	value = providerJWTPattern.ReplaceAllString(value, "[redacted-token]")
	value = providerEmailPattern.ReplaceAllString(value, "[redacted-email]")
	value = providerHexPattern.ReplaceAllString(value, "[redacted-opaque-id]")
	value = strings.Map(func(r rune) rune {
		if r == '\t' || r >= 0x20 {
			return r
		}
		return ' '
	}, value)
	value = strings.TrimSpace(value)
	for len(value) > 1000 {
		_, size := utf8.DecodeLastRuneInString(value)
		value = value[:len(value)-size]
	}
	value = strings.Join(strings.Fields(providerEvidence(value)), " ")
	for len(value) > 1000 {
		_, size := utf8.DecodeLastRuneInString(value)
		value = value[:len(value)-size]
	}
	return strings.TrimSpace(value)
}

// providerEvidence keeps enough platform-produced stack text to identify the
// crashing frame while removing common identifiers, URL credentials, control
// characters, and Markdown fence injection. Google already sanitizes report
// text, but the monitor retains its own last-mile privacy boundary.
func providerEvidence(value string) string {
	value = strings.ToValidUTF8(value, "�")
	value = providerBearerPattern.ReplaceAllString(value, "Bearer [redacted-token]")
	value = providerGoogleToken.ReplaceAllString(value, "[redacted-token]")
	value = providerJWTPattern.ReplaceAllString(value, "[redacted-token]")
	value = providerEmailPattern.ReplaceAllString(value, "[redacted-email]")
	value = logIDRe.ReplaceAllString(value, "[redacted-id]")
	value = providerHexPattern.ReplaceAllString(value, "[redacted-opaque-id]")
	value = providerURLPattern.ReplaceAllStringFunc(value, func(raw string) string {
		core := strings.TrimRight(raw, "\"'.,;:!?)]}")
		suffix := raw[len(core):]
		query := strings.IndexByte(core, '?')
		if query < 0 {
			return raw
		}
		return core[:query] + "?redacted" + suffix
	})
	value = strings.ReplaceAll(value, "`", "'")
	value = strings.ReplaceAll(value, "&", "&amp;")
	value = strings.ReplaceAll(value, "<", "&lt;")
	value = strings.ReplaceAll(value, ">", "&gt;")
	value = strings.ReplaceAll(value, "[", "\\[")
	value = strings.ReplaceAll(value, "]", "\\]")
	lines := strings.Split(strings.ReplaceAll(value, "\r\n", "\n"), "\n")
	truncated := len(lines) > 40
	clean := make([]string, 0, min(len(lines), 40))
	for _, line := range lines {
		line = strings.Map(func(r rune) rune {
			if r == '\t' || r >= 0x20 {
				return r
			}
			return -1
		}, line)
		line = neutralizeProviderMarkdownLine(line)
		if len(clean) == 40 {
			break
		}
		clean = append(clean, line)
	}
	result := strings.TrimSpace(strings.Join(clean, "\n"))
	for len(result) > 4000 {
		truncated = true
		_, size := utf8.DecodeLastRuneInString(result)
		result = result[:len(result)-size]
	}
	if truncated {
		if result != "" {
			result += "\n"
		}
		result += "[report truncated]"
	}
	return result
}

func neutralizeProviderMarkdownLine(line string) string {
	offset := len(line) - len(strings.TrimLeft(line, " \t"))
	content := line[offset:]
	markdownPrefix := strings.HasPrefix(content, "#") || strings.HasPrefix(content, ">") ||
		strings.HasPrefix(content, "---") || strings.HasPrefix(content, "***") ||
		strings.HasPrefix(content, "- ") || strings.HasPrefix(content, "* ") || strings.HasPrefix(content, "+ ")
	if markdownPrefix {
		return line[:offset] + "\\" + content
	}
	return line
}

func providerLabel(value string) string {
	value = strings.Join(strings.Fields(providerEvidence(value)), " ")
	for len(value) > 200 {
		_, size := utf8.DecodeLastRuneInString(value)
		value = value[:len(value)-size]
	}
	return strings.TrimSpace(value)
}
