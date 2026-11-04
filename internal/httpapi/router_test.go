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

func TestConnectRejectsMissingHeaders(t *testing.T) {
	srv := newTestServer(t)

	resp, err := http.Get(srv.URL + ConnectPath)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}

	var body struct {
		Error    string            `json:"error"`
		Missing  []string          `json:"missing_headers"`
		Required []string          `json:"required_headers"`
		Optional map[string]string `json:"optional_headers_with_defaults"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}

	if body.Error != "invalid_db_headers" {
		t.Errorf("error = %q", body.Error)
	}
	if len(body.Missing) != 4 {
		t.Errorf("missing = %v, want all four mandatory headers", body.Missing)
	}
	// The error must be actionable: it should tell the client what to send.
	if len(body.Required) == 0 || body.Optional["x-db-database"] != "postgres" {
		t.Errorf("error body does not document the expected headers: %+v", body)
	}
}

func TestConnectRejectsInvalidPort(t *testing.T) {
	srv := newTestServer(t)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+ConnectPath, nil)
	req.Header.Set("x-db-username", "u")
	req.Header.Set("x-db-password", "p")
	req.Header.Set("x-db-host", "db.example.com")
	req.Header.Set("x-db-port", "not-a-port")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var body struct {
		Invalid map[string]string `json:"invalid_headers"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if _, ok := body.Invalid["x-db-port"]; !ok {
		t.Errorf("expected x-db-port to be flagged, got %v", body.Invalid)
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

func TestPostWithoutSessionIDIsRejected(t *testing.T) {
	srv := newTestServer(t)

	resp, err := http.Post(srv.URL+ConnectPath, "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// POSTs skip header validation by design; they are authorised by the
	// server-issued sessionid instead, so the failure must be about that.
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "sessionid") {
		t.Errorf("expected a sessionid complaint, got: %s", body)
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
