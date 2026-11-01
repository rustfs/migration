package s3client

import (
	"context"
	"io"
	"net"
	"net/http"
	"time"
)

type writeTimeoutConn struct {
	net.Conn
	timeout time.Duration
}

func (c *writeTimeoutConn) Write(p []byte) (int, error) {
	if err := c.Conn.SetWriteDeadline(time.Now().Add(c.timeout)); err != nil {
		return 0, err
	}
	n, err := c.Conn.Write(p)
	_ = c.Conn.SetWriteDeadline(time.Time{})
	return n, err
}

// Bound individual blocked network reads, not time spent pacing a source body.
// A slow configured bandwidth cap must not trip the idle watchdog between reads.
type idleTimeoutTransport struct {
	base    http.RoundTripper
	timeout time.Duration
}

func (t idleTimeoutTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithCancelCause(req.Context())
	resp, err := t.base.RoundTrip(req.Clone(ctx))
	if err != nil {
		cancel(err)
		return nil, err
	}
	resp.Body = &idleTimeoutBody{ReadCloser: resp.Body, ctx: ctx, cancel: cancel, timeout: t.timeout}
	return resp, nil
}

type idleTimeoutBody struct {
	io.ReadCloser
	ctx     context.Context
	cancel  context.CancelCauseFunc
	timeout time.Duration
}

func (b *idleTimeoutBody) Read(p []byte) (int, error) {
	timer := time.AfterFunc(b.timeout, func() { b.cancel(context.DeadlineExceeded) })
	n, err := b.ReadCloser.Read(p)
	timer.Stop()
	if err != nil && context.Cause(b.ctx) != nil {
		return n, context.Cause(b.ctx)
	}
	return n, err
}

func (b *idleTimeoutBody) Close() error { b.cancel(nil); return b.ReadCloser.Close() }
