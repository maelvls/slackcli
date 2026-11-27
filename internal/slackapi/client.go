// Package slackapi is a minimal Slack Web API client that works with both
// standard tokens (xoxb/xoxp) and browser session credentials (xoxc token +
// xoxd "d" cookie). Responses are returned as loosely-typed JSON; see
// package jsonx for accessors.
package slackapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/maelvls/slackcli/internal/appinfo"
	"github.com/maelvls/slackcli/internal/audit"
	"github.com/maelvls/slackcli/internal/trace"
)

// AuthType distinguishes standard tokens from browser session credentials.
type AuthType string

const (
	AuthStandard AuthType = "standard"
	AuthBrowser  AuthType = "browser"
)

// Auth is a set of Slack credentials. For AuthStandard only Token is set;
// for AuthBrowser XoxcToken and XoxdCookie are set. XoxdCookie is stored
// URL-decoded.
type Auth struct {
	Type       AuthType `json:"auth_type"`
	Token      string   `json:"token,omitempty"`
	XoxcToken  string   `json:"xoxc_token,omitempty"`
	XoxdCookie string   `json:"xoxd_cookie,omitempty"`
}

// IsBrowser reports whether these are browser session credentials.
func (a Auth) IsBrowser() bool { return a.Type == AuthBrowser }

// BearerToken is the token to present, whatever the auth type.
func (a Auth) BearerToken() string {
	if a.IsBrowser() {
		return a.XoxcToken
	}
	return a.Token
}

// Handler serves API calls in place of HTTP; used by tests (see NewFake).
type Handler func(ctx context.Context, method string, params map[string]any) (map[string]any, error)

const (
	defaultAPITimeout  = 20 * time.Second
	defaultBaseURL     = "https://slack.com"
	maxRateLimitDelay  = 30 * time.Second
	maxRateLimitRetry  = 3
	maxResponseBodyLen = 64 << 20
)

// LegacyAPITimeoutEnv is the variable the request timeout was first read
// from. It keeps its name, and is read when the tool-specific
// API_TIMEOUT_MS variable gives no usable value.
const LegacyAPITimeoutEnv = "SLACK_API_TIMEOUT_MS"

// Client calls Slack Web API methods.
type Client struct {
	auth         Auth
	workspaceURL string
	baseURL      string
	httpClient   *http.Client
	// apiHTTP is httpClient with redirects turned off, for the Web API
	// itself (see noRedirects).
	apiHTTP *http.Client
	handler Handler
	sleep   func(ctx context.Context, d time.Duration) error
	// loopbackForTesting lets credentials go to a loopback address (see
	// WithLoopbackWorkspaceForTesting).
	loopbackForTesting bool
	// fallback retries some refused calls with other credentials (see
	// WithFallback).
	fallback *Fallback
}

// Fallback retries a refused call with the credentials of another client.
//
// It exists for Enterprise Grid: a few methods (drafts.list,
// drafts.delete …) refuse the session of a workspace with
// team_is_restricted and accept the session of the organisation the
// workspace belongs to, which is the same person signed in one level up.
type Fallback struct {
	// Applies reports whether a call to method that failed with err is to
	// be retried with the other client.
	Applies func(method string, err error) bool
	// Client returns the other client. When there is none, it returns nil
	// and, if it knows, why: a sentence that takes the place of the
	// general explanation of the code in the error (APIError.Hint). It is
	// only called once a call was refused, and may be called again for
	// the next one: it is expected to remember its answer.
	Client func(ctx context.Context) (other *Client, why string)
}

// WithFallback makes the client retry the calls f applies to with the
// client f provides. The retry is one call: the fallback of a client used
// as a fallback is not looked at.
//
// When the other client is refused as well (its credentials are rejected,
// or the method is restricted for it too), the error is the refusal of the
// client that was asked, and says what was tried. Any other failure of the
// retry is returned as it is.
func WithFallback(f Fallback) Option {
	return func(c *Client) { c.fallback = &f }
}

