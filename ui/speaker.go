//go:build unix

package ui

import (
	"bufio"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/charmbracelet/log"
)

const (
	speakerMaxViewers     = 4
	speakerScannerMaxSize = 64 * 1024
	speakerDialTimeout    = 100 * time.Millisecond
	speakerAdvanceRate    = 10 // tokens per second
	speakerAdvanceBurst   = 5  // bucket capacity
)

// stateMsg is the presenter-to-viewer frame sent on slide changes.
type stateMsg struct {
	Type    string `json:"type"` // always "state"
	Index   int    `json:"index"`
	Total   int    `json:"total"`
	Started int64  `json:"started"` // unix millis at presenter start
}

// advanceMsg is the viewer-to-presenter frame that requests navigation.
type advanceMsg struct {
	Type string `json:"type"` // always "advance"
	Dir  string `json:"dir"`  // "next" or "prev"
}

// speakerSocketPath returns the unix socket path that identifies
// filePath's presentation. $XDG_RUNTIME_DIR is preferred when set; the
// fallback is a 0700 per-uid subdirectory of os.TempDir() so a shared
// /tmp does not leak state across users.
func speakerSocketPath(filePath string) (string, error) {
	abs, err := filepath.Abs(filePath)
	if err != nil {
		return "", fmt.Errorf("abs path: %w", err)
	}
	sum := sha256.Sum256([]byte(abs))
	name := fmt.Sprintf("glow-%x.sock", sum[:8])

	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, name), nil
	}

	base := filepath.Join(os.TempDir(), fmt.Sprintf("glow-%d", os.Getuid()))
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", fmt.Errorf("mkdir fallback: %w", err)
	}
	return filepath.Join(base, name), nil
}

// listenSpeaker binds a unix socket at path. It refuses symlinks,
// non-socket files, and paths where a live presenter is already
// accepting. A stale socket file (no listener dial succeeds) is
// removed once, and the listen is retried.
func listenSpeaker(path string) (net.Listener, error) {
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("refusing to bind: path is a symlink")
		}
		if fi.Mode()&os.ModeSocket == 0 {
			return nil, errors.New("refusing to bind: path exists and is not a socket")
		}
		if c, derr := net.DialTimeout("unix", path, speakerDialTimeout); derr == nil {
			_ = c.Close()
			return nil, errors.New("another presenter is already running for this file")
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove stale socket: %w", err)
		}
	}
	return net.Listen("unix", path)
}

// decodeAdvance parses one newline-delimited JSON frame from a viewer.
// Unknown enum values count as a parse error; callers are expected to
// log and drop the connection rather than touch presenter state.
func decodeAdvance(line []byte) (advanceMsg, error) {
	var m advanceMsg
	if err := json.Unmarshal(line, &m); err != nil {
		return m, fmt.Errorf("decode advance: %w", err)
	}
	if m.Type != "advance" {
		return m, fmt.Errorf("unknown advance type %q", m.Type)
	}
	if m.Dir != "next" && m.Dir != "prev" {
		return m, fmt.Errorf("unknown advance dir %q", m.Dir)
	}
	return m, nil
}

// decodeState parses one JSON frame from the presenter. A parse error
// is the viewer's signal to hang up the connection, not crash.
func decodeState(line []byte) (stateMsg, error) {
	var m stateMsg
	if err := json.Unmarshal(line, &m); err != nil {
		return m, fmt.Errorf("decode state: %w", err)
	}
	if m.Type != "state" {
		return m, fmt.Errorf("unknown state type %q", m.Type)
	}
	return m, nil
}

// tokenBucket is a rate limiter. The bucket holds up to burst tokens
// and refills at rate tokens/second. allow() consumes one token or
// returns false. Callers may share a bucket across goroutines.
type tokenBucket struct {
	rate  float64
	burst float64

	mu     sync.Mutex
	tokens float64
	last   time.Time
}

func newTokenBucket(rate, burst float64) *tokenBucket {
	return &tokenBucket{
		rate:   rate,
		burst:  burst,
		tokens: burst,
		last:   time.Now(),
	}
}

func (b *tokenBucket) allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	elapsed := now.Sub(b.last).Seconds()
	b.last = now
	b.tokens += elapsed * b.rate
	if b.tokens > b.burst {
		b.tokens = b.burst
	}
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

