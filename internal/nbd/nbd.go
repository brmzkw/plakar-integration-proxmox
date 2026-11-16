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
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
)

// Fixed newstyle NBD client:
// https://github.com/NetworkBlockDevice/nbd/blob/master/doc/proto.md
const (
	nbdMagic              = 0x4e42444d41474943
	nbdOptMagic           = 0x49484156454f5054
	nbdOptReplyMagic      = 0x3e889045565a9
	nbdRequestMagic       = 0x25609513
	nbdSimpleReplyMagic   = 0x67446698
	nbdStructReplyMagic   = 0x668e33ef
	nbdFlagFixedNewstyle  = 1 << 0
	nbdFlagNoZeroes       = 1 << 1
	nbdOptGo              = 7
	nbdOptStructuredReply = 8
	nbdRepAck             = 1
	nbdRepInfo            = 3
	nbdRepFlagErr         = 1 << 31
	nbdInfoExport         = 0
	nbdTransSendTrim      = 1 << 5
	nbdCmdRead            = 0
	nbdCmdDisc            = 2
	nbdCmdTrim            = 4
	nbdReplyFlagDone      = 1 << 0
	nbdReplyTypeNone      = 0
	nbdReplyTypeData      = 1
	nbdReplyTypeHole      = 2
	nbdReplyTypeErrFlag   = 1 << 15

	nbdChunkSize = 16 << 20
	nbdBuffers   = 3
)

var be = binary.BigEndian

type nbdRefusedError struct {
	opt, typ uint32
	msg      string
}

func (e *nbdRefusedError) Error() string {
	return fmt.Sprintf("nbd option %d refused (%#x): %s", e.opt, e.typ, e.msg)
}

type nbdClient struct {
	conn       net.Conn
	size       uint64
	flags      uint16
	handle     uint64
	structured bool
}

func nbdConnect(conn net.Conn, export string) (*nbdClient, error) {
	var hello struct {
		Magic, OptMagic uint64
		Flags           uint16
	}
	if err := binary.Read(conn, be, &hello); err != nil {
		return nil, fmt.Errorf("nbd handshake: %w", err)
	}
	if hello.Magic != nbdMagic || hello.OptMagic != nbdOptMagic || hello.Flags&nbdFlagFixedNewstyle == 0 {
		return nil, fmt.Errorf("nbd handshake: server does not speak fixed newstyle")
	}
	if err := binary.Write(conn, be, uint32(nbdFlagFixedNewstyle)|uint32(hello.Flags&nbdFlagNoZeroes)); err != nil {
		return nil, err
	}

	c := &nbdClient{conn: conn}

	var refused *nbdRefusedError
	switch err := c.option(nbdOptStructuredReply, nil); {
	case err == nil:
		c.structured = true
	case !errors.As(err, &refused):
		return nil, err
	}

	var name bytes.Buffer
	_ = binary.Write(&name, be, uint32(len(export)))
	name.WriteString(export)
	_ = binary.Write(&name, be, uint16(0))
	if err := c.option(nbdOptGo, name.Bytes()); err != nil {
		return nil, fmt.Errorf("nbd export %q: %w", export, err)
	}
	return c, nil
}

func (c *nbdClient) option(opt uint32, data []byte) error {
	req := make([]byte, 16+len(data))
	be.PutUint64(req[0:], nbdOptMagic)
	be.PutUint32(req[8:], opt)
	be.PutUint32(req[12:], uint32(len(data)))
	copy(req[16:], data)
	if _, err := c.conn.Write(req); err != nil {
		return err
	}

	for {
		var rep struct {
			Magic     uint64
			Opt, Type uint32
			Len       uint32
		}
		if err := binary.Read(c.conn, be, &rep); err != nil {
			return fmt.Errorf("nbd option reply: %w", err)
		}
		if rep.Magic != nbdOptReplyMagic || rep.Len > 1<<16 {
			return fmt.Errorf("nbd option reply: bad header")
		}
		payload := make([]byte, rep.Len)
		if _, err := io.ReadFull(c.conn, payload); err != nil {
			return err
		}

		switch {
		case rep.Type&nbdRepFlagErr != 0:
			return &nbdRefusedError{opt: opt, typ: rep.Type, msg: string(payload)}
		case rep.Type == nbdRepInfo && len(payload) >= 12 && be.Uint16(payload) == nbdInfoExport:
			c.size = be.Uint64(payload[2:])
			c.flags = be.Uint16(payload[10:])
		case rep.Type == nbdRepAck:
			return nil
		}
	}
}