// Option customises a Client.
type Option func(*Client)

// WithHTTPClient sets the HTTP client (tests, proxies).
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.httpClient = h } }

// WithBaseURL overrides https://slack.com for standard-token calls (tests).
func WithBaseURL(u string) Option {
	return func(c *Client) { c.baseURL = strings.TrimRight(u, "/") }
}

// WithHandler short-circuits API and APIMultipart to fn (tests).
func WithHandler(fn Handler) Option { return func(c *Client) { c.handler = fn } }

// WithSleep replaces the rate-limit back-off sleep (tests).
func WithSleep(fn func(ctx context.Context, d time.Duration) error) Option {
	return func(c *Client) { c.sleep = fn }
}

// WithLoopbackWorkspaceForTesting is for tests only: it lets a client send
// its credentials to a workspace URL on a loopback address (an
// net/http/httptest server such as http://127.0.0.1:1234), over http or
// https. Without it, browser credentials are only ever sent to an https
// Slack host. It never allows a non-loopback host, and nothing in the CLI
// (flag, environment variable, configuration) can turn it on.
func WithLoopbackWorkspaceForTesting() Option {
	return func(c *Client) { c.loopbackForTesting = true }
}

// New returns a client for auth. workspaceURL (e.g.
// "https://acme.slack.com") is required for browser auth and optional for
// standard tokens.
//
// Browser credentials (xoxc token + "d" cookie) are sent to the workspace
// URL itself, so the client refuses to call the API when that URL is not an
// https Slack workspace origin (see WorkspaceOrigin): the call fails before
// anything is sent. Standard tokens always go to https://slack.com.
func New(auth Auth, workspaceURL string, opts ...Option) *Client {
	c := &Client{
		auth:         auth,
		workspaceURL: strings.TrimRight(workspaceURL, "/"),
		baseURL:      defaultBaseURL,
		httpClient:   &http.Client{},
		sleep:        sleepCtx,
	}
	for _, o := range opts {
		o(c)
	}
	if c.httpClient == nil {
		c.httpClient = &http.Client{}
	}
	c.apiHTTP = noRedirects(c.httpClient)
	return c
}

// noRedirects returns a copy of h that hands a 3xx response back instead of
// following it. A Web API call is a POST that carries the credentials (the
// xoxc token is a form field): on a 307 or 308 net/http would replay that
// body to whatever host and scheme the Location names, and it only strips
// the Cookie and Authorization headers, not the body. The Web API never
// redirects, so a redirect is an error (see redirectError) rather than
// something to follow, even to another Slack host. It is a copy so that
// the policy holds for a client given with WithHTTPClient and does not
// leak into HTTPClient, whose users have their own.
func noRedirects(h *http.Client) *http.Client {
	c := *h
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &c
}

// NewFake returns a browser-auth client for workspace
// https://test.slack.com whose API calls are served by fn.
func NewFake(fn Handler, opts ...Option) *Client {
	auth := Auth{Type: AuthBrowser, XoxcToken: "xoxc-test", XoxdCookie: "xoxd-test"}
	return New(auth, "https://test.slack.com", append([]Option{WithHandler(fn)}, opts...)...)
}

// Auth returns the client's credentials.
func (c *Client) Auth() Auth { return c.auth }

// WorkspaceURL returns the workspace origin, without trailing slash ("" if
// unknown).
func (c *Client) WorkspaceURL() string { return c.workspaceURL }

// HTTPClient returns the underlying HTTP client, for non-API requests such
// as file downloads and uploads. It follows redirects as configured (by
// default, as net/http does): a caller that sends anything worth protecting
// sets its own CheckRedirect on a copy.
func (c *Client) HTTPClient() *http.Client { return c.httpClient }

