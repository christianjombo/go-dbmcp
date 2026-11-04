package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/christianjombo/go-dbmcp/internal/config"
	"github.com/christianjombo/go-dbmcp/internal/guard"
)

func testDeps(t *testing.T) Deps {
	t.Helper()
	scanner, _ := guard.New(nil, true)
	return Deps{
		Cfg: &config.Config{
			Version:             "test",
			JSONMaxRows:         100,
			CSVMaxRows:          2000,
			DefaultDatabase:     "postgres",
			DefaultSSLMode:      "prefer",
			QueryTimeout:        5 * time.Second,
			ConnectTimeout:      2 * time.Second,
			PoolMaxConns:        2,
			PoolMaxConnIdleTime: time.Minute,
			PoolMaxConnLifetime: time.Minute,
			MaxResultBytes:      1 << 20,
		},
		// Discard logs so test output stays readable.
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Scanner: scanner,
	}
}

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(New(testDeps(t)).Handler)
	t.Cleanup(srv.Close)
	return srv
}

// Health must not depend on any database: this process has none of its own,
// and one tenant's unreachable server must never mark the proxy unhealthy.
func TestHealthzIsIndependentOfAnyDatabase(t *testing.T) {
	srv := newTestServer(t)

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["status"] != "ok" {
		t.Errorf("status field = %v", body["status"])
	}
}

// Connecting with no credentials at all must SUCCEED.
//
// Claude's connector setup probes a new server before it has been
// configured and reports any 4xx as "server unreachable". A credential check
// at connect time therefore looked like a broken server. Credentials are
// required at tool-call time instead, where the message can be read.
func TestConnectSucceedsWithoutCredentials(t *testing.T) {
	srv := newTestServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+ConnectPath, nil)
	req.Header.Set("Accept", "text/event-stream")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 — a probe with no credentials must not look like a broken server",
			resp.StatusCode)
	}
	if event, _ := readFirstEvent(t, resp.Body); event != "endpoint" {
		t.Errorf("first event = %q, want endpoint", event)
	}
}

// Likewise for a malformed value: the stream opens and the problem surfaces
// on use, not during discovery.
func TestConnectSucceedsWithInvalidPort(t *testing.T) {
	srv := newTestServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+ConnectPath, nil)
	req.Header.Set("x-db-username", "u")
	req.Header.Set("x-db-password", "p")
	req.Header.Set("x-db-host", "db.example.com")
	req.Header.Set("x-db-port", "not-a-port")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

// OAuth discovery must answer definitively so a client can conclude that no
// sign-in is required, rather than reporting "couldn't determine how this
// server signs in".
func TestOAuthDiscoveryReportsNoAuthRequired(t *testing.T) {
	srv := newTestServer(t)

	for _, path := range []string{
		"/.well-known/oauth-protected-resource",
		"/.well-known/oauth-authorization-server",
		"/.well-known/openid-configuration",
	} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", path, resp.StatusCode)
		}
		if !strings.Contains(string(body), "no_authentication_required") {
			t.Errorf("%s: body should state that no auth is required, got: %s", path, body)
		}
	}
}

// The endpoint must also speak the current Streamable HTTP transport, which
// is what Claude's connector uses.
func TestStreamableHTTPInitializeSucceeds(t *testing.T) {
	srv := newTestServer(t)

	for _, path := range []string{ConnectPath, StreamablePath, ShortPath} {
		body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{` +
			`"protocolVersion":"2025-06-18","capabilities":{},` +
			`"clientInfo":{"name":"probe","version":"1"}}}`

		req, _ := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		got, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s: status = %d, want 200: %s", path, resp.StatusCode, got)
			continue
		}
		if !strings.Contains(string(got), "serverInfo") {
			t.Errorf("%s: initialize did not return serverInfo: %s", path, got)
		}
	}
}

// The full SSE handshake, end to end. This works without a database because
// the pool is created lazily on the first query, not when the stream opens —
// which is also what keeps idle streams from consuming tenant connections.
func TestSSEHandshakeEmitsEndpointEvent(t *testing.T) {
	srv := newTestServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+ConnectPath, nil)
	req.Header.Set("x-db-username", "u")
	req.Header.Set("x-db-password", "p")
	req.Header.Set("x-db-host", "db.example.com")
	req.Header.Set("x-db-port", "5432")
	req.Header.Set("Accept", "text/event-stream")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}

	// Per the 2024-11-05 transport, the first event must be `endpoint` and
	// must carry the URL the client POSTs its messages to. Reaching this
	// assertion also proves the access-log wrapper forwards Flush: without
	// that, the event would sit in a buffer and this would time out.
	event, data := readFirstEvent(t, resp.Body)

	if event != "endpoint" {
		t.Fatalf("first event = %q, want %q", event, "endpoint")
	}
	if !strings.Contains(data, "sessionid=") {
		t.Fatalf("endpoint %q does not carry a sessionid", data)
	}
}

// Closing the stream must release the session. The server counts active
// streams, so the count returning to zero is the observable proof that the
// per-stream teardown ran.
func TestStreamCloseReleasesSession(t *testing.T) {
	s := New(testDeps(t))
	srv := httptest.NewServer(s.Handler)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+ConnectPath, nil)
	req.Header.Set("x-db-username", "u")
	req.Header.Set("x-db-password", "p")
	req.Header.Set("x-db-host", "db.example.com")
	req.Header.Set("x-db-port", "5432")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	readFirstEvent(t, resp.Body) // ensure the stream is fully established

	if got := s.ActiveStreams(); got != 1 {
		t.Fatalf("active streams = %d, want 1", got)
	}

	cancel() // the AI client goes away
	resp.Body.Close()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if s.ActiveStreams() == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("active streams = %d after disconnect, want 0", s.ActiveStreams())
}

// A POST carrying the legacy sessionid parameter still routes to the SSE
// transport, so existing clients are unaffected by the dual dispatch.
func TestLegacySessionIDRoutesToSSE(t *testing.T) {
	srv := newTestServer(t)

	resp, err := http.Post(srv.URL+ConnectPath+"?sessionid=does-not-exist",
		"application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	// The SSE handler owns this request and reports an unknown session;
	// the Streamable HTTP handler would instead complain about Accept.
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (session not found), got body: %s", resp.StatusCode, body)
	}
}

// readFirstEvent reads one SSE event ("event:" then "data:" then a blank line).
func readFirstEvent(t *testing.T, r io.Reader) (event, data string) {
	t.Helper()

	br := bufio.NewReader(r)
	type result struct{ event, data string }
	ch := make(chan result, 1)

	go func() {
		var ev, dt string
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				ch <- result{ev, dt}
				return
			}
			line = strings.TrimRight(line, "\r\n")
			switch {
			case strings.HasPrefix(line, "event:"):
				ev = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				dt = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			case line == "" && ev != "":
				ch <- result{ev, dt}
				return
			}
		}
	}()

	select {
	case res := <-ch:
		return res.event, res.data
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the first SSE event (is Flush being forwarded?)")
		return "", ""
	}
}
