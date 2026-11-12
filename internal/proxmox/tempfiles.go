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
	"log"
	"sync"
)

type TempFiles struct {
	client *Client

	mu    sync.Mutex
	paths map[string]struct{}
}

func NewTempFiles(client *Client) *TempFiles {
	return &TempFiles{
		client: client,
		paths:  make(map[string]struct{}),
	}
}

func (t *TempFiles) Track(filepath string) {
	if filepath == "" {
		return
	}

	t.mu.Lock()
	t.paths[filepath] = struct{}{}
	t.mu.Unlock()
}

func (t *TempFiles) Forget(filepath string) {
	t.mu.Lock()
	delete(t.paths, filepath)
	t.mu.Unlock()
}

func (t *TempFiles) Remove(ctx context.Context, filepath string) error {
	if filepath == "" {
		return nil
	}

	ctx = context.WithoutCancel(ctx)
	if err := t.client.Remove(ctx, filepath); err != nil {
		return err
	}
	t.Forget(filepath)

	if logPath := DumpLogPath(filepath); logPath != "" {
		_ = t.client.Remove(ctx, logPath)
	}
	return nil
}

func (t *TempFiles) Sweep(ctx context.Context) {
	t.mu.Lock()
	paths := make([]string, 0, len(t.paths))
	for filepath := range t.paths {
		paths = append(paths, filepath)
	}
	t.mu.Unlock()

	for _, filepath := range paths {
		if err := t.Remove(ctx, filepath); err != nil {
			log.Printf("unable to remove temporary dump %s: %v", filepath, err)
		}
	}
}