// Authorize adds credentials and the User-Agent to a non-API request (file
// download/upload). Only call it for requests to Slack-owned hosts: as a
// last line of defence, a request that is not https to a Slack host
// (IsSlackHost) only gets the User-Agent, never the token or the cookie.
func (c *Client) Authorize(req *http.Request) {
	req.Header.Set("User-Agent", appinfo.UserAgent())
	if !c.mayReceiveCredentials(req.URL) {
		return
	}
	if t := c.auth.BearerToken(); t != "" {
		req.Header.Set("Authorization", "Bearer "+t)
	}
	if c.auth.IsBrowser() {
		req.Header.Set("Cookie", "d="+EncodeCookieD(c.auth.XoxdCookie))
	}
}

// WebSocketHeader returns the headers to open the WebSocket at u with, as
// Authorize does for an HTTP request: the User-Agent, and for a browser
// session the Origin of the Slack client and the "d" cookie. It refuses a
// URL that credentials must not be sent to: anything but wss to a Slack
// host (IsSlackHost). The URL comes from Slack (rtm.connect) and carries a
// token of its own, so it is not trusted more than a workspace URL.
func (c *Client) WebSocketHeader(u *url.URL) (http.Header, error) {
	if u == nil {
		return nil, errors.New("missing WebSocket URL")
	}
	allowed := u.Scheme == "wss" && u.User == nil && IsSlackHost(u.Hostname())
	if !allowed && c.loopbackForTesting && (u.Scheme == "ws" || u.Scheme == "wss") {
		allowed = isLoopbackURL(&url.URL{Scheme: "http", Host: u.Host})
	}
	if !allowed {
		return nil, fmt.Errorf("refusing to open the WebSocket at %s://%s: it is not a wss address of a Slack host, and the credentials are only sent to Slack", u.Scheme, u.Host)
	}
	h := http.Header{}
	h.Set("User-Agent", appinfo.UserAgent())
	if c.auth.IsBrowser() {
		h.Set("Origin", "https://app.slack.com")
		h.Set("Cookie", "d="+EncodeCookieD(c.auth.XoxdCookie))
	}
	return h, nil
}

// APITimeout is the per-request timeout (SLACKCLI_API_TIMEOUT_MS, legacy
// SLACK_API_TIMEOUT_MS; default 20s).
func APITimeout() time.Duration {
	return appinfo.EnvDurationMS("API_TIMEOUT_MS", legacyAPITimeout(), false)
}

// legacyAPITimeout reads LegacyAPITimeoutEnv by the rule of
// appinfo.EnvDurationMS: a value that cannot be used gives the default,
// and is reported by the root command (CheckLegacyAPITimeout), not here.
func legacyAPITimeout() time.Duration {
	raw := strings.TrimSpace(getenv(LegacyAPITimeoutEnv))
	if raw == "" {
		return defaultAPITimeout
	}
	d, err := appinfo.ParseDurationMS(raw, false)
	if err != nil {
		return defaultAPITimeout
	}
	return d
}

// CheckLegacyAPITimeout says what is wrong with the value of
// LegacyAPITimeoutEnv, as appinfo.CheckEnvDurationMS does for the
// tool-specific variables. It returns nil when the variable is unset or
// valid, and when APITimeout does not read it because the tool-specific
// variable has a valid value.
func CheckLegacyAPITimeout() error {
	raw := strings.TrimSpace(getenv(LegacyAPITimeoutEnv))
	if raw == "" {
		return nil
	}
	if _, current := appinfo.EnvSource("API_TIMEOUT_MS"); current != "" && appinfo.CheckEnvDurationMS("API_TIMEOUT_MS", false) == nil {
		return nil
	}
	if _, err := appinfo.ParseDurationMS(raw, false); err != nil {
		return fmt.Errorf("%s=%q is not valid: %w", LegacyAPITimeoutEnv, raw, err)
	}
	return nil
}

