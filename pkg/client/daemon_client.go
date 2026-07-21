package client

import "context"

// Config initializes a Client. The credential is the project-scoped one issued
// at onboarding; the SDK never sees another project's credential or key.
type Config struct {
	ProjectID string
	Endpoint  string // unix socket path or network address of the daemon
	// credential wiring lands here (loaded from env / secrets, never hardcoded).
}

// New returns a daemon-backed Client. Not yet implemented — build sequence
// step 4 (docs/architecture.md).
func New(cfg Config) (Client, error) {
	return &daemonClient{cfg: cfg}, nil
}

type daemonClient struct {
	cfg Config
}

var _ Client = (*daemonClient)(nil)

func (c *daemonClient) Read(ctx context.Context, path string) ([]byte, error) {
	return nil, errNotImplemented
}

func (c *daemonClient) Write(ctx context.Context, path string, content []byte) error {
	return errNotImplemented
}

func (c *daemonClient) List(ctx context.Context, pathPrefix string) ([]string, error) {
	return nil, errNotImplemented
}

func (c *daemonClient) Search(ctx context.Context, pathPrefix, query string) ([]SearchResult, error) {
	return nil, errNotImplemented
}
