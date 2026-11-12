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

package nbdtest

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"sync/atomic"
	"testing"
)

const (
	nbdMagic              = 0x4e42444d41474943
	nbdOptMagic           = 0x49484156454f5054
	nbdOptReplyMagic      = 0x3e889045565a9
	nbdSimpleReplyMagic   = 0x67446698
	nbdStructReplyMagic   = 0x668e33ef
	nbdFlagFixedNewstyle  = 1 << 0
	nbdFlagNoZeroes       = 1 << 1
	nbdOptGo              = 7
	nbdOptStructuredReply = 8
	nbdRepAck             = 1
	nbdRepInfo            = 3
	nbdRepFlagErr         = 1 << 31
	nbdTransSendTrim      = 1 << 5
	nbdCmdRead            = 0
	nbdCmdDisc            = 2
	nbdCmdTrim            = 4
	nbdReplyFlagDone      = 1 << 0
	nbdReplyTypeData      = 1
	nbdReplyTypeHole      = 2

	piece = 64 << 10
)

var be = binary.BigEndian

type Server struct {
	Data       []byte
	Structured bool // accept structured replies and send zero pieces as holes
	SkipFirst  bool // leave the first piece of every structured read out, like a broken server
	Holes      atomic.Int64
	Trims      atomic.Int64
}

func (s *Server) Listen(t testing.TB) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "nbd.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go s.serve(ln)
	return sock
}

func (s *Server) serve(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer func() { _ = conn.Close() }()
			_ = s.handle(conn)
		}()
	}
}

func (s *Server) handle(conn io.ReadWriter) error {
	var hello [18]byte
	be.PutUint64(hello[0:], nbdMagic)
	be.PutUint64(hello[8:], nbdOptMagic)
	be.PutUint16(hello[16:], nbdFlagFixedNewstyle|nbdFlagNoZeroes)
	if _, err := conn.Write(hello[:]); err != nil {
		return err
	}
	var clientFlags uint32
	if err := binary.Read(conn, be, &clientFlags); err != nil {
		return err
	}

	structured := false
	for {
		var hdr struct {
			Magic    uint64
			Opt, Len uint32
		}
		if err := binary.Read(conn, be, &hdr); err != nil {
			return err
		}
		if _, err := io.CopyN(io.Discard, conn, int64(hdr.Len)); err != nil {
			return err
		}

		switch {
		case hdr.Opt == nbdOptStructuredReply && s.Structured:
			structured = true
			if err := optReply(conn, hdr.Opt, nbdRepAck, nil); err != nil {
				return err
			}
		case hdr.Opt == nbdOptGo:
			info := make([]byte, 12)
			be.PutUint64(info[2:], uint64(len(s.Data)))
			be.PutUint16(info[10:], 1|nbdTransSendTrim)
			if err := optReply(conn, hdr.Opt, nbdRepInfo, info); err != nil {
				return err
			}
			if err := optReply(conn, hdr.Opt, nbdRepAck, nil); err != nil {
				return err
			}
			return s.transmission(conn, structured)
		default:
			if err := optReply(conn, hdr.Opt, nbdRepFlagErr|1, nil); err != nil {
				return err
			}
		}
	}
}

func optReply(w io.Writer, opt, typ uint32, payload []byte) error {
	reply := make([]byte, 20+len(payload))
	be.PutUint64(reply[0:], nbdOptReplyMagic)
	be.PutUint32(reply[8:], opt)
	be.PutUint32(reply[12:], typ)
	be.PutUint32(reply[16:], uint32(len(payload)))
	copy(reply[20:], payload)
	_, err := w.Write(reply)
	return err
}

func (s *Server) transmission(conn io.ReadWriter, structured bool) error {
	var req [28]byte
	for {
		if _, err := io.ReadFull(conn, req[:]); err != nil {
			return err
		}
		typ, handle := be.Uint16(req[6:]), be.Uint64(req[8:])
		off, n := int(be.Uint64(req[16:])), int(be.Uint32(req[24:]))

		var out bytes.Buffer
		put := func(values ...any) {
			for _, v := range values {
				_ = binary.Write(&out, be, v)
			}
		}
		switch typ {
		case nbdCmdDisc:
			return nil
		case nbdCmdTrim:
			s.Trims.Add(1)
			put(uint32(nbdSimpleReplyMagic), uint32(0), handle)
		case nbdCmdRead:
			if off+n > len(s.Data) {
				return fmt.Errorf("read past the end")
			}
			if !structured {
				put(uint32(nbdSimpleReplyMagic), uint32(0), handle)
				out.Write(s.Data[off : off+n])
				break
			}
			for at := off; at < off+n; at += piece {
				piece := s.Data[at:min(at+piece, off+n)]
				flags := uint16(0)
				if at+len(piece) == off+n {
					flags = nbdReplyFlagDone
				}
				if s.SkipFirst && at == off && len(piece) < n {
					continue
				}
				if bytes.Count(piece, []byte{0}) == len(piece) {
					s.Holes.Add(1)
					put(uint32(nbdStructReplyMagic), flags, uint16(nbdReplyTypeHole), handle, uint32(12), uint64(at), uint32(len(piece)))
				} else {
					put(uint32(nbdStructReplyMagic), flags, uint16(nbdReplyTypeData), handle, uint32(8+len(piece)), uint64(at))
					out.Write(piece)
				}
			}
		default:
			return fmt.Errorf("unexpected command %d", typ)
		}
		if _, err := conn.Write(out.Bytes()); err != nil {
			return err
		}
	}
}