// RateLimitMaxWait is how long the client is willing to sleep on a rate limit
// before retrying (SLACKCLI_RATE_LIMIT_MAX_WAIT_MS; default 0 = fail fast).
func RateLimitMaxWait() time.Duration {
	return appinfo.EnvDurationMS("RATE_LIMIT_MAX_WAIT_MS", 0, true)
}

// API calls a Web API method with form-encoded parameters. Nil parameters
// are dropped; strings are sent as-is; bools and numbers are stringified;
// everything else (maps, slices, structs) is JSON-encoded.
//
// A response with ok=false is returned as an *APIError, except a rate
// limit: an HTTP 429 or the "ratelimited" (or "rate_limited", see
// PostingRateLimitedCode) error code is a *RateLimitError
// (which wraps the *APIError of the code).
func (c *Client) API(ctx context.Context, method string, params map[string]any) (map[string]any, error) {
	return c.call(ctx, method, params, false)
}

// APIMultipart is like API but uses multipart/form-data. Some internal
// Slack methods (e.g. saved.update) silently ignore form-encoded parameters.
func (c *Client) APIMultipart(ctx context.Context, method string, params map[string]any) (map[string]any, error) {
	return c.call(ctx, method, params, true)
}

type noRateLimitRetryKey struct{}

// WithoutRateLimitRetry makes the calls made under the returned context
// fail at once with a *RateLimitError when Slack rate limits them,
// whatever SLACKCLI_RATE_LIMIT_MAX_WAIT_MS allows. It is for a caller that
// has a retry policy of its own (see Client.Sleep): the two would
// otherwise multiply, each try of the caller being up to four of the
// client.
func WithoutRateLimitRetry(ctx context.Context) context.Context {
	return context.WithValue(ctx, noRateLimitRetryKey{}, true)
}

// RateLimitRetryDisabled reports whether ctx was made by
// WithoutRateLimitRetry.
func RateLimitRetryDisabled(ctx context.Context) bool {
	return ctx.Value(noRateLimitRetryKey{}) != nil
}

// Sleep waits for d, or until ctx is done, in which case it returns the
// context's error. It is the wait the client itself makes when Slack rate
// limits a call (tests replace it with WithSleep), for the callers that
// have a retry of their own to space out.
func (c *Client) Sleep(ctx context.Context, d time.Duration) error {
	span := trace.Begin("wait", d.String())
	err := c.sleep(ctx, d)
	span.EndErr(err)
	return err
}

// call makes the call and, when it is refused in a way the fallback
// applies to, makes it once more with the fallback's client.
func (c *Client) call(ctx context.Context, method string, params map[string]any, multi bool) (map[string]any, error) {
	resp, err := c.callOnce(ctx, method, params, multi)
	if err == nil || c.fallback == nil || c.fallback.Applies == nil || c.fallback.Client == nil || !c.fallback.Applies(method, err) {
		return resp, err
	}
	other, why := c.fallback.Client(ctx)
	if ctx.Err() != nil {
		// Looking for the other client may take calls of its own. When
		// the command was interrupted or timed out meanwhile, that is what
		// happened to this call: the refusal would hide it.
		return nil, ctx.Err()
	}
	if other == nil || other == c {
		return nil, withHint(err, why)
	}
	trace.Logf("%s was refused for %s (%s): retrying with %s", method, c.workspaceURL, ErrorCode(err), other.workspaceURL)
	resp, otherErr := other.callOnce(ctx, method, params, multi)
	switch {
	case otherErr == nil:
		return resp, nil
	case ctx.Err() != nil:
		return nil, otherErr
	case IsAuthError(otherErr):
		// The other credentials are of no use: the refusal of the
		// workspace that was asked for is the answer, with what would
		// make the retry work.
		return nil, withHint(err, fmt.Sprintf("these credentials are refused this method, and the ones stored for %s, tried next, were rejected by Slack (%s): import them again with `%s auth import-desktop` (or import-chrome, import-brave, import-firefox)",
			other.workspaceURL, ErrorCode(otherErr), appinfo.Name))
	case IsRestricted(otherErr):
		return nil, withHint(err, fmt.Sprintf("these credentials are refused this method, and so are the ones stored for %s, tried next (%s)",
			other.workspaceURL, ErrorCode(otherErr)))
	}
	return nil, otherErr
}

