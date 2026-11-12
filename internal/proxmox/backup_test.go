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
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gillesdubois/plakar-integration-proxmox/internal/runner"
)

type streamRunner struct {
	runner.Runner
	started int32
}

func (r *streamRunner) Stream(ctx context.Context, name string, args ...string) (*runner.CommandStream, error) {
	atomic.AddInt32(&r.started, 1)
	return runner.NewCommandStream(strings.NewReader("VMA\x00data"), strings.NewReader(""), nil, nil), nil
}

func TestBackupVMStreamStartsVzdumpOnFirstRead(t *testing.T) {
	runner := &streamRunner{}
	client := &Client{cfg: &Config{BackupMode: "snapshot"}, runner: runner}

	name, reader := client.BackupLXCStream(context.Background(), 301)
	if !strings.HasPrefix(name, "vzdump-lxc-301-") || !strings.HasSuffix(name, ".tar") {
		t.Errorf("unexpected archive name %q", name)
	}
	if atomic.LoadInt32(&runner.started) != 0 {
		t.Fatal("vzdump must not start before the first Read")
	}

	data, err := io.ReadAll(reader)
	if err != nil || string(data) != "VMA\x00data" || atomic.LoadInt32(&runner.started) != 1 {
		t.Fatalf("got data=%q err=%v started=%d", data, err, atomic.LoadInt32(&runner.started))
	}
	_ = reader.Close()

	_, unread := client.BackupLXCStream(context.Background(), 500)
	if err := unread.Close(); err != nil || atomic.LoadInt32(&runner.started) != 1 {
		t.Fatalf("closing an unread stream: err=%v started=%d", err, atomic.LoadInt32(&runner.started))
	}
}

func TestOneVzdumpAtATime(t *testing.T) {
	runner := &streamRunner{}
	client := &Client{cfg: &Config{BackupMode: "snapshot"}, runner: runner}

	_, first := client.BackupLXCStream(context.Background(), 301)
	_, second := client.BackupLXCStream(context.Background(), 302)

	if _, err := first.Read(make([]byte, 1)); err != nil {
		t.Fatal(err)
	}

	secondDone := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(second)
		secondDone <- err
	}()

	select {
	case <-secondDone:
		t.Fatal("second vzdump started while the first one was still streaming")
	case <-time.After(50 * time.Millisecond):
	}

	_ = first.Close()
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&runner.started); got != 2 {
		t.Fatalf("started %d vzdump, want 2", got)
	}
}

func TestStreamReadCloserReportsVzdumpFailure(t *testing.T) {
	done := make(chan struct{})
	close(done)
	r := &streamReadCloser{
		stdout:     strings.NewReader("partial"),
		finish:     func() error { return errors.New("exit status 1") },
		stderr:     bytes.NewBufferString("ERROR: VM is locked\n"),
		stderrDone: done,
	}

	data, err := io.ReadAll(r)
	if err == nil || !strings.Contains(err.Error(), "VM is locked") {
		t.Fatalf("expected vzdump failure with stderr, got data=%q err=%v", data, err)
	}
	if cerr := r.Close(); cerr == nil {
		t.Fatal("Close must keep reporting the vzdump failure")
	}
}

type shRunner struct {
	runner.Local
	script string
}

func (r *shRunner) Stream(ctx context.Context, _ string, _ ...string) (*runner.CommandStream, error) {
	return r.Local.Stream(ctx, "sh", "-c", r.script)
}

func TestLocalVzdumpFailureKeepsStderr(t *testing.T) {
	for script, want := range map[string]string{
		`for i in $(seq 200); do echo "INFO: line $i" >&2; done; echo "ERROR: CT 301 is locked" >&2; exit 2`: "CT 301 is locked",
		`exit 0`: "no data",
	} {
		client := &Client{cfg: &Config{BackupMode: "snapshot"}, runner: &shRunner{script: script}}
		_, reader := client.BackupLXCStream(context.Background(), 301)
		_, err := io.ReadAll(reader)
		_ = reader.Close()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want an error with %q", script, err, want)
		}
	}
}
