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

package nbd

import (
	"bytes"
	"crypto/rand"
	"io"
	"net"
	"strings"
	"testing"

	"github.com/gillesdubois/plakar-integration-proxmox/internal/nbd/nbdtest"
)

func TestNBDCopy(t *testing.T) {
	data := make([]byte, (2*nbdBuffers-1)*nbdChunkSize+12345)
	_, _ = rand.Read(data[:nbdChunkSize/2])
	_, _ = rand.Read(data[len(data)-100:])

	for _, structured := range []bool{false, true} {
		srv := &nbdtest.Server{Data: data, Structured: structured}
		conn, err := net.Dial("unix", srv.Listen(t))
		if err != nil {
			t.Fatal(err)
		}

		var got bytes.Buffer
		if err := Copy(&got, conn, "drive-scsi0", int64(len(data))); err != nil {
			t.Fatalf("structured=%v: %v", structured, err)
		}
		_ = conn.Close()

		if !bytes.Equal(got.Bytes(), data) {
			t.Fatalf("structured=%v: data differs", structured)
		}
		if srv.Trims.Load() != 2*nbdBuffers {
			t.Errorf("structured=%v: %d trims, want one per chunk", structured, srv.Trims.Load())
		}
		if structured != (srv.Holes.Load() > 0) {
			t.Errorf("structured=%v: %d holes sent", structured, srv.Holes.Load())
		}
	}
}

func TestNBDCopyRejectsPartialRead(t *testing.T) {
	srv := &nbdtest.Server{Data: bytes.Repeat([]byte{7}, nbdChunkSize), Structured: true, SkipFirst: true}
	conn, err := net.Dial("unix", srv.Listen(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	err = Copy(io.Discard, conn, "drive-scsi0", int64(len(srv.Data)))
	if err == nil || !strings.Contains(err.Error(), "covered") {
		t.Fatalf("a read with a gap must fail, got %v", err)
	}
}

type failingWriter struct{ n int }

func (f *failingWriter) Write(p []byte) (int, error) {
	if f.n++; f.n > 1 {
		return 0, io.ErrClosedPipe
	}
	return len(p), nil
}

func TestNBDCopyStopsOnWriteError(t *testing.T) {
	srv := &nbdtest.Server{Data: make([]byte, 4*nbdBuffers*nbdChunkSize)}
	conn, err := net.Dial("unix", srv.Listen(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	err = Copy(&failingWriter{}, conn, "drive-scsi0", int64(len(srv.Data)))
	if err != io.ErrClosedPipe {
		t.Fatalf("got %v, want the writer's error", err)
	}
	if n := srv.Trims.Load(); n > 2+nbdBuffers {
		t.Errorf("%d pieces read after the writer failed", n)
	}
}