func (c *Client) callOnce(ctx context.Context, method string, params map[string]any, multi bool) (map[string]any, error) {
	if c.handler != nil {
		span := trace.Begin("api "+method, "")
		rec := c.auditEntry(method, c.workspaceURL, 1)
		resp, err := c.handler(ctx, method, params)
		span.EndErr(err)
		if err != nil {
			code := ErrorCode(err)
			var limited *RateLimitError
			switch {
			case errors.As(err, &limited):
				rec.entry.RetryAfterS = limited.RetryAfter.Seconds()
				rec.entry.RateLimited = true
				code = "ratelimited"
			case code == "":
				code = audit.ErrorClass(ctx, err)
			}
			rec.done(0, nil, code)
			return nil, err
		}
		if resp == nil {
			resp = map[string]any{"ok": true}
		}
		if code, _ := resp["error"].(string); IsRateLimitCode(code) && resp["ok"] != true {
			// The rate limit a handler answers in the body, as Slack
			// may: the same error as an HTTP 429 (see the loop below).
			rec.done(0, resp, code)
			return nil, &RateLimitError{Method: method, RetryAfter: parseRetryAfter(""), MaxWait: RateLimitMaxWait(), API: newAPIError(method, resp)}
		}
		if ok, present := resp["ok"]; present && ok != true {
			apiErr := newAPIError(method, resp)
			rec.done(0, resp, apiErr.Code)
			return nil, apiErr
		}
		rec.done(0, resp, "")
		return resp, nil
	}

	endpoint, err := c.endpoint(method)
	if err != nil {
		return nil, err
	}
	fields, err := EncodeParams(params)
	if err != nil {
		return nil, fmt.Errorf("encoding parameters for %s: %w", method, err)
	}
	if c.auth.IsBrowser() {
		fields["token"] = c.auth.XoxcToken
	}

	timeout := APITimeout()
	for attempt := 0; ; attempt++ {
		detail := ""
		if attempt > 0 {
			detail = fmt.Sprintf("(attempt %d)", attempt+1)
		}
		span := trace.Begin("api "+method, detail)
		rec := c.auditEntry(method, endpoint, attempt+1)
		status, header, body, err := c.post(ctx, endpoint, fields, multi, timeout)
		if err != nil {
			if ctx.Err() != nil {
				span.Fail("cancelled: " + ctx.Err().Error())
				rec.done(0, nil, "cancelled")
				return nil, ctx.Err()
			}
			var ne interface{ Timeout() bool }
			if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
				span.Fail("no answer after " + timeout.String())
				rec.done(0, nil, "timeout")
				return nil, &TimeoutError{Method: method, Timeout: timeout}
			}
			span.Fail("network error")
			rec.done(0, nil, "network")
			return nil, &NetworkError{Method: method, Err: err}
		}

		if status == http.StatusTooManyRequests {
			retryAfter := parseRetryAfter(header.Get("Retry-After"))
			maxWait := RateLimitMaxWait()
			span.Fail(fmt.Sprintf("HTTP 429, Retry-After %s", retryAfter))
			rec.rateLimited(retryAfter)
			if err := c.retryRateLimited(ctx, method, attempt, retryAfter, maxWait, nil); err != nil {
				return nil, err
			}
			continue
		}
		// The span ends as the call does: a failure for everything that
		// is returned as an error.
		if status >= 300 && status <= 399 {
			span.Fail(fmt.Sprintf("HTTP %d, a redirect", status))
			rec.done(status, nil, "redirect")
			return nil, c.redirectError(method, status, header.Get("Location"))
		}

		var data map[string]any
		_ = json.Unmarshal(body, &data)
		code, _ := data["error"].(string)
		switch {
		case data == nil && (status < 200 || status > 299):
			rec.done(status, nil, "http_status")
		case data == nil:
			rec.done(status, nil, "not_json")
		default:
			rec.done(status, data, code)
		}
		switch {
		case IsRateLimitCode(code) && data["ok"] != true:
			// Some methods answer a rate limit with an HTTP 200 (or
			// another status) and one of these codes instead of an HTTP
			// 429: it is the same refusal, waited for and retried the same
			// way. Retrying is safe for a write too: an ok=false answer
			// means it was not done (chat.postMessage's rate_limited: the
			// message was not posted).
			retryAfter := parseRetryAfter(header.Get("Retry-After"))
			maxWait := RateLimitMaxWait()
			span.Fail(fmt.Sprintf("HTTP %d, %s, Retry-After %s", status, code, retryAfter))
			apiErr := newAPIError(method, data)
			apiErr.Status = status
			if err := c.retryRateLimited(ctx, method, attempt, retryAfter, maxWait, apiErr); err != nil {
				return nil, err
			}
			continue
		case status < 200 || status > 299:
			if code != "" {
				span.Fail(fmt.Sprintf("HTTP %d, %s", status, code))
				apiErr := newAPIError(method, data)
				apiErr.Status = status
				return nil, apiErr
			}
			span.Fail(fmt.Sprintf("HTTP %d", status))
			return nil, &HTTPStatusError{Method: method, Status: status}
		case data == nil:
			span.Fail(fmt.Sprintf("HTTP %d, not JSON", status))
			return nil, fmt.Errorf("Slack API %s returned a non-JSON response", method)
		case data["ok"] != true:
			if code == "" {
				code = "not ok"
			}
			span.Fail(fmt.Sprintf("HTTP %d, %s", status, code))
			apiErr := newAPIError(method, data)
			apiErr.Status = status
			return nil, apiErr
		}
		span.End(fmt.Sprintf("HTTP %d", status))
		return data, nil
	}
}

