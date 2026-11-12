/*
 * Copyright (c) 2026 Gilles DUBOIS
 *
 * Permission to use, copy, modify, and distribute this software for any
 * purpose with or without fee is hereby granted, provided that the above
 * copyright notice and this permission notice appear in all copies.
 *
 * THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES
 * WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF
 * MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR
 * ANY SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES
 * WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN AN
 * ACTION OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT OF
 * OR IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.
 */

package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

type SSHConfig struct {
	Host         string
	User         string
	Password     string
	IdentityFile string
	IdentityKey  string

	RetryCount int
	RetryDelay time.Duration
}

type SSH struct {
	dial func() (*ssh.Client, error)

	mu     sync.Mutex
	client *ssh.Client

	maxRetries int
	retryDelay time.Duration
}

func NewSSH(cfg SSHConfig) (*SSH, error) {
	if cfg.User == "" {
		return nil, fmt.Errorf("missing ssh user")
	}

	auth := ssh.Password(cfg.Password)
	if cfg.IdentityFile != "" || cfg.IdentityKey != "" {
		key := []byte(cfg.IdentityKey)
		if cfg.IdentityFile != "" {
			var err error
			key, err = os.ReadFile(cfg.IdentityFile)
			if err != nil {
				return nil, fmt.Errorf("failed to read identity file: %w", err)
			}
		}
		signer, err := ssh.ParsePrivateKey(key)
		if err != nil {
			return nil, fmt.Errorf("failed to parse identity key: %w", err)
		}
		auth = ssh.PublicKeys(signer)
	}

	addr := normalizeSSHAddr(cfg.Host)

	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	verifyHostKey, err := knownHostsCallback(filepath.Join(home, ".ssh", "known_hosts"), addr)
	if err != nil {
		return nil, err
	}

	clientCfg := &ssh.ClientConfig{
		User:              cfg.User,
		Auth:              []ssh.AuthMethod{auth},
		HostKeyCallback:   verifyHostKey,
		HostKeyAlgorithms: knownHostKeyAlgorithms(filepath.Join(home, ".ssh", "known_hosts"), addr),
		Timeout:           30 * time.Second,
	}

	dial := func() (*ssh.Client, error) {
		return ssh.Dial("tcp", addr, clientCfg)
	}

	client, err := dial()
	if err != nil {
		return nil, fmt.Errorf("ssh dial failed: %w", err)
	}

	return &SSH{
		dial:       dial,
		client:     client,
		maxRetries: cfg.RetryCount,
		retryDelay: cfg.RetryDelay,
	}, nil
}

// Trust on first use, like ssh's accept-new: a changed key is still rejected.
func knownHostsCallback(knownHostsPath, addr string) (ssh.HostKeyCallback, error) {
	if err := os.MkdirAll(filepath.Dir(knownHostsPath), 0700); err != nil {
		return nil, fmt.Errorf("unable to create known_hosts directory: %w", err)
	}
	f, err := os.OpenFile(knownHostsPath, os.O_CREATE|os.O_RDONLY, 0600)
	if err != nil {
		return nil, fmt.Errorf("unable to open known_hosts %s: %w", knownHostsPath, err)
	}
	_ = f.Close()

	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		callback, err := knownhosts.New(knownHostsPath)
		if err != nil {
			return fmt.Errorf("unable to parse known_hosts %s: %w", knownHostsPath, err)
		}

		err = callback(hostname, remote, key)
		var keyErr *knownhosts.KeyError
		if err == nil || !errors.As(err, &keyErr) {
			return err
		}
		if len(keyErr.Want) > 0 {
			return hostKeyMismatch(addr, knownHostsPath, key)
		}
		return addKnownHost(knownHostsPath, addr, key)
	}, nil
}

type probeKey struct{}

func (probeKey) Type() string                        { return "probe" }
func (probeKey) Marshal() []byte                     { return []byte("probe") }
func (probeKey) Verify([]byte, *ssh.Signature) error { return errors.New("probe key") }

func knownHostKeyAlgorithms(knownHostsPath, addr string) []string {
	callback, err := knownhosts.New(knownHostsPath)
	if err != nil {
		return nil
	}
	var keyErr *knownhosts.KeyError
	if !errors.As(callback(addr, &net.TCPAddr{}, probeKey{}), &keyErr) {
		return nil
	}

	var algos []string
	for _, known := range keyErr.Want {
		types := []string{known.Key.Type()}
		if types[0] == ssh.KeyAlgoRSA {
			types = []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSA}
		}
		for _, t := range types {
			if !slices.Contains(algos, t) {
				algos = append(algos, t)
			}
		}
	}
	return algos
}

