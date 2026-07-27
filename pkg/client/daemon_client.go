package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Config initializes a Client. The token is the project-scoped credential
// issued at onboarding; the daemon resolves it to exactly one project, so the
// SDK can never address another project's memory.
type Config struct {
	// Endpoint is the daemon address: a Unix socket path (e.g.
	// /var/run/silo.sock) or an http(s) URL for a networked daemon.
	Endpoint string
	// Token is the project-scoped bearer credential.
	Token string
	// Actor names who is writing — an agent name, a job, a person. Recorded on
	// every write so an operator can see which agent produced which memory.
	// Optional: the daemon supplies its own default when this is empty.
	Actor string
	// HTTPClient overrides the default (mainly for tests).
	HTTPClient *http.Client
	// Timeout for each request when HTTPClient isn't supplied.
	Timeout time.Duration
}

// New returns a daemon-backed Client.
func New(cfg Config) (Client, error) {
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("client: endpoint required")
	}
	if cfg.Token == "" {
		return nil, fmt.Errorf("client: token required")
	}

	httpClient := cfg.HTTPClient
	baseURL := strings.TrimSuffix(cfg.Endpoint, "/")

	if httpClient == nil {
		timeout := cfg.Timeout
		if timeout == 0 {
			timeout = 30 * time.Second
		}
		// A non-URL endpoint is a Unix socket path: dial it directly and use a
		// placeholder host so net/http can still build a valid request URL.
		if !strings.HasPrefix(baseURL, "http://") && !strings.HasPrefix(baseURL, "https://") {
			socket := baseURL
			httpClient = &http.Client{
				Timeout: timeout,
				Transport: &http.Transport{
					DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
						return (&net.Dialer{}).DialContext(ctx, "unix", socket)
					},
				},
			}
			baseURL = "http://silo-daemon"
		} else {
			httpClient = &http.Client{Timeout: timeout}
		}
	}

	return &daemonClient{baseURL: baseURL, token: cfg.Token, actor: cfg.Actor, http: httpClient}, nil
}

type daemonClient struct {
	baseURL string
	token   string
	actor   string
	http    *http.Client
}

var _ Client = (*daemonClient)(nil)

// do issues a request with the project-scoped bearer token and decodes a JSON
// response into out (when non-nil).
func (c *daemonClient) do(ctx context.Context, method, path string, query url.Values, body, out interface{}) error {
	u := c.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}

	var reader *bytes.Reader
	if body != nil {
		blob, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("client: encode request: %w", err)
		}
		reader = bytes.NewReader(blob)
	} else {
		reader = bytes.NewReader(nil)
	}

	req, err := http.NewRequestWithContext(ctx, method, u, reader)
	if err != nil {
		return fmt.Errorf("client: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("client: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	// Any 2xx is a success. The daemon returns 202 for a write it buffered
	// locally because the backend was unreachable — that write was accepted and
	// will be replayed, so treating it as an error would turn a degraded-but-
	// working path into a hard failure, and callers would retry a write that is
	// already queued.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return decodeError(resp)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("client: decode response: %w", err)
		}
	}
	return nil
}

// decodeError turns a non-200 into a typed error where we have one.
func decodeError(resp *http.Response) error {
	var payload struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&payload)
	msg := payload.Error
	if msg == "" {
		msg = resp.Status
	}
	switch resp.StatusCode {
	case http.StatusNotFound:
		return ErrNotFound
	case http.StatusUnauthorized:
		return fmt.Errorf("%w: %s", ErrUnauthorized, msg)
	case http.StatusForbidden:
		// The token authenticated; the operation is not permitted. Mapping this
		// to ErrUnauthorized would tell a caller to re-authenticate, which can
		// never resolve it.
		return fmt.Errorf("%w: %s", ErrReadOnly, msg)
	default:
		return fmt.Errorf("client: daemon error (%d): %s", resp.StatusCode, msg)
	}
}

func (c *daemonClient) Read(ctx context.Context, path string) ([]byte, error) {
	var out struct {
		Content []byte `json:"content"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/read", url.Values{"path": {path}}, nil, &out); err != nil {
		return nil, err
	}
	return out.Content, nil
}

func (c *daemonClient) Write(ctx context.Context, path string, content []byte) error {
	return c.WriteAs(ctx, path, content, "")
}

func (c *daemonClient) WriteAs(ctx context.Context, path string, content []byte, actor string) error {
	body := map[string]interface{}{"path": path, "content": content}
	// Attribution is what lets an operator see which agent wrote what. The
	// per-call actor wins over the client-wide one: one MCP server serves a
	// whole repo, so the caller is the only thing that knows which agent it is.
	// Omitted when both are unset, so the daemon keeps its own default rather
	// than recording an empty actor.
	if actor == "" {
		actor = c.actor
	}
	if actor != "" {
		body["actor"] = actor
	}
	return c.do(ctx, http.MethodPost, "/v1/write", nil, body, nil)
}

func (c *daemonClient) List(ctx context.Context, pathPrefix string) ([]string, error) {
	var out struct {
		Paths []string `json:"paths"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/list", url.Values{"prefix": {pathPrefix}}, nil, &out); err != nil {
		return nil, err
	}
	return out.Paths, nil
}

func (c *daemonClient) Search(ctx context.Context, pathPrefix, query string) ([]SearchResult, error) {
	var out struct {
		Results []SearchResult `json:"results"`
	}
	q := url.Values{"prefix": {pathPrefix}, "q": {query}}
	if err := c.do(ctx, http.MethodGet, "/v1/search", q, nil, &out); err != nil {
		return nil, err
	}
	return out.Results, nil
}
