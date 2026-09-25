package httpx

// Минимальный HTTP/1.1-клиент — для консоли webdb-shell.
//
// Зачем он собственный, а не net/http: net/http вместе с TLS-стеком весит
// несколько мегабайт в бинаре. Консоль — инструмент разработчика, и тащить в
// неё весь TLS ради одного http://-соединения незачем. Сервер tinydb TLS не
// говорит вообще, поэтому здесь только незашифрованный http:// и один адрес
// вида host:port — без URL-парсинга, редиректов, пула соединений и прокси.
//
// Ограничения намеренные и зафиксированы: один адрес, один запрос за
// соединение (Connection: close), без TLS, ответ читается потоком с лимитом.

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

// DefaultClientTimeout — таймаут одного запроса.
const DefaultClientTimeout = 30 * time.Second

// MaxResponseBytes — потолок тела ответа (защита от бесконечного чтения).
const MaxResponseBytes = 256 << 20

// ClientConfig — настройки клиента.
type ClientConfig struct {
	// Addr — host:port (только IP или localhost).
	Addr string
	// Token — если задан, добавляется заголовок Authorization: Bearer.
	Token string
	// UserAgent — значение заголовка User-Agent.
	UserAgent string
	// Timeout — общий таймаут запроса; 0 = DefaultClientTimeout.
	Timeout time.Duration
	// MaxBody — лимит тела ответа; 0 = MaxResponseBytes.
	MaxBody int64
}

// Client — минимальный HTTP/1.1-клиент.
type Client struct {
	cfg ClientConfig
}

// NewClient создаёт клиента.
func NewClient(cfg ClientConfig) *Client {
	if cfg.UserAgent == "" {
		cfg.UserAgent = "tinydb-shell/0.2"
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = DefaultClientTimeout
	}
	if cfg.MaxBody == 0 {
		cfg.MaxBody = MaxResponseBytes
	}
	return &Client{cfg: cfg}
}

// ClientResponse — ответ сервера. Body нужно закрыть (Close).
type ClientResponse struct {
	Status int
	Header Header
	Body   io.ReadCloser
}

// Close закрывает тело (и соединение).
func (r *ClientResponse) Close() error {
	if r.Body != nil {
		return r.Body.Close()
	}
	return nil
}

// ErrAuth — 401 от сервера (выделен отдельно, чтобы вызывающий показал
// понятную подсказку про токен).
var ErrAuth = errors.New("unauthorized")

