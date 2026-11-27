// Package rtm receives what happens in a workspace as it happens, over the
// WebSocket of Slack's Real Time Messaging API (rtm.connect).
//
// Slack pushes there every event of the conversations the caller is a
// member of: messages, thread replies included, whether read or not. It
// works with browser session credentials and with classic tokens; the
// token of a granular-scope Slack app cannot call rtm.connect.
package rtm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	"golang.org/x/net/websocket"

	"github.com/maelvls/slackcli/internal/audit"
	"github.com/maelvls/slackcli/internal/jsonx"
	"github.com/maelvls/slackcli/internal/slackapi"
)

const (
	// defaultPingInterval is how often a ping is sent. Slack answers each
	// with a pong, which is how a connection that died silently (a laptop
	// that slept, a network that changed) is noticed.
	defaultPingInterval = 30 * time.Second
	// defaultReadTimeout is how long the connection may stay silent: more
	// than two pings went unanswered.
	defaultReadTimeout = 75 * time.Second
	// maxBackoff bounds the pause between two attempts to reconnect.
	maxBackoff = 30 * time.Second
)

// Options tunes Stream.
type Options struct {
	// OnConnect is called each time the connection is ready, with how
	// long the stream was down (0 the first time): the time since Slack
	// was last heard on the lost connection, the sleep of the machine
	// included. The events of that gap are lost: Slack does not replay
	// them.
	OnConnect func(gap time.Duration)
	// OnDisconnect is called when the connection is lost and Stream is
	// about to reconnect.
	OnDisconnect func(err error)

	// PingInterval, ReadTimeout and Backoff (the pause before the n-th
	// attempt to reconnect) have sound defaults; tests shorten them.
	PingInterval, ReadTimeout time.Duration
	Backoff                   func(attempt int) time.Duration
}

// Stream connects and calls handle with each event Slack sends, in order,
// until ctx is done (it then returns nil) or handle fails (it returns that
// error). The events of the protocol itself (hello, pong, goodbye) are not
// passed on.
//
// A lost connection is reopened, for as long as it takes, with a growing
// pause. The first connection must succeed, and credentials that Slack
// rejects end the stream whenever that happens.
func Stream(ctx context.Context, client *slackapi.Client, opts Options, handle func(event map[string]any) error) error {
	if opts.PingInterval <= 0 {
		opts.PingInterval = defaultPingInterval
	}
	if opts.ReadTimeout <= 0 {
		opts.ReadTimeout = defaultReadTimeout
	}
	if opts.Backoff == nil {
		opts.Backoff = backoff
	}

	var (
		// lastFrame is when Slack was last heard. It is a wall clock time
		// (no monotonic reading): the monotonic clock stops while the
		// machine sleeps, which is when most events are missed.
		lastFrame time.Time
		// lostAt is since when events may be missing; zero while connected.
		lostAt        time.Time
		everConnected bool
		attempt       int
	)
	// alive and hello run on this goroutine, from serve.
	alive := func() { lastFrame = time.Now().Round(0) }
	hello := func() {
		var gap time.Duration
		if !lostAt.IsZero() {
			// Never 0, which means the first connection, even if the
			// clock was set back meanwhile.
			gap = max(time.Since(lostAt), time.Nanosecond)
		}
		lostAt, everConnected, attempt = time.Time{}, true, 0
		if opts.OnConnect != nil {
			opts.OnConnect(gap)
		}
	}
	for {
		served := false
		ws, err := dial(ctx, client)
		if err == nil {
			served = true
			err = serve(ctx, ws, opts, alive, hello, handle)
			var he *handlerError
			if errors.As(err, &he) {
				return he.err
			}
		}
		switch {
		case ctx.Err() != nil:
			return nil
		case !everConnected:
			if served {
				// The error of a read alone ("EOF") says nothing.
				return fmt.Errorf("the real-time connection ended before Slack said it was ready: %w", err)
			}
			return err
		case slackapi.IsAuthError(err):
			return err
		}
		if lostAt.IsZero() {
			// A connection found dead (no answer to the pings, a machine
			// that slept) died some time after Slack was last heard, not
			// when that was noticed.
			lostAt = lastFrame
		}
		if opts.OnDisconnect != nil {
			opts.OnDisconnect(err)
		}
		attempt++
		wait := opts.Backoff(attempt)
		// rtm.connect is rate limited: asking again before the time Slack
		// gave only pushes the end of the limit further away.
		var limited *slackapi.RateLimitError
		if errors.As(err, &limited) {
			wait = max(wait, limited.RetryAfter)
		}
		pause := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			pause.Stop()
			return nil
		case <-pause.C:
		}
	}
}

