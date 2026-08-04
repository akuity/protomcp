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
// tagged with a Sec-Fetch-Site header, and returns the status code.
func postInitialize(t *testing.T, url, secFetchSite string) int {
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

	if got := postInitialize(t, ts.URL, "cross-site"); got != http.StatusForbidden {
		t.Errorf("cross-site POST status = %d, want %d (cross-origin protection missing)", got, http.StatusForbidden)
	}
	if got := postInitialize(t, ts.URL, ""); got != http.StatusOK {
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

	if got := postInitialize(t, ts.URL, "cross-site"); got != http.StatusForbidden {
		t.Errorf("cross-site POST status = %d, want %d (wrap dropped by unrelated HTTP options)", got, http.StatusForbidden)
	}
}

// TestCrossOriginProtection_CallerSuppliedWins verifies the opt-out: a
// caller-supplied CrossOriginProtection passes through to the SDK
// untouched, so a permissive policy admits cross-site requests.
func TestCrossOriginProtection_CallerSuppliedWins(t *testing.T) {
	permissive := http.NewCrossOriginProtection()
	permissive.AddInsecureBypassPattern("/")
	s := New("t", "0.0.1",
		WithHTTPOptions(&mcp.StreamableHTTPOptions{
			CrossOriginProtection: permissive,
		}),
	)
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)

	if got := postInitialize(t, ts.URL, "cross-site"); got != http.StatusOK {
		t.Errorf("cross-site POST status = %d, want %d (caller-supplied protection not honored)", got, http.StatusOK)
	}
}
