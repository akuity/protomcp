package protomcp

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// postInitialize fires a raw initialize POST at the server, optionally
// tagged with Sec-Fetch-Site and Origin headers, and returns the status
// code.
func postInitialize(t *testing.T, url, secFetchSite, origin string) int {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"c","version":"0.0.1"}}}`
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if secFetchSite != "" {
		req.Header.Set("Sec-Fetch-Site", secFetchSite)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp.StatusCode
}

// TestCrossOriginProtection_OnByDefault locks in the v1.5.0 behavior we
// restore on top of go-sdk v1.7.0: with no HTTP options at all, a
// cross-site POST is rejected while a same-origin one is served. The SDK
// stopped installing this protection itself in v1.7.0.
func TestCrossOriginProtection_OnByDefault(t *testing.T) {
	s := New("t", "0.0.1")
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)

	if got := postInitialize(t, ts.URL, "cross-site", ""); got != http.StatusForbidden {
		t.Errorf("cross-site POST status = %d, want %d (cross-origin protection missing)", got, http.StatusForbidden)
	}
	if got := postInitialize(t, ts.URL, "", ""); got != http.StatusOK {
		t.Errorf("same-origin POST status = %d, want %d", got, http.StatusOK)
	}
}

// TestCrossOriginProtection_UserHTTPOptsStillWrapped verifies that
// supplying unrelated StreamableHTTPOptions does not lose the default
// protection: only a caller-supplied CrossOriginProtection opts out.
func TestCrossOriginProtection_UserHTTPOptsStillWrapped(t *testing.T) {
	s := New("t", "0.0.1",
		WithHTTPOptions(&mcp.StreamableHTTPOptions{JSONResponse: true}),
	)
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)

	if got := postInitialize(t, ts.URL, "cross-site", ""); got != http.StatusForbidden {
		t.Errorf("cross-site POST status = %d, want %d (wrap dropped by unrelated HTTP options)", got, http.StatusForbidden)
	}
}

// TestCrossOriginProtection_CallerSuppliedWins verifies the opt-out in
// both directions: a caller-supplied CrossOriginProtection replaces
// protomcp's default wrap, so its trusted origin is admitted where the
// default would 403 — and its policy still rejects untrusted origins.
// The rejection leg matters: it fails if a future SDK release keeps the
// deprecated field but stops honoring it, which would otherwise leave
// the server with no protection at all while this test stayed green.
func TestCrossOriginProtection_CallerSuppliedWins(t *testing.T) {
	cop := http.NewCrossOriginProtection()
	if err := cop.AddTrustedOrigin("https://trusted.example"); err != nil {
		t.Fatalf("AddTrustedOrigin: %v", err)
	}
	s := New("t", "0.0.1",
		WithHTTPOptions(&mcp.StreamableHTTPOptions{
			CrossOriginProtection: cop,
		}),
	)
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)

	if got := postInitialize(t, ts.URL, "cross-site", "https://trusted.example"); got != http.StatusOK {
		t.Errorf("trusted-origin cross-site POST status = %d, want %d (caller-supplied protection not honored)", got, http.StatusOK)
	}
	if got := postInitialize(t, ts.URL, "cross-site", "https://evil.example"); got != http.StatusForbidden {
		t.Errorf("untrusted-origin cross-site POST status = %d, want %d (caller-supplied policy not enforced)", got, http.StatusForbidden)
	}
}