// backoff is the pause before the attempt-th reconnection: 1s, 2s, 4s… up
// to maxBackoff.
func backoff(attempt int) time.Duration {
	return min(time.Second<<min(attempt-1, 10), maxBackoff)
}

// handlerError marks an error of the caller's handle, as opposed to one of
// the connection.
type handlerError struct{ err error }

func (e *handlerError) Error() string { return e.err.Error() }

// dial asks Slack for a WebSocket URL and opens it.
func dial(ctx context.Context, client *slackapi.Client) (*websocket.Conn, error) {
	resp, err := client.API(ctx, "rtm.connect", map[string]any{"batch_presence_aware": true})
	if err != nil {
		if slackapi.IsErrorCode(err, "not_allowed_token_type", "missing_scope") {
			return nil, fmt.Errorf("%w (the real-time API needs browser session credentials or a classic token: the token of a Slack app cannot use it)", err)
		}
		return nil, err
	}
	raw := jsonx.Str(resp["url"])
	u, err := url.Parse(raw)
	if err != nil || raw == "" {
		return nil, errors.New("Slack API rtm.connect returned no WebSocket URL")
	}
	header, err := client.WebSocketHeader(u)
	if err != nil {
		return nil, err
	}
	origin := header.Get("Origin")
	if origin == "" {
		origin = "https://slack.com"
	}
	header.Del("Origin") // the handshake writes the one of the config
	config, err := websocket.NewConfig(raw, origin)
	if err != nil {
		return nil, fmt.Errorf("opening the real-time connection to %s: invalid address", u.Host)
	}
	config.Header = header
	dialCtx, cancel := context.WithTimeout(ctx, slackapi.APITimeout())
	defer cancel()
	start := time.Now()
	ws, err := config.DialContext(dialCtx)
	recordDial(dialCtx, start, u.Host, err)
	if err != nil {
		// A DialError names the URL, which carries a token.
		var de *websocket.DialError
		if errors.As(err, &de) {
			err = de.Err
		}
		return nil, fmt.Errorf("opening the real-time connection to %s: %w", u.Host, err)
	}
	return ws, nil
}

// recordDial writes the audit line of the opening of the WebSocket to
// host, started at start, which failed with err (nil for success).
func recordDial(ctx context.Context, start time.Time, host string, err error) {
	e := audit.Entry{
		TS:         start.UTC().Format(time.RFC3339Nano),
		Kind:       audit.KindWebSocket,
		Host:       host,
		Method:     "websocket.connect",
		DurationMS: time.Since(start).Milliseconds(),
	}
	if err != nil {
		e.Error = "dial_failed"
		if class := audit.ErrorClass(ctx, err); class != "network" {
			e.Error = class
		}
	} else {
		e.Status = http.StatusSwitchingProtocols
	}
	audit.Record(e)
}

// serve reads ws until it fails, ctx is done or Slack says goodbye. It
// calls alive each time something is received, and hello when Slack says
// the connection is ready.
func serve(ctx context.Context, ws *websocket.Conn, opts Options, alive, hello func(), handle func(map[string]any) error) error {
	// A read cannot be cancelled: closing the connection is what ends it.
	stop := context.AfterFunc(ctx, func() { ws.Close() })
	defer stop()

	done := make(chan struct{})
	var wg sync.WaitGroup
	defer wg.Wait()
	defer ws.Close()
	defer close(done)
	wg.Go(func() {
		t := time.NewTicker(opts.PingInterval)
		defer t.Stop()
		for id := 1; ; id++ {
			select {
			case <-done:
				return
			case <-t.C:
				_ = ws.SetWriteDeadline(time.Now().Add(opts.ReadTimeout))
				if err := websocket.JSON.Send(ws, map[string]any{"type": "ping", "id": id}); err != nil {
					ws.Close()
					return
				}
			}
		}
	})

	for {
		_ = ws.SetReadDeadline(time.Now().Add(opts.ReadTimeout))
		var data []byte
		if err := websocket.Message.Receive(ws, &data); err != nil {
			return err
		}
		alive()
		var event map[string]any
		if json.Unmarshal(data, &event) != nil {
			continue
		}
		switch jsonx.Str(event["type"]) {
		case "hello":
			hello()
		case "pong", "":
			// "": the acknowledgement of something sent.
		case "goodbye":
			return errors.New("Slack asked to reconnect")
		case "error":
			return fmt.Errorf("Slack reported an error on the real-time connection: %s", jsonx.GetStr(event, "error", "msg"))
		default:
			if err := handle(event); err != nil {
				return &handlerError{err}
			}
		}
	}
}
