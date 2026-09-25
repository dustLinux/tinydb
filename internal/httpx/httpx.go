// Package httpx is a tiny HTTP/1.1 server core for tinydb.
//
// It exists to keep the binary and RSS small: net/http would pull in
// crypto/tls, x509, http2 and friends (~5 MB of code). httpx implements just
// what tinydb needs:
//
//   - request line + header parsing (bounded, 32 KiB),
//   - Content-Length and chunked request bodies, Expect: 100-continue,
//   - responses framed automatically: buffered + Content-Length when small,
//     chunked when large (streaming), raw length for explicit Content-Length,
//   - keep-alive, idle/read/write deadlines,
//   - {param} routing with 404/405 distinction,
//   - graceful shutdown.
//
// No TLS (terminate upstream if needed), no HTTP/2, no trailers.
package httpx

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Handler processes one request.
type Handler func(w *Response, r *Request)

// Middleware wraps a handler.
type Middleware func(Handler) Handler

// Status codes used by tinydb.
const (
	StatusOK                    = 200
	StatusCreated               = 201
	StatusNoContent             = 204
	StatusBadRequest            = 400
	StatusUnauthorized          = 401
	StatusForbidden             = 403
	StatusNotFound              = 404
	StatusMethodNotAllowed      = 405
	StatusConflict              = 409
	StatusRequestEntityTooLarge = 413
	StatusInsufficientStorage   = 507
	StatusInternalServerError   = 500
	StatusServiceUnavailable    = 503
)

var statusText = map[int]string{
	200: "OK",
	201: "Created",
	204: "No Content",
	400: "Bad Request",
	401: "Unauthorized",
	403: "Forbidden",
	404: "Not Found",
	405: "Method Not Allowed",
	409: "Conflict",
	413: "Content Too Large",
	414: "URI Too Long",
	431: "Request Header Fields Too Large",
	500: "Internal Server Error",
	503: "Service Unavailable",
	507: "Insufficient Storage",
}

// StatusText returns the reason phrase for a status code.
func StatusText(code int) string {
	if s, ok := statusText[code]; ok {
		return s
	}
	return "Status"
}

// ---- headers ----

// Header is a case-insensitive header map preserving insertion order.
type Header struct {
	keys []string
	vals []string
}

// Get returns the first value for key (case-insensitive).
func (h *Header) Get(key string) string {
	for i, k := range h.keys {
		if strings.EqualFold(k, key) {
			return h.vals[i]
		}
	}
	return ""
}

// Set replaces the value for key.
func (h *Header) Set(key, value string) {
	for i, k := range h.keys {
		if strings.EqualFold(k, key) {
			h.vals[i] = value
			return
		}
	}
	h.keys = append(h.keys, key)
	h.vals = append(h.vals, value)
}

// Add appends a value for key.
func (h *Header) Add(key, value string) {
	h.keys = append(h.keys, key)
	h.vals = append(h.vals, value)
}

// Del removes a key.
func (h *Header) Del(key string) {
	for i, k := range h.keys {
		if strings.EqualFold(k, key) {
			h.keys = append(h.keys[:i], h.keys[i+1:]...)
			h.vals = append(h.vals[:i], h.vals[i+1:]...)
			return
		}
	}
}

func (h *Header) writeTo(b *strings.Builder) {
	for i, k := range h.keys {
		b.WriteString(k)
		b.WriteString(": ")
		b.WriteString(h.vals[i])
		b.WriteString("\r\n")
	}
}

// ---- request ----

// Request is a parsed HTTP request.
type Request struct {
	Method     string
	Path       string // path without query
	RawQuery   string
	Proto      string
	Header     *Header
	Body       io.ReadCloser
	RemoteAddr string

	pathParams map[string]string
	query      Values
	queryOK    bool
	lc         *limitedConn   // Content-Length body (nil if none)
	chunked    *chunkedReader // chunked body (nil if none)
}

// remaining returns unread body bytes: >=0 exact count, -1 = unknown
// (chunked, not finished).
func (r *Request) remaining() int64 {
	if r.lc != nil {
		return r.lc.left
	}
	if r.chunked != nil {
		if r.chunked.done {
			return 0
		}
		return -1
	}
	return 0
}

// PathValue returns a routed {param} value.
func (r *Request) PathValue(name string) string {
	if r.pathParams == nil {
		return ""
	}
	return r.pathParams[name]
}