// retryRateLimited is what to do with an attempt that Slack rate limited
// and asked to retry after retryAfter: it waits, and returns nil for the
// call to be made again, unless the wait exceeds maxWait, the attempts
// are used up or ctx asks not to retry (WithoutRateLimitRetry). It then
// returns the *RateLimitError to fail the call with, which wraps apiErr
// when the rate limit came as an error code (nil for an HTTP 429).
func (c *Client) retryRateLimited(ctx context.Context, method string, attempt int, retryAfter, maxWait time.Duration, apiErr *APIError) error {
	if attempt >= maxRateLimitRetry || retryAfter > maxWait || RateLimitRetryDisabled(ctx) {
		return &RateLimitError{Method: method, RetryAfter: retryAfter, MaxWait: maxWait, API: apiErr}
	}
	wait := trace.Begin("rate-limit wait", method)
	err := c.sleep(ctx, retryAfter)
	wait.EndErr(err)
	return err
}

// auditCall is the line of the audit log of one attempt of a call; done
// or rateLimited writes it.
type auditCall struct {
	start time.Time
	entry audit.Entry
}

// auditEntry starts the audit line of an attempt (from 1) of a call to
// method at target, a URL of which only the host is kept.
func (c *Client) auditEntry(method, target string, attempt int) *auditCall {
	host := ""
	if u, err := url.Parse(target); err == nil {
		host = u.Host
	}
	now := time.Now()
	return &auditCall{start: now, entry: audit.Entry{
		TS:      now.UTC().Format(time.RFC3339Nano),
		Kind:    audit.KindAPI,
		Host:    host,
		Method:  method,
		Attempt: attempt,
	}}
}

