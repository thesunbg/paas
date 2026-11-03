// Package remote runs commands on a machine over SSH: host key checks, password and key
// authentication, sudo, credential masking and the preflight checks done before installing
// paas on it. The control plane uses it to add nodes ("Install over SSH"), the paas CLI to
// install a control plane on a fresh server (paas install --ssh).
package remote

import (
	"bytes"
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// DefaultDialTimeout bounds the TCP connect plus the SSH handshake and authentication.
const DefaultDialTimeout = 20 * time.Second

// Runner runs one command on a machine. Output (stdout and stderr) goes to out, which
// must be safe for concurrent writes. A non-zero exit is an *ExitError.
type Runner interface {
	Run(ctx context.Context, cmd string, stdin io.Reader, out io.Writer) error
	Close() error
}

// ExitError is a command that ran and exited non-zero.
type ExitError struct{ Code int }

func (e *ExitError) Error() string { return fmt.Sprintf("exit status %d", e.Code) }

// HostKeyMismatchError is a server whose host key is not the one the user accepted.
// It is returned before any credential is sent.
type HostKeyMismatchError struct {
	Want, Got string
}

func (e *HostKeyMismatchError) Error() string {
	return fmt.Sprintf("host key mismatch: the server presented %s but %s was expected; refusing to connect (possible man-in-the-middle, or the server was reinstalled)", e.Got, e.Want)
}

// Fingerprint is the OpenSSH SHA256 fingerprint of a key ("SHA256:...").
func Fingerprint(k ssh.PublicKey) string { return ssh.FingerprintSHA256(k) }

// NormalizeFingerprint accepts "SHA256:abc", "abc" or "sha256:abc" (with optional
// base64 padding) and returns the canonical "SHA256:abc".
func NormalizeFingerprint(fp string) string {
	fp = strings.TrimSpace(fp)
	if i := strings.IndexByte(fp, ':'); i >= 0 && strings.EqualFold(fp[:i], "sha256") {
		fp = fp[i+1:]
	}
	return "SHA256:" + strings.TrimRight(fp, "=")
}

// FingerprintCallback accepts only a host key with the given SHA256 fingerprint. The
// check runs during key exchange, before authentication, so a mismatch never sees a
// password or a signature.
func FingerprintCallback(want string) ssh.HostKeyCallback {
	want = NormalizeFingerprint(want)
	return func(_ string, _ net.Addr, key ssh.PublicKey) error {
		got := Fingerprint(key)
		if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			return &HostKeyMismatchError{Want: want, Got: got}
		}
		return nil
	}
}

// Addr joins host and port (default 22).
func Addr(host string, port int) string {
	if port <= 0 {
		port = 22
	}
	return net.JoinHostPort(strings.Trim(host, "[]"), strconv.Itoa(port))
}

var errProbed = errors.New("host key probed")

// ProbeHostKey connects to an SSH server only to read its host key: the handshake is
// abandoned right after key exchange, before any authentication.
func ProbeHostKey(ctx context.Context, addr string, timeout time.Duration) (ssh.PublicKey, error) {
	if timeout <= 0 {
		timeout = DefaultDialTimeout
	}
	conn, err := dialTCP(ctx, addr, timeout)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	var key ssh.PublicKey
	cfg := &ssh.ClientConfig{
		User: "paas-hostkey-probe",
		HostKeyCallback: func(_ string, _ net.Addr, k ssh.PublicKey) error {
			key = k
			return errProbed
		},
		Timeout: timeout,
	}
	_, _, _, err = ssh.NewClientConn(conn, addr, cfg)
	if key != nil {
		return key, nil
	}
	if err == nil {
		err = errors.New("the server sent no host key")
	}
	return nil, fmt.Errorf("ssh %s: %w", addr, err)
}

func dialTCP(ctx context.Context, addr string, timeout time.Duration) (net.Conn, error) {
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("connect %s: %w", addr, err)
	}
	_ = conn.SetDeadline(time.Now().Add(timeout))
	return conn, nil
}

// DialConfig is how to reach and authenticate to a server. At least one of Password,
// PrivateKey or Signers is required, and HostKey always is.
type DialConfig struct {
	Addr string
	User string

	Password   string
	PrivateKey []byte // PEM / OpenSSH private key
	Passphrase string // of an encrypted PrivateKey
	Signers    []ssh.Signer

	HostKey ssh.HostKeyCallback
	Timeout time.Duration
}