func addKnownHost(knownHostsPath, addr string, key ssh.PublicKey) error {
	f, err := os.OpenFile(knownHostsPath, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("unable to add %s to known_hosts: %w", addr, err)
	}
	defer func() { _ = f.Close() }()

	if _, err := fmt.Fprintln(f, knownhosts.Line([]string{knownhosts.Normalize(addr)}, key)); err != nil {
		return fmt.Errorf("unable to add %s to known_hosts: %w", addr, err)
	}
	log.Printf("permanently added %s (%s, fingerprint %s) to %s",
		addr, key.Type(), ssh.FingerprintSHA256(key), knownHostsPath)
	return nil
}

func hostKeyMismatch(addr, knownHostsPath string, key ssh.PublicKey) error {
	return fmt.Errorf("host key mismatch for %s: the node presented a %s key with fingerprint %s, "+
		"which does not match the entry in %s. Either the node was reinstalled or rekeyed, or this "+
		"is a man-in-the-middle. If the change is expected, drop the stale entry with: "+
		"ssh-keygen -R %q -f %q",
		addr, key.Type(), ssh.FingerprintSHA256(key), knownHostsPath,
		knownhosts.Normalize(addr), knownHostsPath)
}

func (r *SSH) withRetry(ctx context.Context, op func(*ssh.Client) error) error {
	var lastErr error
	for attempt := 0; ; attempt++ {
		client := r.currentClient()
		err := op(client)
		if err == nil {
			return nil
		}
		lastErr = err
		if attempt >= r.maxRetries {
			return fmt.Errorf("ssh failed after %d attempt(s): %w", attempt+1, lastErr)
		}

		select {
		case <-time.After(r.retryDelay):
		case <-ctx.Done():
			return ctx.Err()
		}
		if err := r.reconnect(client); err != nil {
			lastErr = fmt.Errorf("ssh reconnect failed: %w", err)
		}
	}
}

func (r *SSH) newSession(ctx context.Context) (*ssh.Session, error) {
	var session *ssh.Session
	err := r.withRetry(ctx, func(client *ssh.Client) (err error) {
		session, err = client.NewSession()
		return err
	})
	return session, err
}

func (r *SSH) currentClient() *ssh.Client {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.client
}