// Query parses (lazily) the URL query string.
func (r *Request) Query() Values {
	if !r.queryOK {
		r.query = parseQuery(r.RawQuery)
		r.queryOK = true
	}
	return r.query
}

// TooLargeError is returned when a body exceeds the allowed limit.
type TooLargeError struct{ Limit int64 }

func (e *TooLargeError) Error() string {
	return fmt.Sprintf("request body exceeds %d bytes", e.Limit)
}

// ReadBody reads the full request body, enforcing limit.
func ReadBody(r *Request, limit int64) ([]byte, error) {
	if r.Body == nil {
		return []byte{}, nil
	}
	if left := r.remaining(); left > limit {
		return nil, &TooLargeError{Limit: limit}
	}
	var buf bytes.Buffer
	n, err := io.Copy(&buf, io.LimitReader(r.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if n > limit {
		return nil, &TooLargeError{Limit: limit}
	}
	return buf.Bytes(), nil
}

// ---- response ----

const bufferLimit = 64 << 10 // switch to streaming above this

type framing int

const (
	frameBuffered framing = iota // headers not sent yet
	frameChunked                 // Transfer-Encoding: chunked
	frameRaw                     // explicit Content-Length
	frameClose                   // no length, close at end (HTTP/1.0 clients)
)

// Response accumulates and streams an HTTP response.
type Response struct {
	hdr        Header
	status     int
	buf        bytes.Buffer
	frame      framing
	explicitCL int64 // -1 = not set by handler
	written    int64
	wErr       error

	w          *bufio.Writer
	conn       Conn
	keepAlive  bool
	proto11    bool
	hdrWritten bool
	bytesHdr   int64
}

func newResponse(conn Conn, w *bufio.Writer, req *Request) *Response {
	proto11 := req.Proto == "HTTP/1.1"
	connHdr := req.Header.Get("Connection")
	keepAlive := !strings.EqualFold(connHdr, "close")
	if !proto11 {
		keepAlive = strings.EqualFold(connHdr, "keep-alive")
	}
	return &Response{
		status:     StatusOK,
		explicitCL: -1,
		w:          w,
		conn:       conn,
		keepAlive:  keepAlive,
		proto11:    proto11,
	}
}

// Header returns the response headers (valid until the first Write/Flush).
func (r *Response) Header() *Header { return &r.hdr }

// Status returns the status code written (or to be written).
func (r *Response) Status() int { return r.status }

// Written returns total body bytes written.
func (r *Response) Written() int64 { return r.written }

// CloseConnection marks the connection to close after this response.
func (r *Response) CloseConnection() { r.keepAlive = false }

// HdrWritten reports whether the response headers were already sent.
func (r *Response) HdrWritten() bool { return r.hdrWritten }

func (r *Response) WriteHeader(status int) {
	if !r.hdrWritten && r.status == StatusOK {
		r.status = status
	}
}

// SetContentLength declares an exact body length (enables raw framing).
func (r *Response) SetContentLength(n int64) {
	r.explicitCL = n
	r.hdr.Set("Content-Length", strconv.FormatInt(n, 10))
}

// Write buffers small bodies and streams large ones (chunked).
func (r *Response) Write(p []byte) (int, error) {
	if r.wErr != nil {
		return 0, r.wErr
	}
	if r.frame == frameBuffered && !r.hdrWritten {
		if r.explicitCL >= 0 {
			if r.explicitCL > bufferLimit {
				// Too big to buffer: stream with the declared length.
				if err := r.flushHeader(frameRaw); err != nil {
					return 0, err
				}
			} else {
				if int64(r.buf.Len()+len(p)) > r.explicitCL {
					return 0, errors.New("httpx: wrote more than declared Content-Length")
				}
				r.buf.Write(p)
				r.written += int64(len(p))
				return len(p), nil
			}
		} else if r.buf.Len()+len(p) <= bufferLimit {
			r.buf.Write(p)
			r.written += int64(len(p))
			return len(p), nil
		} else {
			// Overflow: flush the already-buffered prefix first (it must not
			// be dropped!), then switch framing and continue streaming.
			f := frameChunked
			if !r.proto11 {
				f = frameClose // HTTP/1.0: length unknown -> close-delimited
			}
			if err := r.flushHeader(f); err != nil {
				return 0, err
			}
			if r.buf.Len() > 0 {
				n := r.buf.Len()
				var err error
				if f == frameChunked {
					_, err = r.writeChunk(r.buf.Bytes())
				} else {
					_, err = r.writeDirect(r.buf.Bytes())
				}
				r.buf.Reset()
				r.written -= int64(n) // already counted while buffering
				if err != nil {
					return 0, err
				}
			}
		}
	}
	switch r.frame {
	case frameChunked:
		n, err := r.writeChunk(p)
		r.written += int64(n)
		return n, err
	case frameRaw, frameClose:
		n, err := r.writeDirect(p)
		r.written += int64(n)
		return n, err
	default:
		// header already flushed earlier in another mode
		n, err := r.writeDirect(p)
		r.written += int64(n)
		return n, err
	}
}

func (r *Response) writeDirect(p []byte) (int, error) {
	if err := r.conn.SetWriteDeadline(time.Now().Add(60 * time.Second)); err != nil {
		return 0, err
	}
	n, err := r.w.Write(p)
	if err == nil && r.explicitCL >= 0 && r.written+int64(n) > r.explicitCL {
		return n, errors.New("httpx: exceeded declared Content-Length")
	}
	return n, err
}

func (r *Response) writeChunk(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	var head [32]byte
	h := appendstrconv(head[:0], len(p))
	// chunk framing: <size-hex>\r\n<data>\r\n
	if _, err := r.w.Write(h); err != nil {
		return 0, err
	}
	if _, err := r.w.WriteString("\r\n"); err != nil {
		return 0, err
	}
	n, err := r.w.Write(p)
	if _, err2 := r.w.WriteString("\r\n"); err == nil {
		err = err2
	}
	return n, err
}

func appendstrconv(b []byte, n int) []byte {
	return strconv.AppendInt(b, int64(n), 16)
}

// JSON writes a JSON response with the given status.
func (r *Response) JSON(status int, v any) error {
	r.status = status
	r.hdr.Set("Content-Type", "application/json; charset=utf-8")
	return json.NewEncoder(r).Encode(v)
}

// Fail writes a JSON error object {"error": ..., "status": ...}.
func (r *Response) Fail(status int, msg string) {
	_ = r.JSON(status, map[string]any{"error": msg, "status": status})
}

func (r *Response) flushHeader(f framing) error {
	r.frame = f
	r.hdrWritten = true
	var b strings.Builder
	fmt.Fprintf(&b, "HTTP/1.1 %d %s\r\n", r.status, StatusText(r.status))
	if r.explicitCL >= 0 {
		if r.hdr.Get("Content-Length") == "" {
			r.hdr.Set("Content-Length", strconv.FormatInt(r.explicitCL, 10))
		}
	}
	switch f {
	case frameChunked:
		r.hdr.Set("Transfer-Encoding", "chunked")
	case frameClose:
		r.keepAlive = false
	}
	if !r.keepAlive {
		r.hdr.Set("Connection", "close")
	} else {
		r.hdr.Del("Connection")
	}
	r.hdr.Set("Server", "webdb")
	r.hdr.writeTo(&b)
	b.WriteString("\r\n")
	if err := r.conn.SetWriteDeadline(time.Now().Add(60 * time.Second)); err != nil {
		return err
	}
	_, err := io.WriteString(r.w, b.String())
	r.bytesHdr = int64(b.Len())
	return err
}

// finish flushes buffered data and terminates the framing.
func (r *Response) finish() error {
	if r.wErr != nil {
		return r.wErr
	}
	if !r.hdrWritten {
		if r.explicitCL >= 0 && int64(r.buf.Len()) != r.explicitCL {
			r.wErr = fmt.Errorf("httpx: body %d != declared Content-Length %d", r.buf.Len(), r.explicitCL)
			return r.wErr
		}
		if r.explicitCL < 0 {
			r.hdr.Set("Content-Length", strconv.Itoa(r.buf.Len()))
		}
		if err := r.flushHeader(frameBuffered); err != nil {
			r.wErr = err
			return err
		}
		if _, err := r.w.Write(r.buf.Bytes()); err != nil {
			r.wErr = err
			return err
		}
		r.written = int64(r.buf.Len())
	} else {
		switch r.frame {
		case frameChunked:
			if _, err := r.w.WriteString("0\r\n\r\n"); err != nil {
				r.wErr = err
				return err
			}
		}
	}
	r.wErr = r.w.Flush()
	return r.wErr
}

// ---- mux ----

type route struct {
	method   string
	segments []string // literal or "{name}"
	handler  Handler
}

// Mux routes method+pattern (e.g. "GET /v1/collections/{coll}/docs/{id}").
type Mux struct {
	routes []route
}

// NewMux creates an empty router.
func NewMux() *Mux { return &Mux{} }

// HandleFunc registers a handler for a "METHOD /path/{param}" pattern.
func (m *Mux) HandleFunc(pattern string, h Handler) {
	method, path, ok := strings.Cut(pattern, " ")
	if !ok {
		panic("httpx: pattern must be \"METHOD /path\": " + pattern)
	}
	m.routes = append(m.routes, route{
		method:   method,
		segments: splitPath(path),
		handler:  h,
	})
}

// Handle wraps middleware around the whole mux.
func (m *Mux) Handler() Handler {
	return func(w *Response, r *Request) {
		m.dispatch(w, r)
	}
}

func splitPath(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

func (m *Mux) dispatch(w *Response, r *Request) {
	segs := splitPath(r.Path)
	pathMatched := false
	for i := range m.routes {
		rt := &m.routes[i]
		params, ok := match(rt.segments, segs)
		if !ok {
			continue
		}
		pathMatched = true
		if rt.method != r.Method {
			continue
		}
		if r.pathParams == nil {
			r.pathParams = map[string]string{}
		}
		for k, v := range params {
			r.pathParams[k] = v
		}
		rt.handler(w, r)
		return
	}
	if pathMatched {
		// Collect Allow header from matching paths.
		var allow []string
		for i := range m.routes {
			if _, ok := match(m.routes[i].segments, segs); ok {
				allow = append(allow, m.routes[i].method)
			}
		}
		w.Header().Set("Allow", strings.Join(allow, ", "))
		w.Fail(StatusMethodNotAllowed, "method not allowed")
		return
	}
	w.Fail(StatusNotFound, "not found")
}

func match(pattern, actual []string) (map[string]string, bool) {
	if len(pattern) != len(actual) {
		return nil, false
	}
	var params map[string]string
	for i, p := range pattern {
		if strings.HasPrefix(p, "{") && strings.HasSuffix(p, "}") {
			if actual[i] == "" {
				return nil, false
			}
			if params == nil {
				params = map[string]string{}
			}
			name := p[1 : len(p)-1]
			val, err := pathUnescape(actual[i])
			if err != nil {
				val = actual[i]
			}
			params[name] = val
			continue
		}
		if p != actual[i] {
			return nil, false
		}
	}
	return params, true
}

// ---- middleware helpers ----

// Recover converts panics into 500 responses.
func Recover(logf func(string, ...any)) Middleware {
	return func(next Handler) Handler {
		return func(w *Response, r *Request) {
			defer func() {
				if rec := recover(); rec != nil {
					if logf != nil {
						logf("panic: %v (path %s)", rec, r.Path)
					}
					if !w.hdrWritten {
						w.Fail(StatusInternalServerError, "internal error")
					} else {
						w.CloseConnection()
					}
				}
			}()
			next(w, r)
		}
	}
}

// ---- request parsing ----

const (
	maxHeaderBytes = 32 << 10
	maxLineBytes   = 8 << 10
)

func readLine(br *bufio.Reader) (string, error) {
	var sb strings.Builder
	for {
		frag, err := br.ReadSlice('\n')
		if len(frag) > 0 {
			if sb.Len()+len(frag) > maxLineBytes {
				return "", errLineTooLong
			}
			sb.Write(frag)
		}
		if err == nil {
			break
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		return "", err
	}
	return strings.TrimRight(sb.String(), "\r\n"), nil
}

var (
	errLineTooLong = errors.New("line too long")
	errTooManyHdrs = errors.New("too many headers")
)

func readRequest(br *bufio.Reader) (*Request, error) {
	line, err := readLine(br)
	if err != nil {
		return nil, err
	}
	if line == "" {
		// tolerate keep-alive CRLF keepalives
		line, err = readLine(br)
		if err != nil || line == "" {
			return nil, err
		}
	}
	parts := strings.SplitN(line, " ", 3)
	if len(parts) != 3 || !strings.HasPrefix(parts[2], "HTTP/1.") {
		return nil, errors.New("malformed request line")
	}
	target := parts[1]
	path, rawQuery, _ := strings.Cut(target, "?")
	if len(path) > maxLineBytes || len(rawQuery) > maxLineBytes {
		return nil, errLineTooLong
	}

	req := &Request{
		Method:   parts[0],
		Path:     path,
		RawQuery: rawQuery,
		Proto:    parts[2],
		Header:   &Header{},
	}
	total := len(line)
	nlines := 0
	for {
		h, err := readLine(br)
		if err != nil {
			return nil, err
		}
		total += len(h)
		if total > maxHeaderBytes {
			return nil, errTooManyHdrs
		}
		if h == "" {
			break
		}
		nlines++
		if nlines > 100 {
			return nil, errTooManyHdrs
		}
		k, v, ok := strings.Cut(h, ":")
		if !ok {
			return nil, errors.New("malformed header")
		}
		req.Header.Add(strings.TrimSpace(k), strings.TrimSpace(v))
	}
	return req, nil
}

// prepareBody sets req.Body from Content-Length / Transfer-Encoding.
// absoluteLimit rejects absurd lengths outright.
func prepareBody(req *Request, br *bufio.Reader, absoluteLimit int64) error {
	te := req.Header.Get("Transfer-Encoding")
	cl := req.Header.Get("Content-Length")
	if te != "" {
		if !strings.EqualFold(strings.TrimSpace(te), "chunked") {
			return errors.New("unsupported Transfer-Encoding")
		}
		if cl != "" {
			return errors.New("both Content-Length and Transfer-Encoding") // smuggling guard
		}
		req.Body = &chunkedReader{r: br}
		req.chunked = req.Body.(*chunkedReader)
		return nil
	}
	if cl == "" {
		req.Body = io.NopCloser(strings.NewReader(""))
		return nil
	}
	n, err := strconv.ParseInt(strings.TrimSpace(cl), 10, 64)
	if err != nil || n < 0 {
		return errors.New("bad Content-Length")
	}
	if n > absoluteLimit {
		return &TooLargeError{Limit: absoluteLimit}
	}
	lc := &limitedConn{r: br, left: n}
	req.Body = io.NopCloser(lc)
	req.lc = lc
	return nil
}

type limitedConn struct {
	r    io.Reader
	left int64
}

func (l *limitedConn) Read(p []byte) (int, error) {
	if l.left <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > l.left {
		p = p[:l.left]
	}
	n, err := l.r.Read(p)
	l.left -= int64(n)
	return n, err
}

// chunkedReader decodes a chunked request body.
type chunkedReader struct {
	r         *bufio.Reader
	remaining int64
	done      bool
	err       error
}

func (c *chunkedReader) Read(p []byte) (int, error) {
	if c.err != nil {
		return 0, c.err
	}
	if c.done {
		return 0, io.EOF
	}
	if c.remaining == 0 {
		line, err := readLine(c.r)
		if err != nil {
			c.err = err
			return 0, err
		}
		sz := line
		if i := strings.IndexByte(sz, ';'); i >= 0 {
			sz = sz[:i]
		}
		n, err := strconv.ParseInt(strings.TrimSpace(sz), 16, 64)
		if err != nil || n < 0 {
			c.err = errors.New("bad chunk size")
			return 0, c.err
		}
		if n == 0 {
			c.done = true
			// consume trailer section until blank line
			for {
				t, err := readLine(c.r)
				if err != nil {
					c.err = err
					return 0, err
				}
				if t == "" {
					break
				}
			}
			return 0, io.EOF
		}
		c.remaining = n
	}
	if int64(len(p)) > c.remaining {
		p = p[:c.remaining]
	}
	n, err := c.r.Read(p)
	c.remaining -= int64(n)
	if c.remaining == 0 && err == nil {
		// consume trailing CRLF
		if _, e := io.ReadFull(c.r, make([]byte, 2)); e != nil {
			c.err = e
			return n, e
		}
	}
	return n, err
}

func (c *chunkedReader) Close() error { return nil }

// ---- server ----

// ServerConfig tunes the HTTP server.
type ServerConfig struct {
	Addr         string
	Handler      Handler
	ReadHeaderTO time.Duration // default 10s
	IdleTO       time.Duration // default 60s
	AbsoluteBody int64         // hard cap on any request body (default 256 MiB)
	ErrorLog     func(format string, args ...any)
}

// Server is a minimal HTTP/1.1 server.
type Server struct {
	cfg ServerConfig

	mu      sync.Mutex
	ln      *Listener
	conns   map[Conn]struct{}
	closing bool
	wg      sync.WaitGroup
}

// NewServer builds a server.
func NewServer(cfg ServerConfig) *Server {
	if cfg.ReadHeaderTO <= 0 {
		cfg.ReadHeaderTO = 10 * time.Second
	}
	if cfg.IdleTO <= 0 {
		cfg.IdleTO = 60 * time.Second
	}
	if cfg.AbsoluteBody <= 0 {
		cfg.AbsoluteBody = 256 << 20
	}
	return &Server{cfg: cfg, conns: map[Conn]struct{}{}}
}

// ListenAndServe binds and serves until Shutdown.
func (s *Server) ListenAndServe() error {
	ln, err := Listen(s.cfg.Addr)
	if err != nil {
		return err
	}
	return s.Serve(ln)
}

// Addr returns the bound address (after Listen).
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return s.cfg.Addr
	}
	return s.ln.Addr()
}

// Serve accepts connections on ln until Shutdown.
func (s *Server) Serve(ln *Listener) error {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		ln.Close()
		return nil
	}
	s.ln = ln
	s.mu.Unlock()

	for {
		conn, err := ln.Accept()
		if err != nil {
			s.mu.Lock()
			closing := s.closing
			s.mu.Unlock()
			if closing {
				return nil
			}
			if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EINTR) {
				continue
			}
			// A closed listener surfaces as EBADF/EINVAL/EPERM etc.
			if s.closedLn() {
				return nil
			}
			return err
		}
		s.mu.Lock()
		s.conns[conn] = struct{}{}
		s.wg.Add(1)
		s.mu.Unlock()
		go s.serveConn(conn)
	}
}

