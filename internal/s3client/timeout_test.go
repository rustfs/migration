package s3client

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestStalledUploadWriteTimesOut(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	conn := &writeTimeoutConn{Conn: client, timeout: 30 * time.Millisecond}
	_, err := conn.Write([]byte("the peer never reads"))
	var timeout net.Error
	if !errors.As(err, &timeout) || !timeout.Timeout() {
		t.Fatalf("stalled upload: %v", err)
	}
}

func TestStalledBodyReadTimesOut(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer s.Close()
	c := &http.Client{Transport: idleTimeoutTransport{base: http.DefaultTransport, timeout: 30 * time.Millisecond}}
	resp, err := c.Get(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, err = io.ReadAll(resp.Body)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stalled read: %v", err)
	}
}

func TestBandwidthPacingBetweenReadsDoesNotTimeOut(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "ab") }))
	defer s.Close()
	c := &http.Client{Transport: idleTimeoutTransport{base: http.DefaultTransport, timeout: 30 * time.Millisecond}}
	resp, err := c.Get(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 1)
	if _, err = io.ReadFull(resp.Body, buf); err != nil {
		t.Fatal(err)
	}
	time.Sleep(60 * time.Millisecond)
	if _, err = io.ReadFull(resp.Body, buf); err != nil {
		t.Fatal(err)
	}
	if buf[0] != 'b' {
		t.Fatal("wrong body")
	}
}
