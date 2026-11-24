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

package proxmox

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/PlakarKorp/kloset/connectors"
)

const DumpTimeFormat = "2006_01_02-15_04_05"

const qemuConfigDir = "/etc/pve/qemu-server"
const lxcConfigDir = "/etc/pve/lxc"

func (c *Client) BackupLXCStream(ctx context.Context, vmid int) (string, io.ReadCloser) {
	args := []string{strconv.Itoa(vmid), "--stdout", "--mode", c.cfg.BackupMode, "--compress", "0"}
	archiveName := fmt.Sprintf("vzdump-lxc-%d-%s.tar", vmid, time.Now().Format(DumpTimeFormat))

	return archiveName, connectors.NewLazyReader(func() (io.ReadCloser, error) {
		return c.startVzdump(ctx, args)
	})
}

func (c *Client) startVzdump(ctx context.Context, args []string) (io.ReadCloser, error) {
	c.vzdumpMu.Lock()
	if err := ctx.Err(); err != nil {
		c.vzdumpMu.Unlock()
		return nil, err
	}
	stream, err := c.runner.Stream(ctx, "vzdump", args...)
	if err != nil {
		c.vzdumpMu.Unlock()
		return nil, fmt.Errorf("vzdump stream failed: %w", err)
	}

	stderrBuf := &bytes.Buffer{}
	doneCh := make(chan struct{})

	go func() {
		defer close(doneCh)
		_, _ = io.Copy(stderrBuf, stream.Stderr)
	}()

	return &streamReadCloser{
		stdout:     stream.Stdout,
		finish:     stream.Finish,
		abort:      stream.Abort,
		release:    c.vzdumpMu.Unlock,
		stderr:     stderrBuf,
		stderrDone: doneCh,
	}, nil
}

func (c *Client) ReadQEMUConfig(ctx context.Context, vmid int) ([]byte, error) {
	return c.readVMConfig(ctx, "qemu", vmid)
}

func (c *Client) ReadLXCConfig(ctx context.Context, vmid int) ([]byte, error) {
	return c.readVMConfig(ctx, "lxc", vmid)
}

func VMConfigPath(vmType string, vmid int) (string, error) {
	switch vmType {
	case "qemu":
		return path.Join(qemuConfigDir, fmt.Sprintf("%d.conf", vmid)), nil
	case "lxc":
		return path.Join(lxcConfigDir, fmt.Sprintf("%d.conf", vmid)), nil
	default:
		return "", fmt.Errorf("unsupported VM type for config path: %s", vmType)
	}
}

func (c *Client) readVMConfig(ctx context.Context, vmType string, vmid int) ([]byte, error) {
	configPath, err := VMConfigPath(vmType, vmid)
	if err != nil {
		return nil, err
	}

	reader, err := c.Open(ctx, configPath)
	if err != nil {
		return nil, fmt.Errorf("unable to read %s config %s: %w", vmType, configPath, err)
	}
	defer func() { _ = reader.Close() }()

	configData, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("unable to read %s config content %s: %w", vmType, configPath, err)
	}

	return configData, nil
}

type streamReadCloser struct {
	stdout     io.Reader
	finish     func() error
	abort      func() error
	release    func()
	read       int64
	eof        bool
	stderr     *bytes.Buffer
	stderrDone <-chan struct{}
	closed     bool
	finished   bool
	finishErr  error
}

func (r *streamReadCloser) Read(p []byte) (int, error) {
	n, err := r.stdout.Read(p)
	r.read += int64(n)
	if err == io.EOF {
		r.eof = true
		if finishErr := r.finalize(); finishErr != nil {
			return n, finishErr
		}
		if r.read == 0 {
			return 0, fmt.Errorf("vzdump produced no data")
		}
	}
	return n, err
}

func (r *streamReadCloser) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	if !r.eof && r.abort != nil {
		_ = r.abort()
		go func() { _, _ = io.Copy(io.Discard, r.stdout) }()
	}
	return r.finalize()
}

func (r *streamReadCloser) finalize() error {
	if r.finished {
		return r.finishErr
	}
	r.finished = true

	if r.stderrDone != nil {
		<-r.stderrDone
	}
	var err error
	if r.finish != nil {
		err = r.finish()
	}
	if r.release != nil {
		r.release()
	}
	if err != nil {
		r.finishErr = fmt.Errorf("vzdump failed: %w: %s", err, strings.TrimSpace(r.stderr.String()))
	}
	return r.finishErr
}
