package keenable

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultBaseURL is the Keenable API used when Options.BaseURL and
	// KEENABLE_API_URL are both unset.
	DefaultBaseURL = "https://api.keenable.ai"
	// DefaultContextMaxChars is the ToContext budget: about 3k tokens, enough
	// for a handful of results without crowding out the rest of the prompt.
	DefaultContextMaxChars = 12_000
	// DefaultTimeout is the deadline for one request, including the body.
	DefaultTimeout = 30 * time.Second

	defaultClientSource = "Keenable SDK"
)

// Options configures a [Client].
//
// The zero value, and a nil pointer passed to [New], reads KEENABLE_API_KEY
// and KEENABLE_API_URL and otherwise calls the keyless public API.
//
// APIKey and BaseURL are pointers so "unset" and "explicitly empty" differ.
// Nil reads the environment. A pointer to an empty APIKey forces the keyless
// tier even when KEENABLE_API_KEY is set. A pointer to an empty BaseURL is
// rejected. A Go literal cannot be addressed, so put the string in a variable:
//
//	key := "keen_..."
//	client, err := keenable.New(&keenable.Options{APIKey: &key})
type Options struct {
	APIKey *string
	// BaseURL overrides KEENABLE_API_URL. Must be https, except http to
	// localhost, 127.0.0.1, or ::1.
	BaseURL *string
	// Timeout is the deadline for headers and body. Zero means [DefaultTimeout].
	// A caller context that ends sooner wins. The client deadline surfaces as
	// [ConnectionError]; a canceled context is returned as ctx.Err().
	Timeout time.Duration
	// ClientSource is sent as X-Keenable-Title. Empty means "Keenable SDK".
	// Set it when you wrap this SDK and want your own traffic attribution.
	ClientSource string
	// HTTPClient replaces the owned client (proxies, custom transports).
	// Its Timeout is left alone, and [Client.Close] does not close it.
	HTTPClient *http.Client
}

// Client calls the Keenable web search API.
//
// Keyless by default: with no API key it uses the public endpoints, which are
// rate limited per hour. Pass an API key, or set KEENABLE_API_KEY, to lift
// that limit. Create a key at https://keenable.ai/console.
//
// Reuse one Client for the process. Call [Client.Close] when you are done if
// this SDK created the underlying [http.Client].
type Client struct {
	apiKey       string
	baseURL      string
	clientSource string
	timeout      time.Duration
	http         *http.Client
	ownsClient   bool
}

// New builds a client. A nil opts reads the environment and uses the defaults.
func New(opts *Options) (*Client, error) {
	if opts == nil {
		opts = &Options{}
	}
	base, err := resolveBaseURL(opts.BaseURL)
	if err != nil {
		return nil, err
	}
	source := opts.ClientSource
	if source == "" {
		source = defaultClientSource
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	c := &Client{
		apiKey:       resolveAPIKey(opts.APIKey),
		baseURL:      base,
		clientSource: source,
		timeout:      timeout,
	}
	if opts.HTTPClient != nil {
		c.http = opts.HTTPClient
		// The supplied client's Timeout is the only deadline this SDK did not
		// create. Name that one in errors. Zero means it has no deadline.
		c.timeout = opts.HTTPClient.Timeout
		return c, nil
	}
	c.http = &http.Client{Timeout: timeout}
	c.ownsClient = true
	return c, nil
}

// Close releases idle connections on the client this SDK created.
// It does nothing when Options.HTTPClient was provided.
func (c *Client) Close() {
	if c != nil && c.ownsClient && c.http != nil {
		c.http.CloseIdleConnections()
	}
}

// Keyless reports whether the public tier is in use.
// A nil client reports false.
func (c *Client) Keyless() bool { return c != nil && c.apiKey == "" }

// Search queries the web and returns ranked pages with extracted text.
//
// Describe the ideal page in natural language ("blog post comparing React and
// Vue performance") rather than keywords. The index is semantic.
// opts may be nil.
func (c *Client) Search(ctx context.Context, query string, opts *SearchOptions) (*SearchResponse, error) {
	if err := requireClient(c); err != nil {
		return nil, err
	}
	if err := requireContext(ctx); err != nil {
		return nil, err
	}
	if strings.TrimSpace(query) == "" {
		return nil, &InvalidRequestError{msg: "search() needs a non-empty query. Pass the question as the query argument."}
	}
	mode := "pro"
	if opts != nil && opts.Mode != "" {
		mode = opts.Mode
	}
	payload := map[string]any{
		"query": query,
		"mode":  mode,
	}
	applyFilters(payload, opts)
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, &InvalidRequestError{msg: "could not encode the search request. Retry with a string query."}
	}
	data, err := c.do(ctx, http.MethodPost, "search", body, "")
	if err != nil {
		return nil, err
	}
	return searchResponseFromJSON(data, query), nil
}