func (c *nbdClient) command(typ uint16, offset uint64, length uint32, buf []byte) error {
	c.handle++
	var req [28]byte
	be.PutUint32(req[0:], nbdRequestMagic)
	be.PutUint16(req[6:], typ)
	be.PutUint64(req[8:], c.handle)
	be.PutUint64(req[16:], offset)
	be.PutUint32(req[24:], length)
	if _, err := c.conn.Write(req[:]); err != nil {
		return err
	}
	if typ == nbdCmdDisc {
		return nil
	}

	var covered uint64
	for {
		var magic [4]byte
		if _, err := io.ReadFull(c.conn, magic[:]); err != nil {
			return err
		}

		switch be.Uint32(magic[:]) {
		case nbdSimpleReplyMagic:
			var rep [12]byte
			if _, err := io.ReadFull(c.conn, rep[:]); err != nil {
				return err
			}
			if be.Uint64(rep[4:]) != c.handle {
				return fmt.Errorf("nbd: reply for an unknown request")
			}
			if code := be.Uint32(rep[0:]); code != 0 {
				return fmt.Errorf("nbd: command %d at %d failed with errno %d", typ, offset, code)
			}
			if buf != nil {
				_, err := io.ReadFull(c.conn, buf)
				return err
			}
			return nil

		case nbdStructReplyMagic:
			n, done, err := c.chunk(typ, offset, buf)
			if err != nil {
				return err
			}
			covered += n
			if !done {
				continue
			}
			// buf is reused across reads: a gap would carry the previous chunk into the backup.
			if covered != uint64(len(buf)) {
				return fmt.Errorf("nbd: read at %d covered %d of %d bytes", offset, covered, len(buf))
			}
			return nil

		default:
			return fmt.Errorf("nbd: unexpected reply magic")
		}
	}
}

func (c *nbdClient) chunk(typ uint16, offset uint64, buf []byte) (uint64, bool, error) {
	var hdr [16]byte
	if _, err := io.ReadFull(c.conn, hdr[:]); err != nil {
		return 0, false, err
	}
	flags, kind := be.Uint16(hdr[0:]), be.Uint16(hdr[2:])
	length := be.Uint32(hdr[12:])
	if be.Uint64(hdr[4:]) != c.handle {
		return 0, false, fmt.Errorf("nbd: reply for an unknown request")
	}

	within := func(from uint64, n uint64) ([]byte, error) {
		if from < offset || from-offset+n > uint64(len(buf)) {
			return nil, fmt.Errorf("nbd: reply chunk outside the request")
		}
		return buf[from-offset : from-offset+n], nil
	}

	var n uint64
	switch {
	case kind == nbdReplyTypeNone:
	case kind == nbdReplyTypeData && length >= 8:
		var at [8]byte
		if _, err := io.ReadFull(c.conn, at[:]); err != nil {
			return 0, false, err
		}
		dst, err := within(be.Uint64(at[:]), uint64(length-8))
		if err != nil {
			return 0, false, err
		}
		if _, err := io.ReadFull(c.conn, dst); err != nil {
			return 0, false, err
		}
		n = uint64(len(dst))
	case kind == nbdReplyTypeHole && length == 12:
		var hole [12]byte
		if _, err := io.ReadFull(c.conn, hole[:]); err != nil {
			return 0, false, err
		}
		dst, err := within(be.Uint64(hole[0:]), uint64(be.Uint32(hole[8:])))
		if err != nil {
			return 0, false, err
		}
		clear(dst)
		n = uint64(len(dst))
	case kind&nbdReplyTypeErrFlag != 0:
		if length < 6 || length > 1<<16 {
			return 0, false, fmt.Errorf("nbd: command %d at %d failed", typ, offset)
		}
		payload := make([]byte, length)
		if _, err := io.ReadFull(c.conn, payload); err != nil {
			return 0, false, err
		}
		msg := payload[6:min(len(payload), 6+int(be.Uint16(payload[4:])))]
		return 0, false, fmt.Errorf("nbd: command %d at %d failed with errno %d: %s", typ, offset, be.Uint32(payload), msg)
	default:
		if length > 1<<16 {
			return 0, false, fmt.Errorf("nbd: oversized reply chunk")
		}
		if _, err := io.CopyN(io.Discard, c.conn, int64(length)); err != nil {
			return 0, false, err
		}
	}
	return n, flags&nbdReplyFlagDone != 0, nil
}

// Trimming what was read keeps the fleecing image on the node from growing.
func Copy(w io.Writer, conn net.Conn, export string, size int64) error {
	c, err := nbdConnect(conn, export)
	if err != nil {
		return err
	}
	defer func() { _ = c.command(nbdCmdDisc, 0, 0, nil) }()

	if c.size < uint64(size) {
		return fmt.Errorf("%s: export is %d bytes, manifest says %d", export, c.size, size)
	}

	// The node is read ahead while Plakar consumes the previous pieces, so neither side waits on the other.
	free := make(chan []byte, nbdBuffers)
	full := make(chan []byte, nbdBuffers)
	for range nbdBuffers {
		free <- make([]byte, nbdChunkSize)
	}
	stop := make(chan struct{})
	var readErr error
	go func() {
		defer close(full)
		for off := int64(0); off < size; {
			var buf []byte
			select {
			case buf = <-free:
			case <-stop:
				return
			}
			chunk := buf[:min(int64(nbdChunkSize), size-off)]
			if err := c.command(nbdCmdRead, uint64(off), uint32(len(chunk)), chunk); err != nil {
				readErr = fmt.Errorf("%s: %w", export, err)
				return
			}
			if c.flags&nbdTransSendTrim != 0 {
				if err := c.command(nbdCmdTrim, uint64(off), uint32(len(chunk)), nil); err != nil {
					readErr = fmt.Errorf("%s: %w", export, err)
					return
				}
			}
			full <- chunk
			off += int64(len(chunk))
		}
	}()

	var writeErr error
	for chunk := range full {
		if writeErr != nil {
			continue
		}
		if _, writeErr = w.Write(chunk); writeErr != nil {
			close(stop)
			continue
		}
		free <- chunk[:cap(chunk)]
	}
	if writeErr != nil {
		return writeErr
	}
	return readErr
}