func (r *SSH) reconnect(failed *ssh.Client) error {
	if alive(failed) {
		return nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.client != failed {
		return nil
	}

	client, err := r.dial()
	if err != nil {
		return err
	}
	r.client = client
	_ = failed.Close()
	return nil
}

func alive(c *ssh.Client) bool {
	answered := make(chan error, 1)
	go func() {
		_, _, err := c.SendRequest("keepalive@openssh.com", true, nil)
		answered <- err
	}()
	select {
	case err := <-answered:
		return err == nil
	case <-time.After(10 * time.Second):
		return false
	}
}

// Closing the channel alone does not signal a command running without a tty.
func terminate(session *ssh.Session) {
	if err := session.Signal(ssh.SIGTERM); err != nil {
		_ = session.Close()
		return
	}
	time.AfterFunc(killDelay, func() { _ = session.Close() })
}

func (r *SSH) Run(ctx context.Context, name string, args ...string) (string, string, error) {
	session, err := r.newSession(ctx)
	if err != nil {
		return "", "", err
	}
	defer func() {
		_ = session.Close()
	}()

	var stdout, stderr bytes.Buffer
	session.Stdout = &stdout
	session.Stderr = &stderr

	cmd := shellCommand(name, args...)
	stop := context.AfterFunc(ctx, func() { terminate(session) })
	defer stop()

	err = session.Run(cmd)
	return stdout.String(), stderr.String(), err
}

func (r *SSH) Stream(ctx context.Context, name string, args ...string) (*CommandStream, error) {
	session, err := r.newSession(ctx)
	if err != nil {
		return nil, err
	}

	stdout, err := session.StdoutPipe()
	if err != nil {
		_ = session.Close()
		return nil, err
	}

	stderr, err := session.StderrPipe()
	if err != nil {
		_ = session.Close()
		return nil, err
	}

	cmd := shellCommand(name, args...)
	if err := session.Start(cmd); err != nil {
		_ = session.Close()
		return nil, err
	}

	stop := context.AfterFunc(ctx, func() { terminate(session) })

	return &CommandStream{
		Stdout: stdout,
		Stderr: stderr,
		finish: func() error {
			err := session.Wait()
			stop()
			_ = session.Close()
			return err
		},
		abort: func() error {
			terminate(session)
			return nil
		},
	}, nil
}

func (r *SSH) Open(ctx context.Context, filepath string) (io.ReadCloser, error) {
	session, err := r.newSession(ctx)
	if err != nil {
		return nil, err
	}

	stdout, err := session.StdoutPipe()
	if err != nil {
		_ = session.Close()
		return nil, err
	}

	var stderr bytes.Buffer
	session.Stderr = &stderr

	cmd := fmt.Sprintf("cat -- %s", shellQuote(filepath))
	if err := session.Start(cmd); err != nil {
		_ = session.Close()
		return nil, err
	}

	return &sshReadCloser{
		session: session,
		stdout:  stdout,
		stderr:  &stderr,
	}, nil
}

func (r *SSH) Create(ctx context.Context, filepath string) (io.WriteCloser, error) {
	session, err := r.newSession(ctx)
	if err != nil {
		return nil, err
	}

	stdin, err := session.StdinPipe()
	if err != nil {
		_ = session.Close()
		return nil, err
	}

	var stderr bytes.Buffer
	session.Stderr = &stderr

	args := sparseWriteArgs(filepath)
	cmd := shellCommand(args[0], args[1:]...)
	if err := session.Start(cmd); err != nil {
		_ = stdin.Close()
		_ = session.Close()
		return nil, err
	}

	return &sshWriteCloser{
		session: session,
		stdin:   stdin,
		stderr:  &stderr,
	}, nil
}

func (r *SSH) Remove(ctx context.Context, filepath string) error {
	_, stderr, err := r.Run(ctx, "rm", "-f", "--", filepath)
	if err != nil {
		return fmt.Errorf("rm failed: %w: %s", err, strings.TrimSpace(stderr))
	}
	return nil
}

func (r *SSH) ListenUnix(socketPath string) (net.Listener, error) {
	var ln net.Listener
	err := r.withRetry(context.Background(), func(client *ssh.Client) (err error) {
		ln, err = client.ListenUnix(socketPath)
		return err
	})
	return ln, err
}

func (r *SSH) DialUnix(socketPath string) (net.Conn, error) {
	var conn net.Conn
	err := r.withRetry(context.Background(), func(client *ssh.Client) (err error) {
		conn, err = client.Dial("unix", socketPath)
		return err
	})
	return conn, err
}

func (r *SSH) Close() error {
	if client := r.currentClient(); client != nil {
		return client.Close()
	}
	return nil
}

type sshReadCloser struct {
	session *ssh.Session
	stdout  io.Reader
	stderr  *bytes.Buffer
	closed  bool
}

func (r *sshReadCloser) Read(p []byte) (int, error) {
	return r.stdout.Read(p)
}

func (r *sshReadCloser) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true

	_, _ = io.Copy(io.Discard, r.stdout)
	err := r.session.Wait()
	_ = r.session.Close()
	if err != nil {
		return fmt.Errorf("remote read failed: %w: %s", err, strings.TrimSpace(r.stderr.String()))
	}
	return nil
}

type sshWriteCloser struct {
	session *ssh.Session
	stdin   io.WriteCloser
	stderr  *bytes.Buffer
	closed  bool
}

func (w *sshWriteCloser) Write(p []byte) (int, error) {
	return w.stdin.Write(p)
}

func (w *sshWriteCloser) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true

	errClose := w.stdin.Close()
	errWait := w.session.Wait()
	_ = w.session.Close()

	if errClose != nil {
		return errClose
	}
	if errWait != nil {
		return fmt.Errorf("remote write failed: %w: %s", errWait, strings.TrimSpace(w.stderr.String()))
	}
	return nil
}

func normalizeSSHAddr(host string) string {
	if host == "" {
		return ""
	}
	if _, _, err := net.SplitHostPort(host); err == nil {
		return host
	}
	return net.JoinHostPort(host, "22")
}

func shellCommand(name string, args ...string) string {
	parts := append([]string{name}, args...)
	for i, part := range parts {
		parts[i] = shellQuote(part)
	}
	return strings.Join(parts, " ")
}

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