// done writes the line of an attempt that got the HTTP status (0 when
// there was no answer, or the call was served by a Handler), the answer
// data (nil when it had none) and the error code (Slack's, or how the
// request failed; "" for none).
func (a *auditCall) done(status int, data map[string]any, code string) {
	a.entry.DurationMS = time.Since(a.start).Milliseconds()
	a.entry.Status = status
	if ok, present := data["ok"].(bool); present {
		a.entry.OK = &ok
	}
	a.entry.Error = code
	audit.Record(a.entry)
}

// rateLimited writes the line of an attempt that Slack answered with HTTP
// 429.
func (a *auditCall) rateLimited(retryAfter time.Duration) {
	a.entry.RateLimited = true
	a.entry.RetryAfterS = retryAfter.Seconds()
	a.done(http.StatusTooManyRequests, nil, "ratelimited")
}

func (c *Client) endpoint(method string) (string, error) {
	if c.auth.IsBrowser() {
		if c.workspaceURL == "" {
			return "", errors.New("browser auth (xoxc token) requires a workspace URL: pass --workspace, set SLACK_WORKSPACE_URL, or use a Slack message URL as the target")
		}
		if err := c.checkWorkspaceURL(); err != nil {
			return "", err
		}
		return c.workspaceURL + "/api/" + method, nil
	}
	if c.auth.Token == "" {
		return "", errors.New("missing Slack token")
	}
	return c.baseURL + "/api/" + method, nil
}

// checkWorkspaceURL refuses a workspace URL that browser credentials must
// not be sent to. It is the client's own guard: callers are expected to
// have validated the URL already (auth.NormalizeURL).
func (c *Client) checkWorkspaceURL() error {
	if origin, err := WorkspaceOrigin(c.workspaceURL); err == nil && origin == strings.ToLower(c.workspaceURL) {
		return nil
	}
	if u, err := url.Parse(c.workspaceURL); err == nil && c.loopbackForTesting && isLoopbackURL(u) {
		return nil
	}
	return fmt.Errorf("refusing to send browser credentials to %q: a workspace URL must be an https Slack origin such as https://acme.slack.com", c.workspaceURL)
}

// redirectError is returned when Slack (or whatever answers at the
// workspace URL) redirects an API call. The redirect is not followed (see
// noRedirects). The message names where it pointed by origin only: neither
// the response body nor the path and query of the Location, which may echo
// the request.
func (c *Client) redirectError(method string, status int, location string) error {
	target := "an unspecified location"
	if u, err := url.Parse(location); err == nil && u.Host != "" {
		target = u.Scheme + "://" + u.Host
	} else if location != "" {
		target = "another path of the same host"
	}
	sentTo := c.baseURL
	if c.auth.IsBrowser() {
		sentTo = c.workspaceURL
	}
	return fmt.Errorf("Slack API call %s to %s was answered with a redirect (HTTP %d) to %s, which was not followed: the Slack API never redirects, and following it would send the credentials there. "+
		"The workspace URL is probably wrong or out of date (a renamed workspace redirects to its new address): list the configured workspaces with `%s auth whoami`, "+
		"then pass the right one with --workspace, or import it again with `%s auth import-desktop`",
		method, sentTo, status, target, appinfo.Name, appinfo.Name)
}

// mayReceiveCredentials reports whether a non-API request to u may carry
// the token and the cookie.
func (c *Client) mayReceiveCredentials(u *url.URL) bool {
	if u == nil {
		return false
	}
	if u.Scheme == "https" && u.User == nil && IsSlackHost(u.Hostname()) {
		return true
	}
	return c.loopbackForTesting && isLoopbackURL(u)
}

// readOnlyVerbs are the last segments of API method names that only read
// ("conversations.history", "users.info"…).
var readOnlyVerbs = map[string]bool{
	"list": true, "info": true, "history": true, "replies": true, "test": true,
	"get": true, "lookup": true, "counts": true, "members": true, "search": true,
}

// isReadOnlyMethod reports whether an API method obviously changes nothing,
// judging by its name. When in doubt it answers false.
func isReadOnlyMethod(method string) bool {
	if strings.HasPrefix(method, "search.") {
		return true
	}
	return readOnlyVerbs[method[strings.LastIndex(method, ".")+1:]]
}

