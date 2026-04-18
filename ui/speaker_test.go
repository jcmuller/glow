//go:build unix

package ui

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSpeakerSocketPathUsesXDGRuntimeDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", dir)

	got, err := speakerSocketPath("/some/deck.md")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if filepath.Dir(got) != dir {
		t.Errorf("socket path %q not under XDG_RUNTIME_DIR %q", got, dir)
	}
	if !strings.HasPrefix(filepath.Base(got), "glow-") ||
		!strings.HasSuffix(got, ".sock") {
		t.Errorf("unexpected socket name %q", filepath.Base(got))
	}
}

func TestSpeakerSocketPathIsStableForSameFile(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	a, err := speakerSocketPath("/some/deck.md")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	b, err := speakerSocketPath("/some/deck.md")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if a != b {
		t.Errorf("path not stable: %q vs %q", a, b)
	}
}

func TestSpeakerSocketPathFallbackDirIs0700(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("TMPDIR", base)

	got, err := speakerSocketPath("/some/deck.md")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The fallback must live under TMPDIR/glow-<uid>/
	parent := filepath.Dir(got)
	if filepath.Dir(parent) != base {
		t.Fatalf("fallback parent %q not under %q", parent, base)
	}
	fi, err := os.Stat(parent)
	if err != nil {
		t.Fatalf("stat fallback dir: %v", err)
	}
	if mode := fi.Mode().Perm(); mode != 0o700 {
		t.Errorf("fallback dir mode = %o, want 0700", mode)
	}
}

func TestListenSpeakerRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	link := filepath.Join(dir, "link.sock")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if _, err := listenSpeaker(link); err == nil {
		t.Error("expected error for symlink, got nil")
	}
}

func TestListenSpeakerRefusesNonSocketFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "not-a-socket")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if _, err := listenSpeaker(path); err == nil {
		t.Error("expected error for regular file, got nil")
	}
}

func TestListenSpeakerAfterCleanClose(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.sock")

	// A previous presenter that shut down cleanly should not block the
	// next run. On Linux the listener Close removes the socket file, so
	// listenSpeaker hits the fresh-path branch.
	ln1, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("first listen: %v", err)
	}
	if err := ln1.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	ln2, err := listenSpeaker(path)
	if err != nil {
		t.Fatalf("listenSpeaker after clean close: %v", err)
	}
	_ = ln2.Close()
}

func TestListenSpeakerReusesStaleSocketFile(t *testing.T) {
	// Forge a stale socket file: create a real unix socket via syscall,
	// never listen on it, close the fd. The directory entry remains but
	// nothing is accepting, so net.DialTimeout fails and listenSpeaker
	// should unlink-and-retry exactly once.
	dir := t.TempDir()
	path := filepath.Join(dir, "stale.sock")

	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	// Keep the path as a socket, but abandon the listener without Close
	// so the socket file persists even after we stop accepting.
	// Use SetUnlinkOnClose(false) so a later Close doesn't remove the file.
	if ul, ok := ln.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(false)
	}
	_ = ln.Close()

	fi, err := os.Lstat(path)
	if err != nil {
		t.Skipf("stale socket file not preserved on this platform: %v", err)
	}
	if fi.Mode()&os.ModeSocket == 0 {
		t.Skipf("remaining file is not a socket (mode %v); skipping stale-reuse check", fi.Mode())
	}

	ln2, err := listenSpeaker(path)
	if err != nil {
		t.Fatalf("listenSpeaker on stale socket: %v", err)
	}
	_ = ln2.Close()
}

func TestListenSpeakerRefusesLivePresenter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "live.sock")

	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("first listen: %v", err)
	}
	defer ln.Close() //nolint:errcheck

	if _, err := listenSpeaker(path); err == nil {
		t.Error("expected error when a presenter is already bound, got nil")
	}
}

