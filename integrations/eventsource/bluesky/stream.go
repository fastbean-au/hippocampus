package bluesky

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
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

// defaultReadTimeout is used when Config.ReadTimeout is unset. The firehose is never idle, so
// silence means a black-holed connection rather than a quiet topic.
const defaultReadTimeout = 30 * time.Second

// wsStream is the real Jetstream connection.
type wsStream struct {
	conn    *websocket.Conn
	timeout time.Duration
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

	return &wsStream{conn: conn, timeout: timeout}, nil
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
// at once: the read deadline bounds how long a silent connection blocks, and a watchdog closes the
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

func (w *wsStream) Close() error {
	return w.conn.Close()
}
