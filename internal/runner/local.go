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
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

type Local struct{}

func command(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = killDelay
	return cmd
}

func (r *Local) Run(ctx context.Context, name string, args ...string) (string, string, error) {
	cmd := command(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

func (r *Local) Stream(ctx context.Context, name string, args ...string) (*CommandStream, error) {
	cmd := command(ctx, name, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdout.Close()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &CommandStream{
		Stdout: stdout,
		Stderr: stderr,
		finish: cmd.Wait,
		abort: func() error {
			err := cmd.Process.Signal(syscall.SIGTERM)
			time.AfterFunc(killDelay, func() { _ = cmd.Process.Kill() })
			return err
		},
	}, nil
}

func (r *Local) Open(ctx context.Context, filepath string) (io.ReadCloser, error) {
	return os.Open(filepath)
}

func (r *Local) Create(ctx context.Context, filepath string) (io.WriteCloser, error) {
	args := sparseWriteArgs(filepath)
	cmd := command(ctx, args[0], args[1:]...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &commandWriter{WriteCloser: stdin, cmd: cmd, stderr: &stderr}, nil
}

type commandWriter struct {
	io.WriteCloser
	cmd    *exec.Cmd
	stderr *bytes.Buffer
}

func (w *commandWriter) Close() error {
	errClose := w.WriteCloser.Close()
	if err := w.cmd.Wait(); err != nil {
		return fmt.Errorf("local write failed: %w: %s", err, strings.TrimSpace(w.stderr.String()))
	}
	return errClose
}

func (r *Local) Remove(ctx context.Context, filepath string) error {
	return os.Remove(filepath)
}

func (r *Local) ListenUnix(socketPath string) (net.Listener, error) {
	return net.Listen("unix", socketPath)
}

func (r *Local) DialUnix(socketPath string) (net.Conn, error) {
	return net.Dial("unix", socketPath)
}

func (r *Local) Close() error {
	return nil
}