// Fetch returns one URL's main content as markdown.
//
// Use it after Search when a snippet is not enough. Internal hosts and private
// addresses are refused here, before a request is sent.
func (c *Client) Fetch(ctx context.Context, pageURL string) (*Page, error) {
	if err := requireClient(c); err != nil {
		return nil, err
	}
	if err := requireContext(ctx); err != nil {
		return nil, err
	}
	if err := rejectPrivateFetchTarget(pageURL); err != nil {
		return nil, err
	}
	data, err := c.do(ctx, http.MethodGet, "fetch", nil, pageURL)
	if err != nil {
		return nil, err
	}
	return pageFromJSON(data, pageURL), nil
}

func requireClient(c *Client) error {
	if c == nil || c.http == nil {
		return &InvalidRequestError{msg: "nil client: call keenable.New and use the client it returns"}
	}
	return nil
}

func requireContext(ctx context.Context) error {
	if ctx == nil {
		return &InvalidRequestError{msg: "nil context: pass context.Background() or a request context"}
	}
	return nil
}

func (c *Client) do(ctx context.Context, method, endpoint string, body []byte, fetchURL string) (map[string]any, error) {
	rawURL, err := c.endpointURL(endpoint, fetchURL)
	if err != nil {
		return nil, err
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, reader)
	if err != nil {
		return nil, &InvalidRequestError{msg: "could not build the request: " + err.Error() + ". Check Options.BaseURL or KEENABLE_API_URL."}
	}
	req.Header = c.headers(body != nil)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, c.transportError(ctx, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, c.transportError(ctx, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, toAPIError(resp.StatusCode, raw, resp.Header)
	}
	return decodeObject(resp.StatusCode, raw, resp.Header)
}

func (c *Client) transportError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
		if c.timeout > 0 {
			return &ConnectionError{msg: fmt.Sprintf(
				"the Keenable API did not respond within %s. Raise Options.Timeout or pass a context with a longer deadline.",
				c.timeout,
			)}
		}
		return &ConnectionError{msg: "the Keenable API did not respond before the request deadline. Set Timeout on the http.Client you passed to New, or drop that client and set Options.Timeout."}
	}
	return &ConnectionError{msg: fmt.Sprintf("could not reach the Keenable API: %v. Check KEENABLE_API_URL and retry.", err)}
}

func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func (c *Client) headers(jsonBody bool) http.Header {
	h := make(http.Header)
	h.Set("Accept", "application/json")
	h.Set("User-Agent", "keenable-go/"+Version)
	// The public tier rejects requests without this header.
	h.Set("X-Keenable-Title", c.clientSource)
	if c.apiKey != "" {
		h.Set("X-API-Key", c.apiKey)
	}
	if jsonBody {
		h.Set("Content-Type", "application/json")
	}
	return h
}

func (c *Client) endpointURL(endpoint, fetchURL string) (string, error) {
	suffix := ""
	if c.apiKey == "" {
		suffix = "/public"
	}
	u, err := url.Parse(c.baseURL + "/v1/" + endpoint + suffix)
	if err != nil {
		return "", &InvalidRequestError{msg: fmt.Sprintf("baseURL %q is not a valid URL", c.baseURL)}
	}
	if fetchURL != "" {
		q := u.Query()
		q.Set("url", fetchURL)
		u.RawQuery = q.Encode()
	}
	return u.String(), nil
}

func resolveAPIKey(explicit *string) string {
	var key string
	if explicit != nil {
		key = *explicit
	} else {
		key = os.Getenv("KEENABLE_API_KEY")
	}
	return strings.TrimSpace(key)
}

func resolveBaseURL(explicit *string) (string, error) {
	var raw string
	if explicit != nil {
		raw = *explicit
	} else if v, ok := os.LookupEnv("KEENABLE_API_URL"); ok && strings.TrimSpace(v) != "" {
		raw = v
	} else {
		raw = DefaultBaseURL
	}
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	if raw == "" {
		return "", &InvalidRequestError{msg: `baseURL must be an https:// URL with a host, got ""`}
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return "", &InvalidRequestError{msg: fmt.Sprintf("baseURL must be an https:// URL with a host, got %q", raw)}
	}
	scheme := strings.ToLower(u.Scheme)
	host := normalizeHost(u.Hostname())
	if scheme == "https" || (scheme == "http" && isLocalHost(host)) {
		return raw, nil
	}
	return "", &InvalidRequestError{msg: fmt.Sprintf(
		"baseURL must use https (http is only allowed for localhost, 127.0.0.1, and ::1), got %q",
		raw,
	)}
}

func isLocalHost(host string) bool {
	switch host {
	case "localhost", "127.0.0.1", "::1":
		return true
	default:
		return false
	}
}

var blockedHosts = map[string]struct{}{
	"localhost":                {},
	"metadata.google.internal": {},
}