// Do выполняет один запрос. body может быть nil. Возвращает ответ с открытым
// телом: вызывающий читает и закрывает.
func (c *Client) Do(method, path string, body []byte) (*ClientResponse, error) {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	deadline := time.Now().Add(c.cfg.Timeout)
	conn, err := net.DialTimeout("tcp", c.cfg.Addr, c.cfg.Timeout)
	if err != nil {
		return nil, err
	}
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetDeadline(deadline)
	}

	var sb strings.Builder
	sb.Grow(len(path) + 128 + len(body))
	sb.WriteString(method)
	sb.WriteByte(' ')
	sb.WriteString(path)
	sb.WriteString(" HTTP/1.1\r\nHost: ")
	sb.WriteString(c.cfg.Addr)
	sb.WriteString("\r\nUser-Agent: ")
	sb.WriteString(c.cfg.UserAgent)
	sb.WriteString("\r\nConnection: close\r\n")
	if c.cfg.Token != "" {
		sb.WriteString("Authorization: Bearer ")
		sb.WriteString(c.cfg.Token)
		sb.WriteString("\r\n")
	}
	if body != nil {
		sb.WriteString("Content-Type: application/json\r\nContent-Length: ")
		sb.WriteString(strconv.Itoa(len(body)))
		sb.WriteString("\r\n")
	}
	sb.WriteString("\r\n")
	if _, err := io.WriteString(conn, sb.String()); err != nil {
		conn.Close()
		return nil, err
	}
	if len(body) > 0 {
		if _, err := conn.Write(body); err != nil {
			conn.Close()
			return nil, err
		}
	}

	// Статусная строка + заголовки.
	br := bufio.NewReader(conn)
	line, err := readLine(br)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("читаю ответ: %w", err)
	}
	// HTTP/1.1 200 OK
	var proto string
	status := 0
	fmt.Sscanf(line, "%s %d", &proto, &status)
	if status == 0 {
		conn.Close()
		return nil, fmt.Errorf("плохой ответ сервера: %q", line)
	}
	var h Header
	for {
		l, err := readLine(br)
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("читаю заголовки: %w", err)
		}
		if l == "" {
			break
		}
		if i := strings.IndexByte(l, ':'); i > 0 {
			name := strings.TrimSpace(l[:i])
			val := strings.TrimSpace(l[i+1:])
			h.Add(name, val)
		}
	}

	resp := &ClientResponse{Status: status, Header: h}
	if status == 204 || status == 304 {
		resp.Body = io.NopCloser(strings.NewReader(""))
		return resp, nil
	}
	if status == 401 {
		// Тело всё равно читаем до конца, но ошибку отдаём сразу.
		io.Copy(io.Discard, io.LimitReader(conn, 4096))
		conn.Close()
		resp.Body = io.NopCloser(strings.NewReader(""))
		return resp, ErrAuth
	}

	// Тело читаем из того же bufio.Reader: часть тела могла уже попасть в его
	// буфер при разборе заголовков (читать из conn здесь означало бы потерять
	// эти байты).
	limit := c.cfg.MaxBody
	if cl := h.Get("Content-Length"); cl != "" {
		if n, err := strconv.ParseInt(cl, 10, 64); err == nil {
			if n > limit {
				conn.Close()
				return nil, fmt.Errorf("ответ больше лимита (%d > %d)", n, limit)
			}
			resp.Body = &boundedBody{r: br, n: n, conn: conn, left: limit}
			return resp, nil
		}
	}
	if strings.EqualFold(h.Get("Transfer-Encoding"), "chunked") {
		resp.Body = &chunkedBody{r: br, conn: conn, left: limit}
		return resp, nil
	}
	resp.Body = &boundedBody{r: br, n: -1, conn: conn, left: limit}
	return resp, nil
}

// boundedBody — тело фиксированной (n ≥ 0) или неограниченной (-1) длины с
// общим лимитом left.
type boundedBody struct {
	r    io.Reader
	n    int64
	read int64
	left int64
	conn net.Conn
}

func (b *boundedBody) Read(p []byte) (int, error) {
	if b.n >= 0 {
		if b.n-b.read == 0 {
			return 0, io.EOF
		}
		if int64(len(p)) > b.n-b.read {
			p = p[:b.n-b.read]
		}
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.r.Read(p)
	b.read += int64(n)
	b.left -= int64(n)
	if b.left <= 0 && b.n < 0 {
		return n, io.EOF
	}
	if b.n >= 0 && b.read >= b.n {
		return n, io.EOF
	}
	return n, err
}

func (b *boundedBody) Close() error { return b.conn.Close() }

// chunkedBody — декодирует chunked-ответ на лету.
type chunkedBody struct {
	r    *bufio.Reader
	conn net.Conn
	left int64
	done bool
}

func (c *chunkedBody) Read(p []byte) (int, error) {
	if c.done {
		return 0, io.EOF
	}
	line, err := readLine(c.r)
	if err != nil {
		return 0, err
	}
	sz := line
	if i := strings.IndexByte(sz, ';'); i >= 0 {
		sz = sz[:i]
	}
	n, err := strconv.ParseInt(strings.TrimSpace(sz), 16, 64)
	if err != nil || n < 0 {
		return 0, errors.New("плохой chunk в ответе")
	}
	if n == 0 {
		// Хвост трейлеров до пустой строки.
		for {
			t, err := readLine(c.r)
			if err != nil || t == "" {
				break
			}
		}
		c.done = true
		return 0, io.EOF
	}
	c.left -= n
	if c.left < 0 {
		return 0, errors.New("ответ больше лимита")
	}
	buf := make([]byte, n)
	if n > int64(len(p)) {
		buf = buf[:len(p)]
	}
	got, err := io.ReadFull(c.r, buf)
	// Съедаем CRLF после данных чанка.
	if _, rerr := readLine(c.r); rerr != nil && err == nil {
		err = rerr
	}
	return got, err
}

func (c *chunkedBody) Close() error { return c.conn.Close() }