// Shutdown stops accepting and waits for in-flight requests (or ctx).
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return nil
	}
	s.closing = true
	ln := s.ln
	s.mu.Unlock()
	if ln != nil {
		ln.Close()
	}
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		s.mu.Lock()
		for c := range s.conns {
			c.Close()
		}
		s.mu.Unlock()
		<-done
		return ctx.Err()
	}
}

func (s *Server) closedLn() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ln == nil || s.ln.closed
}

func (s *Server) serveConn(conn Conn) {
	defer func() {
		conn.Close()
		s.mu.Lock()
		delete(s.conns, conn)
		s.wg.Done()
		s.mu.Unlock()
	}()

	br := bufio.NewReaderSize(conn, 16<<10)
	bw := bufio.NewWriterSize(conn, 8<<10)

	for {
		_ = conn.SetReadDeadline(time.Now().Add(s.cfg.IdleTO))
		req, err := readRequest(br)
		if err != nil {
			if isTimeout(err) {
				return // idle timeout
			}
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) {
				return
			}
			if errors.Is(err, errLineTooLong) || errors.Is(err, errTooManyHdrs) {
				writeQuickError(bw, 431, "Request Header Fields Too Large")
			} else {
				writeQuickError(bw, 400, "Bad Request")
			}
			return
		}
		req.RemoteAddr = conn.RemoteAddr()

		err = prepareBody(req, br, s.cfg.AbsoluteBody)
		if err != nil {
			var tl *TooLargeError
			if errors.As(err, &tl) {
				writeQuickError(bw, 413, "Content Too Large")
			} else {
				writeQuickError(bw, 400, "Bad Request")
			}
			return
		}

		// Expect: 100-continue
		if strings.EqualFold(req.Header.Get("Expect"), "100-continue") {
			_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if _, err := bw.WriteString("HTTP/1.1 100 Continue\r\n\r\n"); err == nil {
				bw.Flush()
			}
		}

		// Allow the handler to read the body.
		_ = conn.SetReadDeadline(time.Now().Add(120 * time.Second))

		w := newResponse(conn, bw, req)
		s.cfg.Handler(w, req)
		finishErr := w.finish()

		// Body resynchronisation for keep-alive.
		closeConn := !w.keepAlive || finishErr != nil
		if !closeConn {
			rem := req.remaining()
			if rem > 0 {
				// Handler did not consume the body: drain it if reasonable.
				if rem <= 64<<20 {
					_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
					_, _ = io.CopyN(io.Discard, br, rem)
				} else {
					closeConn = true
				}
			} else if rem < 0 {
				// Chunked body not fully consumed: safest is to close.
				closeConn = true
			}
		}
		if closeConn {
			return
		}
	}
}

func writeQuickError(bw *bufio.Writer, status int, text string) {
	body := `{"error":"` + strings.ToLower(text) + `","status":` + strconv.Itoa(status) + "}\n"
	_, _ = bw.WriteString(
		"HTTP/1.1 " + strconv.Itoa(status) + " " + text + "\r\n" +
			"Content-Type: application/json\r\n" +
			"Content-Length: " + strconv.Itoa(len(body)) + "\r\n" +
			"Connection: close\r\nServer: webdb\r\n\r\n" + body)
	_ = bw.Flush()
}