// rejectPrivateFetchTarget refuses obviously internal fetch targets before a
// request leaves the process. The API enforces this too. Stopping here keeps
// an internal hostname out of an outbound request.
func rejectPrivateFetchTarget(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return &InvalidRequestError{msg: fmt.Sprintf("fetch() needs an http(s) URL, got %q. Use an https URL on the public internet.", raw)}
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return &InvalidRequestError{msg: fmt.Sprintf("fetch() needs an http(s) URL, got %q. Use an https URL on the public internet.", raw)}
	}
	// A trailing dot resolves to the same host but does not match it as a
	// string, which is enough to walk past a blocklist.
	host := normalizeHost(u.Hostname())
	if host == "" {
		return &InvalidRequestError{msg: fmt.Sprintf("fetch() URL has no host: %q. Pass an https URL with a public host.", raw)}
	}
	if _, blocked := blockedHosts[host]; blocked {
		return &InvalidRequestError{msg: fmt.Sprintf("refusing to fetch internal host %q. Pass a public http(s) URL.", host)}
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return nil // A hostname. The API's SSRF guard is the backstop.
	}
	// ::ffff:127.0.0.1 and ::ffff:7f00:1 reach the same machine as 127.0.0.1.
	if isNonPublic(addr.Unmap()) {
		return &InvalidRequestError{msg: fmt.Sprintf("refusing to fetch private address %q. Pass a public http(s) URL.", host)}
	}
	return nil
}

func normalizeHost(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	host = strings.TrimPrefix(host, "[")
	host = strings.TrimSuffix(host, "]")
	return strings.TrimRight(host, ".")
}

func isNonPublic(addr netip.Addr) bool {
	return addr.IsLoopback() ||
		addr.IsPrivate() ||
		addr.IsLinkLocalUnicast() ||
		addr.IsLinkLocalMulticast() ||
		addr.IsMulticast() ||
		addr.IsUnspecified() ||
		!addr.IsGlobalUnicast()
}

var statusLabels = map[int]string{
	401: "Keenable authentication failed (401)",
	403: "Keenable authentication failed (403)",
	402: "Keenable: insufficient credits (402)",
	429: "Keenable rate limit exceeded (429)",
}

func toAPIError(status int, raw []byte, header http.Header) error {
	detail := ""
	var body any
	if err := json.Unmarshal(raw, &body); err != nil {
		detail = strings.TrimSpace(string(raw))
	} else if m, ok := body.(map[string]any); ok {
		detail = firstDetail(m, "message", "error", "detail")
	}
	label, ok := statusLabels[status]
	if !ok {
		label = fmt.Sprintf("Keenable API error (%d)", status)
	}
	msg := label
	if detail != "" {
		msg = label + ": " + detail
	}
	switch status {
	case 401, 403:
		msg += ". Check KEENABLE_API_KEY, or pass Options.APIKey."
	case 402:
		msg += ". Add credits at https://keenable.ai/console."
	case 429:
		msg += ". Set KEENABLE_API_KEY to lift the keyless hourly cap. Create a key at https://keenable.ai/console."
		if reset := headerValue(header, "X-RateLimit-Reset"); reset != "" {
			msg += " Retry after " + reset + "."
		}
	default:
		msg += ". Retry the request. If it keeps failing, confirm KEENABLE_API_URL."
	}
	return classifyStatus(status, msg, detail, header)
}

func classifyStatus(status int, msg, body string, header http.Header) error {
	code := CodeAPI
	switch status {
	case 401, 403:
		code = CodeAuth
	case 429:
		code = CodeRateLimit
	}
	api := &APIError{
		StatusCode: status,
		Body:       body,
		RequestID:  headerValue(header, "X-Request-Id"),
		msg:        msg,
		code:       code,
	}
	switch status {
	case 401, 403:
		return &AuthError{APIError: api}
	case 429:
		return &RateLimitError{
			APIError:  api,
			Limit:     headerInt(header, "X-RateLimit-Limit"),
			Remaining: headerInt(header, "X-RateLimit-Remaining"),
			Reset:     headerValue(header, "X-RateLimit-Reset"),
		}
	default:
		return api
	}
}

func headerValue(h http.Header, key string) string {
	if h == nil {
		return ""
	}
	return strings.TrimSpace(h.Get(key))
}

func headerInt(h http.Header, key string) int {
	n, err := strconv.Atoi(headerValue(h, key))
	if err != nil {
		return 0
	}
	return n
}

func firstDetail(m map[string]any, keys ...string) string {
	for _, key := range keys {
		v, ok := m[key]
		if !ok || v == nil {
			continue
		}
		var s string
		switch t := v.(type) {
		case string:
			s = t
		default:
			s = fmt.Sprint(t)
		}
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

func decodeObject(status int, raw []byte, header http.Header) (map[string]any, error) {
	snippet := raw
	if len(snippet) > 200 {
		snippet = snippet[:200]
	}
	id := headerValue(header, "X-Request-Id")
	var data any
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, &APIError{
			StatusCode: status,
			RequestID:  id,
			code:       CodeAPI,
			msg:        fmt.Sprintf("Keenable API returned a non-JSON response: %q. Retry the request. If it keeps failing, confirm KEENABLE_API_URL.", string(snippet)),
		}
	}
	obj, ok := data.(map[string]any)
	if !ok {
		return nil, &APIError{
			StatusCode: status,
			RequestID:  id,
			code:       CodeAPI,
			msg:        fmt.Sprintf("Unexpected response from the Keenable API: %q. Retry the request. If it keeps failing, confirm KEENABLE_API_URL.", string(snippet)),
		}
	}
	return obj, nil
}
