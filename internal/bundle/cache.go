package bundle

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	herrors "github.com/achmadss/hanse/internal/errors"
)

// hashPattern is the bundle hash form of agent-protocol.md section 4.3.
var hashPattern = regexp.MustCompile(`^sha256:([0-9a-f]{64})$`)

// Cache is the agent's content-addressed handler cache: one directory per
// bundle hash, `<cache_dir>/<hex hash>` (docs/agent-protocol.md section
// 5.2). It downloads a missing or damaged bundle once, even when several
// runs need it at the same time, and verifies the hash before every use.
type Cache struct {
	// Dir is the cache directory. Required.
	Dir string
	// GatewayURL is the Gateway's base URL, without a trailing slash.
	GatewayURL string
	// Credential is the node credential used to download a bundle. It is
	// never logged.
	Credential string
	// Client is the HTTP client for downloads. Nil means a client with a
	// 30 second timeout.
	Client *http.Client
	// MaxUnpackedBytes bounds one unpacked bundle. Zero means
	// DefaultMaxUnpackedBytes.
	MaxUnpackedBytes int64

	mu       sync.Mutex
	inflight map[string]*download
}

// download is one in-flight download of a bundle hash.
type download struct {
	done chan struct{}
	dir  string
	err  error
}

// Ensure returns the cache directory of a bundle hash. It verifies the
// cached copy and downloads the bundle when the copy is missing or its
// hash changed (docs/agent-protocol.md sections 5.2 and 5.3).
func (c *Cache) Ensure(ctx context.Context, hash string) (string, error) {
	hex, err := cachePath(hash)
	if err != nil {
		return "", err
	}
	target := filepath.Join(c.Dir, hex)
	if err := c.verify(target, hash); err == nil {
		return target, nil
	}

	c.mu.Lock()
	if c.inflight == nil {
		c.inflight = map[string]*download{}
	}
	if active, ok := c.inflight[hash]; ok {
		c.mu.Unlock()
		select {
		case <-active.done:
			return active.dir, active.err
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	// Another download may have finished between the check above and the
	// lock. Re-check before replacing the copy.
	if err := c.verify(target, hash); err == nil {
		c.mu.Unlock()
		return target, nil
	}
	// A damaged copy is useless, so remove it before the download. A
	// missing copy needs no removal.
	_ = os.RemoveAll(target)
	active := &download{done: make(chan struct{})}
	c.inflight[hash] = active
	c.mu.Unlock()

	active.dir, active.err = c.download(ctx, hash, hex)
	close(active.done)

	c.mu.Lock()
	delete(c.inflight, hash)
	c.mu.Unlock()
	return active.dir, active.err
}

// verify reports whether the cached directory exists and matches hash.
func (c *Cache) verify(target, hash string) error {
	if _, err := os.Stat(target); err != nil {
		return err
	}
	got, err := HashDir(target)
	if err != nil {
		return err
	}
	if got != hash {
		return herrors.New(herrors.HandlerIntegrity,
			fmt.Sprintf("cached handler files hash to %s, not %s", got, hash), nil)
	}
	return nil
}

// download fetches, unpacks, and verifies one bundle, then renames its
// directory into place. The rename is atomic, so a run never sees a
// half-unpacked bundle.
func (c *Cache) download(ctx context.Context, hash, hex string) (string, error) {
	if err := os.MkdirAll(c.Dir, 0o700); err != nil {
		return "", herrors.New(herrors.BundleUnavailable,
			fmt.Sprintf("cannot create the bundle cache: %s", err), nil)
	}
	endpoint, err := c.endpoint(hash)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", herrors.New(herrors.BundleUnavailable,
			"cannot build the bundle download request", nil)
	}
	req.Header.Set("Authorization", "Bearer "+c.Credential)
	resp, err := c.client().Do(req)
	if err != nil {
		return "", herrors.New(herrors.BundleUnavailable,
			fmt.Sprintf("cannot download bundle %s: %s", hash, err), nil)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", herrors.New(herrors.BundleUnavailable,
			fmt.Sprintf("the Gateway answered bundle %s with HTTP %d", hash, resp.StatusCode), nil)
	}
	tmp, err := os.MkdirTemp(c.Dir, ".bundle-*")
	if err != nil {
		return "", herrors.New(herrors.BundleUnavailable,
			fmt.Sprintf("cannot create a bundle directory: %s", err), nil)
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(tmp)
		}
	}()
	if err := Unpack(resp.Body, tmp, c.maxBytes()); err != nil {
		return "", herrors.New(herrors.BundleUnavailable,
			fmt.Sprintf("bundle %s cannot be unpacked: %s", hash, err), nil)
	}
	got, err := HashDir(tmp)
	if err != nil {
		return "", herrors.New(herrors.HandlerIntegrity,
			fmt.Sprintf("cannot verify bundle %s: %s", hash, err), nil)
	}
	if got != hash {
		return "", herrors.New(herrors.HandlerIntegrity,
			fmt.Sprintf("bundle %s unpacks to %s", hash, got), nil)
	}
	target := filepath.Join(c.Dir, hex)
	if err := os.Rename(tmp, target); err != nil {
		// Another process may have filled the cache between the check
		// and the rename. A matching copy is as good as ours.
		if verr := c.verify(target, hash); verr == nil {
			return target, nil
		}
		_ = os.RemoveAll(target)
		if err := os.Rename(tmp, target); err != nil {
			return "", herrors.New(herrors.BundleUnavailable,
				fmt.Sprintf("cannot store bundle %s: %s", hash, err), nil)
		}
	}
	cleanup = false
	return target, nil
}

// client returns the HTTP client for downloads. It refuses every redirect:
// the bundle request carries the node credential, and a same-host redirect
// would keep the Authorization header, including across an https to http
// downgrade. The Gateway never redirects this route, so a redirect is a
// failed download (docs/agent-protocol.md section 5.2).
func (c *Cache) client() *http.Client {
	refuseRedirects := func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	if c.Client != nil {
		copied := *c.Client
		copied.CheckRedirect = refuseRedirects
		return &copied
	}
	return &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: refuseRedirects,
	}
}

// maxBytes returns the unpacked size limit.
func (c *Cache) maxBytes() int64 {
	if c.MaxUnpackedBytes > 0 {
		return c.MaxUnpackedBytes
	}
	return DefaultMaxUnpackedBytes
}

// endpoint builds the bundle download URL.
func (c *Cache) endpoint(hash string) (string, error) {
	base, err := url.Parse(c.GatewayURL)
	if err != nil || base.Host == "" {
		return "", herrors.New(herrors.BundleUnavailable,
			"the Gateway URL is not a valid URL", nil)
	}
	base.Path = strings.TrimSuffix(base.Path, "/") + "/v1/agent/bundles/" + url.PathEscape(hash)
	return base.String(), nil
}

// cachePath checks a bundle hash and returns its cache directory name: the
// lowercase hex digest without the `sha256:` prefix.
func cachePath(hash string) (string, error) {
	match := hashPattern.FindStringSubmatch(hash)
	if match == nil {
		return "", herrors.New(herrors.HandlerIntegrity,
			fmt.Sprintf("bundle %q is not sha256:<hex>", hash), nil)
	}
	return match[1], nil
}