func (c *Client) post(ctx context.Context, endpoint string, fields map[string]string, multi bool, timeout time.Duration) (int, http.Header, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var body io.Reader
	var contentType string
	if multi {
		var buf bytes.Buffer
		w := multipart.NewWriter(&buf)
		for _, k := range sortedKeys(fields) {
			if err := w.WriteField(k, fields[k]); err != nil {
				return 0, nil, nil, err
			}
		}
		if err := w.Close(); err != nil {
			return 0, nil, nil, err
		}
		body, contentType = &buf, w.FormDataContentType()
	} else {
		form := url.Values{}
		for k, v := range fields {
			form.Set(k, v)
		}
		body, contentType = strings.NewReader(form.Encode()), "application/x-www-form-urlencoded"
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		return 0, nil, nil, err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("User-Agent", appinfo.UserAgent())
	if c.auth.IsBrowser() {
		req.Header.Set("Cookie", "d="+EncodeCookieD(c.auth.XoxdCookie))
		req.Header.Set("Origin", "https://app.slack.com")
	} else {
		req.Header.Set("Authorization", "Bearer "+c.auth.Token)
	}

	resp, err := c.apiHTTP.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode <= 399 {
		// Not read: the body of a redirect is of no use, and must not end
		// up in an error message.
		return resp.StatusCode, resp.Header, nil, nil
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodyLen))
	if err != nil {
		return 0, nil, nil, err
	}
	return resp.StatusCode, resp.Header, data, nil
}

// EncodeParams converts API parameters to form fields (see Client.API).
func EncodeParams(params map[string]any) (map[string]string, error) {
	out := make(map[string]string, len(params)+1)
	for k, v := range params {
		s, ok, err := encodeParam(v)
		if err != nil {
			return nil, fmt.Errorf("parameter %q: %w", k, err)
		}
		if ok {
			out[k] = s
		}
	}
	return out, nil
}

func encodeParam(v any) (string, bool, error) {
	switch x := v.(type) {
	case nil:
		return "", false, nil
	case string:
		return x, true, nil
	case bool:
		return strconv.FormatBool(x), true, nil
	case int:
		return strconv.Itoa(x), true, nil
	case int64:
		return strconv.FormatInt(x, 10), true, nil
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64), true, nil
	case json.Number:
		return x.String(), true, nil
	case fmt.Stringer:
		return x.String(), true, nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", false, err
	}
	s := strings.TrimSuffix(buf.String(), "\n")
	if s == "null" {
		return "", false, nil
	}
	return s, true, nil
}

// EncodeCookieD percent-encodes a decoded xoxd cookie value for use in a
// Cookie header (JavaScript's encodeURIComponent).
func EncodeCookieD(v string) string {
	return strings.ReplaceAll(url.QueryEscape(v), "+", "%20")
}

// DecodeCookieD normalises a "d" cookie value to its decoded form. Values
// copied from a browser are percent-encoded ("xoxd-abc%2Bdef"); values that
// are already decoded (or not valid percent-encoding) are returned as-is.
func DecodeCookieD(v string) string {
	v = strings.TrimSpace(v)
	if !strings.Contains(v, "%") {
		return v
	}
	if d, err := url.PathUnescape(v); err == nil {
		return d
	}
	return v
}

func parseRetryAfter(h string) time.Duration {
	secs, err := strconv.ParseFloat(strings.TrimSpace(h), 64)
	switch {
	case err != nil || math.IsNaN(secs):
		secs = 5
	case secs < 1:
		secs = 1
	}
	// Capped before the conversion: what an out-of-range float (an absurd
	// or infinite Retry-After) converts to depends on the architecture, and
	// a negative delay would retry at once.
	secs = min(secs, maxRateLimitDelay.Seconds())
	return time.Duration(secs * float64(time.Second))
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
