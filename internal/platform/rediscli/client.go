// Package rediscli is a small, dependency-free Redis client implementing
// just enough of the RESP2 protocol for what Racetify's Phase 0 needs:
// rate-limit counters, refresh/access token blacklisting, OAuth state
// nonces, and short-lived one-time-token caching.
//
// Why hand-rolled: this codebase was built inside a sandboxed environment
// whose egress allowlist covers github.com (fetchable via `git`/GOPROXY=
// direct) but not golang.org/x/* or go.uber.org/*. go-redis/v9's
// dependency graph needs both (golang.org/x/sys/cpu, go.uber.org/atomic),
// so it could not be fetched. RESP2 is a deliberately simple protocol
// (https://redis.io/docs/reference/protocol-spec/) and the surface area we
// need is six commands, so implementing it directly was safer than
// vendoring a partial/unaudited copy of a third-party client. If network
// policy later allows it, this package's public API was kept intentionally
// narrow so swapping in go-redis is a localized change.
package rediscli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

// ErrNil mirrors redis.Nil: returned when a key does not exist.
var ErrNil = errors.New("rediscli: nil")

type Config struct {
	Addr         string
	Password     string
	DB           int
	DialTimeout  time.Duration
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	PoolSize     int
}

// Client is a minimal connection-pooled RESP2 client. It is safe for
// concurrent use.
type Client struct {
	cfg  Config
	pool chan *conn
}

type conn struct {
	nc net.Conn
	rw *bufio.ReadWriter
}

func New(cfg Config) (*Client, error) {
	if cfg.DialTimeout == 0 {
		cfg.DialTimeout = 5 * time.Second
	}
	if cfg.ReadTimeout == 0 {
		cfg.ReadTimeout = 3 * time.Second
	}
	if cfg.WriteTimeout == 0 {
		cfg.WriteTimeout = 3 * time.Second
	}
	if cfg.PoolSize == 0 {
		cfg.PoolSize = 10
	}

	c := &Client{
		cfg:  cfg,
		pool: make(chan *conn, cfg.PoolSize),
	}

	// Fail fast if Redis is unreachable at boot rather than on first use.
	conn, err := c.dial()
	if err != nil {
		return nil, err
	}
	c.release(conn)
	return c, nil
}

func (c *Client) dial() (*conn, error) {
	nc, err := net.DialTimeout("tcp", c.cfg.Addr, c.cfg.DialTimeout)
	if err != nil {
		return nil, fmt.Errorf("rediscli: dial: %w", err)
	}
	cn := &conn{
		nc: nc,
		rw: bufio.NewReadWriter(bufio.NewReader(nc), bufio.NewWriter(nc)),
	}

	if c.cfg.Password != "" {
		if _, err := cn.do("AUTH", c.cfg.Password); err != nil {
			nc.Close()
			return nil, fmt.Errorf("rediscli: auth: %w", err)
		}
	}
	if c.cfg.DB != 0 {
		if _, err := cn.do("SELECT", strconv.Itoa(c.cfg.DB)); err != nil {
			nc.Close()
			return nil, fmt.Errorf("rediscli: select db: %w", err)
		}
	}
	return cn, nil
}

func (c *Client) acquire() (*conn, error) {
	select {
	case cn := <-c.pool:
		return cn, nil
	default:
		return c.dial()
	}
}

func (c *Client) release(cn *conn) {
	select {
	case c.pool <- cn:
	default:
		cn.nc.Close()
	}
}

func (c *Client) discard(cn *conn) {
	cn.nc.Close()
}

