package gws

import (
	"compress/flate"
	"context"
	"errors"
	"github.com/lxzan/gws/internal"
	"net"
	"net/http"
	"strings"
	"time"
)

const (
	DefaultMessageChannelBufferSize = 16
	DefaultHandshakeTimeout         = 5 * time.Second
	DefaultReadTimeout              = 30 * time.Second
	DefaultWriteTimeout             = 30 * time.Second
	DefaultCompressLevel            = flate.BestSpeed
	DefaultMaxContentLength         = 1 * 1024 * 1024 // 1MiB
)

type (
	Upgrader struct {
		// whether to show error log, dv=true
		LogEnabled bool

		// whether to compress data, dv = false
		CompressEnabled bool

		// compress level eg: flate.BestSpeed
		CompressLevel int

		// websocket  handshake timeout, dv=3s
		HandshakeTimeout time.Duration

		// max message size, dv=1024*1024 (1MiB)
		MaxContentLength int

		// message channel buffer size, dv=16
		MessageChannelBufferSize int

		// read frame timeout, dv=5s
		ReadTimeout time.Duration

		// write frame timeout, dv=5s
		WriteTimeout time.Duration

		// set response header
		Header http.Header

		// filter user request
		CheckOrigin func(r *Request) bool
	}

	Request struct {
		*http.Request               // http request
		Storage       *internal.Map // store user session
	}
)

func (c *Upgrader) initialize() {
	if c.CheckOrigin == nil {
		c.CheckOrigin = func(r *Request) bool {
			return true
		}
	}
	if c.MessageChannelBufferSize <= 0 {
		c.MessageChannelBufferSize = DefaultMessageChannelBufferSize
	}
	if c.HandshakeTimeout <= 0 {
		c.HandshakeTimeout = DefaultHandshakeTimeout
	}
	if c.MaxContentLength <= 0 {
		c.MaxContentLength = DefaultMaxContentLength
	}
	if c.ReadTimeout <= 0 {
		c.ReadTimeout = DefaultReadTimeout
	}
	if c.WriteTimeout <= 0 {
		c.WriteTimeout = DefaultWriteTimeout
	}
	if c.CompressEnabled && c.CompressLevel == 0 {
		c.CompressLevel = DefaultCompressLevel
	}
}

func (c *Upgrader) handshake(conn net.Conn, websocketKey string) error {
	var buf = make([]byte, 0, 256)
	buf = append(buf, "HTTP/1.1 101 Switching Protocols\r\n"...)
	buf = append(buf, "Upgrade: websocket\r\n"...)
	buf = append(buf, "Connection: Upgrade\r\n"...)
	buf = append(buf, "Sec-WebSocket-Accept: "...)
	buf = append(buf, internal.ComputeAcceptKey(websocketKey)...)
	buf = append(buf, "\r\n"...)
	for k, _ := range c.Header {
		buf = append(buf, k...)
		buf = append(buf, ": "...)
		buf = append(buf, c.Header.Get(k)...)
		buf = append(buf, "\r\n"...)
	}
	buf = append(buf, "\r\n"...)
	_, err := conn.Write(buf)
	return err
}

// http protocol upgrade to websocket
func (c *Upgrader) Upgrade(ctx context.Context, w http.ResponseWriter, r *http.Request) (*Conn, error) {
	c.initialize()

	var request = &Request{Request: r, Storage: internal.NewMap()}
	if c.Header == nil {
		c.Header = http.Header{}
	}

	var compressEnabled = false
	if r.Method != http.MethodGet {
		return nil, errors.New("http method must be get")
	}
	if version := r.Header.Get(internal.SecWebSocketVersion); version != internal.SecWebSocketVersion_Value {
		msg := "websocket protocol not supported: " + version
		return nil, errors.New(msg)
	}
	if val := r.Header.Get(internal.Connection); strings.ToLower(val) != strings.ToLower(internal.Connection_Value) {
		return nil, ErrHandshake
	}
	if val := r.Header.Get(internal.Upgrade); strings.ToLower(val) != internal.Upgrade_Value {
		return nil, ErrHandshake
	}
	if val := r.Header.Get(internal.SecWebSocketExtensions); strings.Contains(val, "permessage-deflate") && c.CompressEnabled {
		c.Header.Set(internal.SecWebSocketExtensions, "permessage-deflate; server_no_context_takeover; client_no_context_takeover")
		compressEnabled = true
	}

	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, CloseInternalServerErr
	}
	netConn, brw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}
	if !c.CheckOrigin(request) {
		return nil, ErrCheckOrigin
	}

	// handshake with timeout control
	if err := netConn.SetDeadline(time.Now().Add(c.HandshakeTimeout)); err != nil {
		return nil, err
	}
	var websocketKey = r.Header.Get(internal.SecWebSocketKey)
	if err := c.handshake(netConn, websocketKey); err != nil {
		return nil, err
	}
	if err := netConn.SetDeadline(time.Time{}); err != nil {
		return nil, err
	}
	if err := netConn.SetReadDeadline(time.Time{}); err != nil {
		return nil, err
	}
	if err := netConn.SetWriteDeadline(time.Time{}); err != nil {
		return nil, err
	}
	if err := netConn.(*net.TCPConn).SetNoDelay(false); err != nil {
		return nil, err
	}
	return serveWebSocket(ctx, c, request, netConn, brw, compressEnabled), nil
}