// newScanner wraps r in a bufio.Scanner capped at 64 KiB. A line that
// exceeds the cap stops the scan with bufio.ErrTooLong, which ends the
// peer's connection.
func newScanner(r io.Reader) *bufio.Scanner {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 0, 4096), speakerScannerMaxSize)
	return s
}

// speakerServer is the presenter side of the speaker-view wire. The
// presenter publishes state via Broadcast and reads remote advance
// requests from the Advances channel.
type speakerServer struct {
	ln   net.Listener
	path string

	advances chan advanceMsg
	done     chan struct{}

	mu        sync.Mutex
	conns     map[*viewerConn]struct{}
	lastState stateMsg
	hasState  bool
}

// viewerConn wraps one accepted connection. Writes go through the
// writer goroutine via publish(), so a peer that stops draining its
// socket cannot stall the presenter's key handler.
type viewerConn struct {
	c      net.Conn
	enc    *json.Encoder
	bucket *tokenBucket

	mu       sync.Mutex
	pending  *stateMsg
	signal   chan struct{}
	writerCh chan struct{} // closed when writeLoop exits
}

// publish queues st for delivery. Latest-state-wins: if an older state
// is still pending, publish replaces it in place. The writer goroutine
// picks up whatever is current when it wakes up.
func (vc *viewerConn) publish(st stateMsg) {
	vc.mu.Lock()
	vc.pending = &st
	vc.mu.Unlock()
	select {
	case vc.signal <- struct{}{}:
	default:
	}
}

// take returns and clears any pending state, or (_, false) if nothing
// is waiting.
func (vc *viewerConn) take() (stateMsg, bool) {
	vc.mu.Lock()
	defer vc.mu.Unlock()
	if vc.pending == nil {
		return stateMsg{}, false
	}
	st := *vc.pending
	vc.pending = nil
	return st, true
}

func startSpeakerServer(path string) (*speakerServer, error) {
	ln, err := listenSpeaker(path)
	if err != nil {
		return nil, err
	}
	s := &speakerServer{
		ln:       ln,
		path:     path,
		advances: make(chan advanceMsg, 16),
		done:     make(chan struct{}),
		conns:    map[*viewerConn]struct{}{},
	}
	go s.acceptLoop()
	return s, nil
}

func (s *speakerServer) acceptLoop() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			select {
			case <-s.done:
				return
			default:
				log.Debug("speaker accept error", "error", err)
				return
			}
		}
		s.mu.Lock()
		if len(s.conns) >= speakerMaxViewers {
			s.mu.Unlock()
			log.Info("speaker viewer cap reached, closing incoming connection")
			_ = c.Close()
			continue
		}
		vc := &viewerConn{
			c:        c,
			enc:      json.NewEncoder(c),
			bucket:   newTokenBucket(speakerAdvanceRate, speakerAdvanceBurst),
			signal:   make(chan struct{}, 1),
			writerCh: make(chan struct{}),
		}
		s.conns[vc] = struct{}{}
		replay, ok := s.lastState, s.hasState
		s.mu.Unlock()
		go s.writeLoop(vc)
		if ok {
			vc.publish(replay)
		}
		go s.readLoop(vc)
	}
}

// writeLoop owns all writes to a viewer. It wakes on vc.signal, picks
// up whatever is pending, and Encodes it. When the socket closes or
// Encode errors, it drops the connection and exits.
func (s *speakerServer) writeLoop(vc *viewerConn) {
	defer close(vc.writerCh)
	for {
		select {
		case <-s.done:
			return
		case _, ok := <-vc.signal:
			if !ok {
				return
			}
		}
		st, has := vc.take()
		if !has {
			continue
		}
		if err := vc.enc.Encode(st); err != nil {
			log.Debug("speaker: broadcast encode failed", "error", err)
			s.dropConn(vc)
			return
		}
	}
}

