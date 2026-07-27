package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DaemonClient talks to silod's operator socket.
//
// The console never opens bbolt directly: two processes contending for the same
// file lock would hang both, and only the daemon knows which cache directory is
// actually live. Everything cache-related therefore goes over this socket.
type DaemonClient struct {
	base   string
	client *http.Client
}

// NewDaemonClient dials silod's admin listener. addr is either a Unix socket
// path or a host:port. A socket path is the default and the recommended
// deployment — the filesystem permissions are the authorization.
func NewDaemonClient(addr string) *DaemonClient {
	if strings.HasPrefix(addr, "http://") || strings.HasPrefix(addr, "https://") {
		return &DaemonClient{
			base:   strings.TrimSuffix(addr, "/"),
			client: &http.Client{Timeout: 30 * time.Second},
		}
	}
	if isHostPort(addr) {
		return &DaemonClient{
			base:   "http://" + addr,
			client: &http.Client{Timeout: 30 * time.Second},
		}
	}
	// Unix socket: the host in the URL is ignored by the custom dialer, but has
	// to be present and syntactically valid for the request to be built.
	return &DaemonClient{
		base: "http://silod",
		client: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "unix", addr)
				},
			},
		},
	}
}

// isHostPort reports whether addr looks like a TCP address rather than a
// filesystem path.
func isHostPort(addr string) bool {
	if strings.Contains(addr, "/") {
		return false
	}
	_, _, err := net.SplitHostPort(addr)
	return err == nil
}

func (c *DaemonClient) CacheStats(ctx context.Context) ([]ProjectCacheStat, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/v1/admin/cache-stats", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("daemon: cache stats: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("daemon: cache stats: status %d", resp.StatusCode)
	}
	var body struct {
		Projects []ProjectCacheStat `json:"projects"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("daemon: cache stats: decode: %w", err)
	}
	return body.Projects, nil
}

func (c *DaemonClient) PurgeCache(ctx context.Context, projectID string) (PurgeOutcome, error) {
	resp, err := c.post(ctx, "/v1/admin/purge-cache", projectID)
	if err != nil {
		return PurgeOutcome{}, err
	}
	defer resp.Body.Close()

	var body struct {
		Purged  bool   `json:"purged"`
		Pending int    `json:"pending"`
		Error   string `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)

	// 409 is a refusal, not a failure: the daemon declining to delete unsynced
	// writes is the gate working. Surface it as an outcome the operator can act
	// on rather than an error that looks like a broken console.
	if resp.StatusCode == http.StatusConflict {
		return PurgeOutcome{Pending: body.Pending, Reason: body.Error}, nil
	}
	if resp.StatusCode != http.StatusOK {
		return PurgeOutcome{}, fmt.Errorf("daemon: purge %q: status %d: %s",
			projectID, resp.StatusCode, body.Error)
	}
	return PurgeOutcome{Purged: body.Purged}, nil
}

func (c *DaemonClient) CompactCache(ctx context.Context, projectID string) (CompactOutcome, error) {
	resp, err := c.post(ctx, "/v1/admin/compact-cache", projectID)
	if err != nil {
		return CompactOutcome{}, err
	}
	defer resp.Body.Close()

	var body struct {
		Compacted   bool   `json:"compacted"`
		Reclaimed   int64  `json:"reclaimed_bytes"`
		BytesBefore int64  `json:"bytes_before"`
		BytesAfter  int64  `json:"bytes_after"`
		SkipReason  string `json:"skip_reason"`
		Error       string `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)

	if resp.StatusCode != http.StatusOK {
		return CompactOutcome{}, fmt.Errorf("daemon: compact %q: status %d: %s",
			projectID, resp.StatusCode, body.Error)
	}
	return CompactOutcome{
		Compacted:   body.Compacted,
		Reclaimed:   body.Reclaimed,
		BytesBefore: body.BytesBefore,
		BytesAfter:  body.BytesAfter,
		SkipReason:  body.SkipReason,
	}, nil
}

func (c *DaemonClient) post(ctx context.Context, path, projectID string) (*http.Response, error) {
	u := c.base + path + "?project=" + url.QueryEscape(projectID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("daemon: %s %q: %w", strings.TrimPrefix(path, "/v1/admin/"), projectID, err)
	}
	return resp, nil
}

func (c *DaemonClient) CacheEntries(ctx context.Context, projectID string) ([]CacheEntry, error) {
	u := c.base + "/v1/admin/cache-entries?project=" + url.QueryEscape(projectID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("daemon: cache entries %q: %w", projectID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("daemon: cache entries %q: status %d", projectID, resp.StatusCode)
	}
	var body struct {
		Entries []CacheEntry `json:"entries"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("daemon: cache entries %q: decode: %w", projectID, err)
	}
	return body.Entries, nil
}
