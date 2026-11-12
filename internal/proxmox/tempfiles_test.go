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
	"context"
	"errors"
	"testing"

	"github.com/gillesdubois/plakar-integration-proxmox/internal/runner"
)

type removeRunner struct {
	runner.Runner
	fail    bool
	removed []string
}

func (r *removeRunner) Remove(ctx context.Context, path string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.fail {
		return errors.New("rm failed")
	}
	r.removed = append(r.removed, path)
	return nil
}

func TestTempFilesRemoveAfterCancel(t *testing.T) {
	r := &removeRunner{}
	temp := NewTempFiles(&Client{runner: r})
	temp.Track("/dump/a.tar")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := temp.Remove(ctx, "/dump/a.tar"); err != nil {
		t.Fatal(err)
	}
	if len(r.removed) == 0 || r.removed[0] != "/dump/a.tar" {
		t.Fatalf("removed %v", r.removed)
	}
}

func TestTempFilesSweepRetriesFailedRemove(t *testing.T) {
	r := &removeRunner{fail: true}
	temp := NewTempFiles(&Client{runner: r})
	temp.Track("/dump/a.tar")

	if err := temp.Remove(context.Background(), "/dump/a.tar"); err == nil {
		t.Fatal("expected error")
	}
	r.fail = false
	temp.Sweep(context.Background())
	if len(r.removed) == 0 || r.removed[0] != "/dump/a.tar" {
		t.Fatalf("removed %v", r.removed)
	}
}
