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
	"context"
	"strings"
	"testing"
	"time"
)

func TestLocalRunReturnsOutput(t *testing.T) {
	stdout, stderr, err := (&Local{}).Run(context.Background(), "sh", "-c", "echo out; echo err >&2")
	if err != nil || stdout != "out\n" || stderr != "err\n" {
		t.Fatalf("got %q, %q, %v", stdout, stderr, err)
	}
}

func TestLocalRunCancelSendsTerm(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)

	stdout, _, _ := (&Local{}).Run(ctx, "sh", "-c", `trap 'echo cleaned; exit 1' TERM; echo started; while :; do sleep 0.05; done`)
	if !strings.Contains(stdout, "cleaned") {
		t.Fatalf("command was not given SIGTERM, stdout %q", stdout)
	}
}
