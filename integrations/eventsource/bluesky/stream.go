package bluesky

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// The only file in this package that opens a socket. Everything else is a pure function of a decoded
// frame, which is what lets the consume loop be driven end to end from a canned slice of frames.

// readLimit bounds one frame. Jetstream records are small (a post is capped at 300 graphemes); this
// is generous enough for an embed-heavy record and small enough that a malformed length prefix
// cannot make the bridge allocate unboundedly.
const readLimit = 1 << 20

// handshakeTimeout bounds the websocket upgrade.
const handshakeTimeout = 15 * time.Second

// defaultReadTimeout is used when Config.ReadTimeout is unset. The whole firehose is never idle, but
// a subscription narrowed by wantedDids can be silent for hours, so the deadline is held open by
// keepalive pongs as well as frames: only a connection that stops answering is taken for dead.
const defaultReadTimeout = 30 * time.Second

// pingsPerTimeout is how many keepalive pings are sent per read timeout, so one lost pong does not
// end a live connection. Jetstream's own server ping is every 30 seconds, which races a 30-second
// deadline, so the bridge cannot rely on it alone.
const pingsPerTimeout = 3

// pingWriteTimeout bounds the write of one keepalive ping.
const pingWriteTimeout = 5 * time.Second

// wsStream is the real Jetstream connection.
type wsStream struct {
	conn      *websocket.Conn
	timeout   time.Duration
	stop      chan struct{}
	closeOnce sync.Once
}

// defaultDial opens a Jetstream subscription at the given resume cursor.
func defaultDial(ctx context.Context, cfg Config, cursor int64) (stream, error) {
	endpoint, err := subscribeURL(cfg, cursor)
	if err != nil {
		return nil, err
	}

	dialer := &websocket.Dialer{
		HandshakeTimeout: handshakeTimeout,
		// Jetstream's compression needs a server-supplied zstd dictionary, which would mean a new
		// module and a dictionary fetch for no benefit here.
		EnableCompression: false,
	}

	conn, resp, err := dialer.DialContext(ctx, endpoint, nil)
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("dialling Jetstream at %s: %w (HTTP %s)", cfg.URL, err, resp.Status)
		}

		return nil, fmt.Errorf("dialling Jetstream at %s: %w", cfg.URL, err)
	}

	conn.SetReadLimit(readLimit)

	timeout := cfg.ReadTimeout
	if timeout <= 0 {
		timeout = defaultReadTimeout
	}

	w := &wsStream{
		conn:    conn,
		timeout: timeout,
		stop:    make(chan struct{}),
	}

	w.keepAlive()

	return w, nil
}

// keepAlive makes a pong to the bridge's own ping, and a ping from the server, count as proof the
// connection is live by extending the read deadline, then pings every timeout/pingsPerTimeout until
// Close. Both handlers run inside ReadMessage, so a deadline they set is the one that read honours.
func (w *wsStream) keepAlive() {
	w.conn.SetPongHandler(func(string) error {
		return w.conn.SetReadDeadline(time.Now().Add(w.timeout))
	})

	w.conn.SetPingHandler(func(data string) error {
		if err := w.conn.SetReadDeadline(time.Now().Add(w.timeout)); err != nil {
			return err
		}

		// The default handler's reply, which replacing it would otherwise drop.
		err := w.conn.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(pingWriteTimeout))
		if errors.Is(err, websocket.ErrCloseSent) {
			return nil
		}

		return err
	})

	interval := w.timeout / pingsPerTimeout
	if interval <= 0 {
		interval = w.timeout
	}

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {

			case <-w.stop:
				return

			case <-ticker.C:
				// A failed write is left to the read deadline: a connection that cannot carry a
				// ping will not carry a pong either, and the read is what reports it.
				_ = w.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(pingWriteTimeout))
			}
		}
	}()
}

// subscribeURL builds the /subscribe URL. Split out from defaultDial so the query construction -
// repeated wantedCollections, repeated wantedDids, cursor only when set - is testable without a
// server.
func subscribeURL(cfg Config, cursor int64) (string, error) {
	base := cfg.URL
	if base == "" {
		base = DefaultURL
	}

	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("parsing the Jetstream URL %q: %w", base, err)
	}

	q := u.Query()

	for _, v := range cfg.Collections {
		q.Add("wantedCollections", v)
	}

	for _, v := range cfg.DIDs {
		q.Add("wantedDids", v)
	}

	// A zero cursor is omitted entirely, which is what starts the subscription at the live tip.
	if cursor > 0 {
		q.Set("cursor", strconv.FormatInt(cursor, 10))
	}

	u.RawQuery = q.Encode()

	return u.String(), nil
}

// Next reads one frame.
//
// The underlying ReadMessage takes no context, only a deadline, so cancellation is handled two ways
// at once: the read deadline bounds how long an unanswering connection blocks, and a watchdog closes the
// connection when the context is cancelled - which is what makes an already-blocked read return
// rather than wait out the timeout.
func (w *wsStream) Next(ctx context.Context) ([]byte, error) {
	if err := w.conn.SetReadDeadline(time.Now().Add(w.timeout)); err != nil {
		return nil, err
	}

	done := make(chan struct{})
	defer close(done)

	go func() {
		select {

		case <-ctx.Done():
			_ = w.conn.Close()

		case <-done:
		}
	}()

	_, data, err := w.conn.ReadMessage()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		return nil, err
	}

	return data, nil
}

// Close stops the keepalive pings and closes the connection. Safe to call more than once.
func (w *wsStream) Close() error {
	w.closeOnce.Do(func() { close(w.stop) })

	return w.conn.Close()
}