func (c *Client) exec(ctx context.Context, args ...string) (reply, error) {
	cn, err := c.acquire()
	if err != nil {
		return reply{}, err
	}

	deadline := time.Now().Add(c.cfg.WriteTimeout + c.cfg.ReadTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	cn.nc.SetDeadline(deadline)

	r, err := cn.do(args...)
	if err != nil {
		c.discard(cn)
		return reply{}, err
	}
	c.release(cn)
	return r, nil
}

// Close drains and closes every pooled connection.
func (c *Client) Close() error {
	for {
		select {
		case cn := <-c.pool:
			cn.nc.Close()
		default:
			return nil
		}
	}
}

// ---- high level commands ----

func (c *Client) Ping(ctx context.Context) error {
	_, err := c.exec(ctx, "PING")
	return err
}

func (c *Client) Set(ctx context.Context, key, value string, ttl time.Duration) error {
	args := []string{"SET", key, value}
	if ttl > 0 {
		args = append(args, "PX", strconv.FormatInt(ttl.Milliseconds(), 10))
	}
	_, err := c.exec(ctx, args...)
	return err
}

// SetNX sets key only if it does not already exist (used for OAuth
// state/nonce anti-replay and idempotent invitation token creation). It
// returns true if the key was set.
func (c *Client) SetNX(ctx context.Context, key, value string, ttl time.Duration) (bool, error) {
	args := []string{"SET", key, value, "NX"}
	if ttl > 0 {
		args = append(args, "PX", strconv.FormatInt(ttl.Milliseconds(), 10))
	}
	r, err := c.exec(ctx, args...)
	if err != nil {
		return false, err
	}
	return !r.isNil, nil
}

func (c *Client) Get(ctx context.Context, key string) (string, error) {
	r, err := c.exec(ctx, "GET", key)
	if err != nil {
		return "", err
	}
	if r.isNil {
		return "", ErrNil
	}
	return r.str, nil
}

func (c *Client) Del(ctx context.Context, keys ...string) error {
	args := append([]string{"DEL"}, keys...)
	_, err := c.exec(ctx, args...)
	return err
}

func (c *Client) Exists(ctx context.Context, key string) (bool, error) {
	r, err := c.exec(ctx, "EXISTS", key)
	if err != nil {
		return false, err
	}
	return r.integer == 1, nil
}

func (c *Client) Expire(ctx context.Context, key string, ttl time.Duration) error {
	_, err := c.exec(ctx, "PEXPIRE", key, strconv.FormatInt(ttl.Milliseconds(), 10))
	return err
}

// Incr increments key and returns the new value. Used for fixed-window
// rate-limit counters; pair with Expire on first increment.
func (c *Client) Incr(ctx context.Context, key string) (int64, error) {
	r, err := c.exec(ctx, "INCR", key)
	if err != nil {
		return 0, err
	}
	return r.integer, nil
}

// LPush pushes value onto the head of a Redis list - added for Phase 1's
// internal/jobqueue.Queue.Enqueue, which pushes a newly created job id for
// a worker's BRPop to consume (LPUSH+BRPOP makes a simple FIFO queue).
func (c *Client) LPush(ctx context.Context, key, value string) error {
	_, err := c.exec(ctx, "LPUSH", key, value)
	return err
}

// BRPop blocking-pops from the tail of a Redis list, waiting up to timeout
// before returning ErrNil if nothing arrived - internal/jobqueue.Queue.
// Dequeue's poll loop. Unlike every other command in this client, it sets
// its own connection deadline from timeout (not cfg.ReadTimeout), since a
// blocking pop is expected to legitimately wait far longer than an
// ordinary command; Redis's own BRPOP timeout argument is whole seconds,
// so timeout is rounded up to at least 1s.
func (c *Client) BRPop(ctx context.Context, key string, timeout time.Duration) (string, error) {
	cn, err := c.acquire()
	if err != nil {
		return "", err
	}

	secs := int(timeout.Round(time.Second) / time.Second)
	if secs < 1 {
		secs = 1
	}
	deadline := time.Now().Add(timeout + c.cfg.ReadTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	cn.nc.SetDeadline(deadline)

	r, err := cn.do("BRPOP", key, strconv.Itoa(secs))
	if err != nil {
		c.discard(cn)
		return "", fmt.Errorf("rediscli: brpop: %w", err)
	}
	c.release(cn)

	if r.isNil || len(r.array) < 2 {
		return "", ErrNil
	}
	return r.array[1].str, nil
}

// ---- RESP2 wire protocol ----

type reply struct {
	str     string
	integer int64
	isNil   bool
	array   []reply
}

func (cn *conn) do(args ...string) (reply, error) {
	if err := writeCommand(cn.rw.Writer, args); err != nil {
		return reply{}, fmt.Errorf("rediscli: write: %w", err)
	}
	if err := cn.rw.Writer.Flush(); err != nil {
		return reply{}, fmt.Errorf("rediscli: flush: %w", err)
	}
	r, err := readReply(cn.rw.Reader)
	if err != nil {
		return reply{}, fmt.Errorf("rediscli: read: %w", err)
	}
	return r, nil
}

// writeCommand encodes args as a RESP array of bulk strings, the standard
// way clients issue commands.
func writeCommand(w *bufio.Writer, args []string) error {
	if _, err := fmt.Fprintf(w, "*%d\r\n", len(args)); err != nil {
		return err
	}
	for _, a := range args {
		if _, err := fmt.Fprintf(w, "$%d\r\n%s\r\n", len(a), a); err != nil {
			return err
		}
	}
	return nil
}

func readReply(r *bufio.Reader) (reply, error) {
	line, err := readLine(r)
	if err != nil {
		return reply{}, err
	}
	if len(line) == 0 {
		return reply{}, errors.New("rediscli: empty reply line")
	}

	switch line[0] {
	case '+': // simple string
		return reply{str: line[1:]}, nil
	case '-': // error
		return reply{}, errors.New("rediscli: server error: " + line[1:])
	case ':': // integer
		n, err := strconv.ParseInt(line[1:], 10, 64)
		if err != nil {
			return reply{}, err
		}
		return reply{integer: n}, nil
	case '$': // bulk string
		n, err := strconv.Atoi(line[1:])
		if err != nil {
			return reply{}, err
		}
		if n == -1 {
			return reply{isNil: true}, nil
		}
		buf := make([]byte, n+2) // +2 for trailing CRLF
		if _, err := readFull(r, buf); err != nil {
			return reply{}, err
		}
		return reply{str: string(buf[:n])}, nil
	case '*': // array
		n, err := strconv.Atoi(line[1:])
		if err != nil {
			return reply{}, err
		}
		if n == -1 {
			return reply{isNil: true}, nil
		}
		arr := make([]reply, n)
		for i := 0; i < n; i++ {
			el, err := readReply(r)
			if err != nil {
				return reply{}, err
			}
			arr[i] = el
		}
		return reply{array: arr}, nil
	default:
		return reply{}, fmt.Errorf("rediscli: unknown reply type %q", line[0])
	}
}

func readLine(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func readFull(r *bufio.Reader, buf []byte) (int, error) {
	n := 0
	for n < len(buf) {
		m, err := r.Read(buf[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}
