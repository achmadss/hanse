package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"github.com/achmadss/hanse/internal/auth"
	"github.com/achmadss/hanse/internal/registry"
)

// reloadFixture loads one capability and returns the registry and the
// directory it scans.
func reloadFixture(t *testing.T) (*registry.Registry, string) {
	t.Helper()
	capsDir := filepath.Join(t.TempDir(), "capabilities")
	writeCapability(t, capsDir, "pc", pcManifest, "handler.sh")
	reg, _, err := registry.Load(registry.Options{Dirs: []string{capsDir}, Input: fixtureInput()})
	if err != nil {
		t.Fatalf("load fixture registry: %v", err)
	}
	return reg, capsDir
}

// authHandler wraps handler with a real token store and returns one full
// access token and one restricted token.
func authHandler(t *testing.T, handler http.Handler) (http.Handler, string, string) {
	t.Helper()
	store, err := auth.LoadStore(filepath.Join(t.TempDir(), "tokens.json"))
	if err != nil {
		t.Fatalf("load token store: %v", err)
	}
	full, err := store.Add("full", nil)
	if err != nil {
		t.Fatalf("add full token: %v", err)
	}
	limited, err := store.Add("limited", []string{"pc.up"})
	if err != nil {
		t.Fatalf("add restricted token: %v", err)
	}
	return store.Middleware(handler), full, limited
}

// postToken performs one POST with a bearer token.
func postToken(t *testing.T, handler http.Handler, path, token string) (*http.Response, []byte) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	handler.ServeHTTP(rec, req)
	return rec.Result(), rec.Body.Bytes()
}

// getToken performs one GET with a bearer token.
func getToken(t *testing.T, handler http.Handler, path, token string) (*http.Response, []byte) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	handler.ServeHTTP(rec, req)
	return rec.Result(), rec.Body.Bytes()
}

// TestReloadEndpoint checks that a full-access token reloads the capability
// list and a restricted token is refused.
func TestReloadEndpoint(t *testing.T) {
	reg, capsDir := reloadFixture(t)
	handler, full, limited := authHandler(t, New(reg, nil, nil))

	resp, body := postToken(t, handler, "/v1/capabilities/reload", limited)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("restricted reload status = %d, want 403: %s", resp.StatusCode, body)
	}
	var wire struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if wire.Code != "forbidden" {
		t.Errorf("restricted reload code = %q, want forbidden", wire.Code)
	}

	writeCapability(t, capsDir, "demo", demoManifest, "handler.sh")
	resp, body = postToken(t, handler, "/v1/capabilities/reload", full)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("full reload status = %d, want 200: %s", resp.StatusCode, body)
	}
	var out reloadResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode reload body: %v", err)
	}
	if len(out.Capabilities) != 2 {
		t.Fatalf("reload capabilities = %+v, want pc and demo", out.Capabilities)
	}
	if out.Capabilities[0].ID != "demo" || out.Capabilities[1].ID != "pc" {
		t.Errorf("reload capabilities = %s, %s, want demo then pc", out.Capabilities[0].ID, out.Capabilities[1].ID)
	}
}

// TestReloadConcurrentWithReads reloads while discovery requests read the
// registry. It is meaningful under -race.
func TestReloadConcurrentWithReads(t *testing.T) {
	reg, capsDir := reloadFixture(t)
	handler, full, _ := authHandler(t, New(reg, nil, nil))
	writeCapability(t, capsDir, "demo", demoManifest, "handler.sh")

	var wg sync.WaitGroup
	errs := make(chan string, 64)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				resp, body := getToken(t, handler, "/v1/capabilities", full)
				if resp.StatusCode != http.StatusOK {
					errs <- "GET /v1/capabilities answered " + resp.Status
					return
				}
				var out capabilitiesResponse
				if err := json.Unmarshal(body, &out); err != nil {
					errs <- "GET /v1/capabilities returned invalid JSON"
					return
				}
			}
		}()
	}
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				resp, body := postToken(t, handler, "/v1/capabilities/reload", full)
				if resp.StatusCode != http.StatusOK {
					errs <- "POST /v1/capabilities/reload answered " + resp.Status + ": " + string(body)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for msg := range errs {
		t.Error(msg)
	}
}