func TestDecodeAdvanceRejectsMalformed(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"not json", "garbage"},
		{"wrong type", `{"type":"bogus","dir":"next"}`},
		{"wrong dir", `{"type":"advance","dir":"sideways"}`},
		{"missing dir", `{"type":"advance"}`},
		{"empty type", `{"type":"","dir":"next"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := decodeAdvance([]byte(tc.in)); err == nil {
				t.Error("expected decode error, got nil")
			}
		})
	}
}

func TestDecodeAdvanceAcceptsValid(t *testing.T) {
	for _, dir := range []string{"next", "prev"} {
		msg := `{"type":"advance","dir":"` + dir + `"}`
		got, err := decodeAdvance([]byte(msg))
		if err != nil {
			t.Errorf("decodeAdvance(%q) error: %v", msg, err)
		}
		if got.Dir != dir {
			t.Errorf("decodeAdvance(%q).Dir = %q, want %q", msg, got.Dir, dir)
		}
	}
}

func TestDecodeStateRejectsMalformed(t *testing.T) {
	cases := []string{
		"not json",
		`{"type":"bogus","index":0,"total":1,"started":0}`,
		`{broken`,
	}
	for _, in := range cases {
		if _, err := decodeState([]byte(in)); err == nil {
			t.Errorf("expected decode error for %q", in)
		}
	}
}

func TestServerCapsConcurrentViewers(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	path, err := speakerSocketPath("/fixtures/caps.md")
	if err != nil {
		t.Fatalf("path: %v", err)
	}
	srv, err := startSpeakerServer(path)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer srv.Close() //nolint:errcheck

	var clients []*speakerClient
	defer func() {
		for _, c := range clients {
			_ = c.Close()
		}
	}()

	for range speakerMaxViewers {
		c, err := dialSpeaker(path)
		if err != nil {
			t.Fatalf("dial within cap: %v", err)
		}
		clients = append(clients, c)
	}

	// Let the accept loop notice the cap-full state.
	time.Sleep(50 * time.Millisecond)

	over, err := dialSpeaker(path)
	if err != nil {
		// Dial itself succeeded below the accept queue; the server
		// should have closed immediately, causing a read EOF when the
		// client tries to receive.
		t.Logf("dial-over error (acceptable): %v", err)
		return
	}
	defer over.Close() //nolint:errcheck

	// The server closed this conn at accept time. A send or receive
	// must fail within a reasonable timeout.
	select {
	case _, ok := <-over.States():
		if ok {
			t.Errorf("over-cap client should not receive state frames")
		}
	case <-time.After(500 * time.Millisecond):
		// SendAdvance should also fail on a closed connection.
		if err := over.SendAdvance("next"); err == nil {
			// On some platforms the write buffer absorbs one message
			// before ECONNRESET. Retry once.
			time.Sleep(50 * time.Millisecond)
			if err := over.SendAdvance("next"); err == nil {
				t.Errorf("over-cap client appears writable; expected dropped connection")
			}
		}
	}
}

func TestServerDropsOversizeLine(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	path, err := speakerSocketPath("/fixtures/oversize.md")
	if err != nil {
		t.Fatalf("path: %v", err)
	}
	srv, err := startSpeakerServer(path)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer srv.Close() //nolint:errcheck

	c, err := net.DialTimeout("unix", path, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close() //nolint:errcheck

	// Write one line bigger than the scanner cap.
	big := strings.Repeat("A", speakerScannerMaxSize+1024)
	if _, err := c.Write([]byte(big + "\n")); err != nil {
		t.Fatalf("write oversize: %v", err)
	}

	// No advance should arrive: the scanner errors out and drops the
	// connection. Accept a small drain window.
	select {
	case msg := <-srv.Advances():
		t.Errorf("unexpected advance from oversize line: %+v", msg)
	case <-time.After(150 * time.Millisecond):
		// Expected: nothing arrives.
	}
}

func TestSpeakerRoundTripAdvanceAndState(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	path, err := speakerSocketPath("/fixtures/roundtrip.md")
	if err != nil {
		t.Fatalf("path: %v", err)
	}
	srv, err := startSpeakerServer(path)
	if err != nil {
		t.Fatalf("start server: %v", err)
	}
	defer srv.Close() //nolint:errcheck

	cli, err := dialSpeaker(path)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer cli.Close() //nolint:errcheck

	// Presenter publishes initial state.
	srv.Broadcast(stateMsg{Type: "state", Index: 0, Total: 3, Started: 111})

	select {
	case st, ok := <-cli.States():
		if !ok {
			t.Fatal("viewer channel closed before initial state")
		}
		if st.Index != 0 || st.Total != 3 || st.Started != 111 {
			t.Errorf("viewer got %+v, want index=0 total=3 started=111", st)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timeout waiting for initial state")
	}

	// Viewer requests the next slide.
	if err := cli.SendAdvance("next"); err != nil {
		t.Fatalf("send advance: %v", err)
	}

	select {
	case ad := <-srv.Advances():
		if ad.Dir != "next" {
			t.Errorf("presenter got dir %q, want %q", ad.Dir, "next")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timeout waiting for advance on presenter")
	}

	// Presenter replies with the new slide index.
	srv.Broadcast(stateMsg{Type: "state", Index: 1, Total: 3, Started: 111})

	select {
	case st := <-cli.States():
		if st.Index != 1 {
			t.Errorf("viewer got index %d after advance, want 1", st.Index)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timeout waiting for post-advance state")
	}
}

func TestServerRateLimitsAdvances(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	path, err := speakerSocketPath("/fixtures/ratelimit.md")
	if err != nil {
		t.Fatalf("path: %v", err)
	}
	srv, err := startSpeakerServer(path)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer srv.Close() //nolint:errcheck

	c, err := net.DialTimeout("unix", path, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close() //nolint:errcheck

	// Blast 100 valid advances as fast as possible. The token bucket
	// starts full (burst=5) and refills at 10/s, so over the ~10 ms
	// the writes take, at most burst+epsilon should make it through.
	payload := []byte(`{"type":"advance","dir":"next"}` + "\n")
	for range 100 {
		if _, err := c.Write(payload); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	// Drain for 150 ms and count accepted advances.
	got := 0
	deadline := time.After(150 * time.Millisecond)
drain:
	for {
		select {
		case <-srv.Advances():
			got++
		case <-deadline:
			break drain
		}
	}

	if got > 15 {
		t.Errorf("rate limiter let through %d/100 advances, want ≤15", got)
	}
	if got < 1 {
		t.Errorf("rate limiter blocked everything (%d), want at least the burst", got)
	}
}

func TestBroadcastDoesNotStallOnStuckViewer(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	path, err := speakerSocketPath("/fixtures/stuck-viewer.md")
	if err != nil {
		t.Fatalf("path: %v", err)
	}
	srv, err := startSpeakerServer(path)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer srv.Close() //nolint:errcheck

	// Raw unix dial that never reads. The server's write buffer will
	// fill eventually; Broadcast must not block the caller.
	c, err := net.DialTimeout("unix", path, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close() //nolint:errcheck

	// Give the server's accept loop time to register the conn.
	time.Sleep(50 * time.Millisecond)

	payload := stateMsg{Type: "state", Index: 0, Total: 10000, Started: 1}

	done := make(chan struct{})
	go func() {
		for range 20000 {
			srv.Broadcast(payload)
		}
		close(done)
	}()

	select {
	case <-done:
		// Success: Broadcast returned for every call even though the
		// peer never drained its socket.
	case <-time.After(2 * time.Second):
		t.Fatal("Broadcast stalled when the viewer stopped reading")
	}
}

func TestSendAdvanceDoesNotStallOnStuckPresenter(t *testing.T) {
	// Stand up a raw listener that accepts one conn and never reads.
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "stuck-presenter.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close() //nolint:errcheck

	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		accepted <- c
	}()

	cli, err := dialSpeaker(sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer cli.Close() //nolint:errcheck

	var serverSide net.Conn
	select {
	case serverSide = <-accepted:
		defer serverSide.Close() //nolint:errcheck
	case <-time.After(500 * time.Millisecond):
		t.Fatal("server never accepted")
	}

	done := make(chan struct{})
	go func() {
		for range 20000 {
			_ = cli.SendAdvance("next")
		}
		close(done)
	}()

	select {
	case <-done:
		// Success.
	case <-time.After(2 * time.Second):
		t.Fatal("SendAdvance stalled when the presenter stopped reading")
	}
}

func TestTokenBucketBurstAndRefill(t *testing.T) {
	b := newTokenBucket(10, 5)
	allowed := 0
	for range 100 {
		if b.allow() {
			allowed++
		}
	}
	// No sleep between calls → only the burst capacity (5) should pass,
	// minus one tick of natural refill that may add ~0 tokens.
	if allowed > 6 {
		t.Errorf("burst allowed %d messages, want ≤6", allowed)
	}
	if allowed < 5 {
		t.Errorf("burst allowed %d messages, want ≥5", allowed)
	}

	// After 1 second, ~10 more tokens should have refilled (capped at burst).
	time.Sleep(1100 * time.Millisecond)
	allowed = 0
	for range 100 {
		if b.allow() {
			allowed++
		}
	}
	if allowed > 6 {
		t.Errorf("after refill allowed %d messages, want ≤6", allowed)
	}
}