func (s *speakerServer) readLoop(vc *viewerConn) {
	defer s.dropConn(vc)
	scanner := newScanner(vc.c)
	for scanner.Scan() {
		line := scanner.Bytes()
		msg, err := decodeAdvance(line)
		if err != nil {
			log.Debug("speaker: bad advance from viewer", "error", err)
			continue
		}
		if !vc.bucket.allow() {
			log.Debug("speaker: rate-limited advance dropped")
			continue
		}
		select {
		case s.advances <- msg:
		case <-s.done:
			return
		}
	}
	if err := scanner.Err(); err != nil {
		log.Debug("speaker: scanner error", "error", err)
	}
}

func (s *speakerServer) dropConn(vc *viewerConn) {
	s.mu.Lock()
	delete(s.conns, vc)
	s.mu.Unlock()
	_ = vc.c.Close()
}

// Broadcast caches st and hands it to every connected viewer's writer
// goroutine. Never blocks on a slow peer: per-conn writes are async
// and "latest state wins," so a stuck viewer only loses intermediate
// updates.
func (s *speakerServer) Broadcast(st stateMsg) {
	s.mu.Lock()
	s.lastState = st
	s.hasState = true
	targets := make([]*viewerConn, 0, len(s.conns))
	for vc := range s.conns {
		targets = append(targets, vc)
	}
	s.mu.Unlock()
	for _, vc := range targets {
		vc.publish(st)
	}
}

// Advances returns the channel of advance requests from viewers.
func (s *speakerServer) Advances() <-chan advanceMsg {
	return s.advances
}

// Close stops accepting, closes every viewer connection, and removes
// the socket file.
func (s *speakerServer) Close() error {
	select {
	case <-s.done:
		return nil
	default:
		close(s.done)
	}
	_ = s.ln.Close()
	s.mu.Lock()
	for vc := range s.conns {
		_ = vc.c.Close()
	}
	s.conns = map[*viewerConn]struct{}{}
	s.mu.Unlock()
	_ = os.Remove(s.path)
	return nil
}

// speakerClient is the viewer side. Reads go through a background
// goroutine; writes go through an outbox channel so a stuck presenter
// cannot stall the local key handler.
type speakerClient struct {
	c      net.Conn
	enc    *json.Encoder
	states chan stateMsg
	outbox chan advanceMsg
	done   chan struct{}
}

const speakerOutboxSize = 16

func dialSpeaker(path string) (*speakerClient, error) {
	c, err := net.DialTimeout("unix", path, 2*speakerDialTimeout)
	if err != nil {
		return nil, fmt.Errorf("dial speaker: %w", err)
	}
	cl := &speakerClient{
		c:      c,
		enc:    json.NewEncoder(c),
		states: make(chan stateMsg, 8),
		outbox: make(chan advanceMsg, speakerOutboxSize),
		done:   make(chan struct{}),
	}
	go cl.readLoop()
	go cl.writeLoop()
	return cl, nil
}

// writeLoop drains the outbox and encodes to the socket. On any write
// error the loop exits; subsequent SendAdvance calls will still queue
// into a full outbox and be dropped, so they never block either.
func (cl *speakerClient) writeLoop() {
	for {
		select {
		case <-cl.done:
			return
		case m := <-cl.outbox:
			if err := cl.enc.Encode(m); err != nil {
				log.Debug("viewer: send encode failed", "error", err)
				return
			}
		}
	}
}

func (cl *speakerClient) readLoop() {
	defer close(cl.states)
	scanner := newScanner(cl.c)
	for scanner.Scan() {
		st, err := decodeState(scanner.Bytes())
		if err != nil {
			log.Debug("viewer: bad state frame", "error", err)
			return
		}
		select {
		case cl.states <- st:
		case <-cl.done:
			return
		}
	}
}

// SendAdvance asks the presenter to move in dir ("next" or "prev").
// The call is non-blocking: if the outbox is full (a stuck presenter)
// the request is dropped so the viewer's key handler stays responsive.
func (cl *speakerClient) SendAdvance(dir string) error {
	select {
	case cl.outbox <- advanceMsg{Type: "advance", Dir: dir}:
		return nil
	default:
		log.Debug("viewer: outbox full, dropping advance", "dir", dir)
		return nil
	}
}

// States returns the channel of incoming state frames from the presenter.
func (cl *speakerClient) States() <-chan stateMsg {
	return cl.states
}

func (cl *speakerClient) Close() error {
	select {
	case <-cl.done:
		return nil
	default:
		close(cl.done)
	}
	return cl.c.Close()
}