// ParsePrivateKey parses a private key, decrypting it with passphrase when it is encrypted.
func ParsePrivateKey(key []byte, passphrase string) (ssh.Signer, error) {
	s, err := ssh.ParsePrivateKey(key)
	var missing *ssh.PassphraseMissingError
	if errors.As(err, &missing) {
		if passphrase == "" {
			return nil, errors.New("the private key is encrypted: give its passphrase")
		}
		s, err = ssh.ParsePrivateKeyWithPassphrase(key, []byte(passphrase))
	}
	if err != nil {
		return nil, fmt.Errorf("private key: %w", err)
	}
	return s, nil
}

// Client is an authenticated SSH connection.
type Client struct {
	conn *ssh.Client
}

// Dial connects, verifies the host key with cfg.HostKey and authenticates.
func Dial(ctx context.Context, cfg DialConfig) (*Client, error) {
	if cfg.HostKey == nil {
		return nil, errors.New("no host key verification configured")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultDialTimeout
	}
	var auth []ssh.AuthMethod
	signers := cfg.Signers
	if len(cfg.PrivateKey) > 0 {
		s, err := ParsePrivateKey(cfg.PrivateKey, cfg.Passphrase)
		if err != nil {
			return nil, err
		}
		signers = append([]ssh.Signer{s}, signers...)
	}
	if len(signers) > 0 {
		auth = append(auth, ssh.PublicKeys(signers...))
	}
	if cfg.Password != "" {
		pw := cfg.Password
		auth = append(auth, ssh.Password(pw), ssh.KeyboardInteractive(func(_, _ string, qs []string, _ []bool) ([]string, error) {
			ans := make([]string, len(qs))
			for i := range ans {
				ans[i] = pw
			}
			return ans, nil
		}))
	}
	if len(auth) == 0 {
		return nil, errors.New("no SSH credentials: give a private key, a password or use an SSH agent")
	}
	conn, err := dialTCP(ctx, cfg.Addr, cfg.Timeout)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	c, chans, reqs, err := ssh.NewClientConn(conn, cfg.Addr, &ssh.ClientConfig{
		User:            cfg.User,
		Auth:            auth,
		HostKeyCallback: cfg.HostKey,
		Timeout:         cfg.Timeout,
	})
	if err != nil {
		_ = conn.Close()
		var mm *HostKeyMismatchError
		if errors.As(err, &mm) {
			return nil, mm
		}
		// errors.As cannot see through the handshake's %v wrapping: match the text too.
		if strings.Contains(err.Error(), "host key mismatch") {
			return nil, fmt.Errorf("ssh %s: %w", cfg.Addr, err)
		}
		if strings.Contains(err.Error(), "unable to authenticate") {
			return nil, fmt.Errorf("ssh %s: authentication failed for user %q (wrong key or password?)", cfg.Addr, cfg.User)
		}
		return nil, fmt.Errorf("ssh %s: %w", cfg.Addr, err)
	}
	_ = conn.SetDeadline(time.Time{})
	return &Client{conn: ssh.NewClient(c, chans, reqs)}, nil
}

// Close ends the connection.
func (c *Client) Close() error { return c.conn.Close() }

// Run runs cmd in a new session. When ctx ends first the remote process is killed and
// ctx's error returned.
func (c *Client) Run(ctx context.Context, cmd string, stdin io.Reader, out io.Writer) error {
	sess, err := c.conn.NewSession()
	if err != nil {
		return err
	}
	defer sess.Close()
	sess.Stdout = out
	sess.Stderr = out
	if stdin != nil {
		sess.Stdin = stdin
	}
	if err := sess.Start(cmd); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- sess.Wait() }()
	select {
	case err = <-done:
	case <-ctx.Done():
		_ = sess.Signal(ssh.SIGKILL)
		_ = sess.Close()
		return ctx.Err()
	}
	var ee *ssh.ExitError
	if errors.As(err, &ee) {
		return &ExitError{Code: ee.ExitStatus()}
	}
	var em *ssh.ExitMissingError
	if errors.As(err, &em) {
		return &ExitError{Code: -1}
	}
	return err
}

// syncBuffer is a bytes.Buffer safe for the concurrent stdout/stderr copies of a session.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// Output runs cmd and returns its combined output.
func Output(ctx context.Context, r Runner, cmd string, stdin io.Reader) (string, error) {
	var buf syncBuffer
	err := r.Run(ctx, cmd, stdin, &buf)
	return buf.String(), err
}

// Quote quotes s for a POSIX shell.
func Quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// WithTimeout runs fn with a deadline of d; a step that runs out of time reports which
// step it was and how long it was given.
func WithTimeout(ctx context.Context, step string, d time.Duration, fn func(ctx context.Context) error) error {
	sctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	err := fn(sctx)
	if err != nil && errors.Is(sctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
		return fmt.Errorf("%s timed out after %s", step, d)
	}
	return err
}
