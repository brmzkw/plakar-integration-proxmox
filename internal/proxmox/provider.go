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
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"path"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gillesdubois/plakar-integration-proxmox/internal/nbd"
	"github.com/gillesdubois/plakar-integration-proxmox/internal/runner"
)

const ProviderManifestVersion = 1

const ProviderProtocolFile = "/usr/share/plakar-pve-provider/protocol"

const (
	ProviderManifestFile = "manifest.json"
	ProviderGuestFile    = "guest.conf"
	ProviderFirewallFile = "firewall.conf"
	ProviderDiskPrefix   = "vm-"
	ProviderDiskSuffix   = ".raw"

	MaxProviderManifest = 4 << 20
)

var providerDeviceRegex = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9_.-]*$`)

func ProviderDiskFile(device string) string {
	return ProviderDiskPrefix + device + ProviderDiskSuffix
}

func ParseProviderDiskFile(name string) (string, bool) {
	device, ok := strings.CutPrefix(name, ProviderDiskPrefix)
	if !ok {
		return "", false
	}
	device, ok = strings.CutSuffix(device, ProviderDiskSuffix)
	return device, ok && providerDeviceRegex.MatchString(device)
}

type ProviderDevice struct {
	Size    int64  `json:"size"`
	NBDPath string `json:"nbd-path,omitempty"`
	Image   string `json:"image,omitempty"`
}

type ProviderManifest struct {
	Version        int                       `json:"version"`
	VMID           int                       `json:"vmid"`
	Archive        string                    `json:"archive"`
	BandwidthLimit int64                     `json:"bandwidth-limit"`
	GuestConfig    string                    `json:"guest-config"`
	FirewallConfig *string                   `json:"firewall-config"`
	Devices        map[string]ProviderDevice `json:"devices"`
	Pool           string                    `json:"pool,omitempty"`
}

func (m *ProviderManifest) Validate() error {
	if m.Version != ProviderManifestVersion {
		return fmt.Errorf("unsupported provider manifest version %d", m.Version)
	}
	if len(m.Devices) == 0 {
		return fmt.Errorf("provider manifest lists no device")
	}
	for name, dev := range m.Devices {
		if !providerDeviceRegex.MatchString(name) {
			return fmt.Errorf("invalid device name %q in provider manifest", name)
		}
		if dev.Size < 0 {
			return fmt.Errorf("invalid size for device %s", name)
		}
	}
	return nil
}

func (m *ProviderManifest) DeviceNames() []string {
	names := make([]string, 0, len(m.Devices))
	for name := range m.Devices {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (m *ProviderManifest) Stored() ([]byte, error) {
	stored := *m
	stored.Devices = make(map[string]ProviderDevice, len(m.Devices))
	for name, dev := range m.Devices {
		stored.Devices[name] = ProviderDevice{Size: dev.Size, Image: dev.Image}
	}
	return json.MarshalIndent(&stored, "", "  ")
}

type ProviderRecord struct {
	File   string
	Reader io.ReadCloser
}

func (c *Client) ProviderBackup(ctx context.Context, vmid int, pool string, devices []string) []ProviderRecord {
	v := &providerVM{c: c, ctx: ctx, vmid: vmid, pool: pool, devices: devices, pending: len(devices)}
	records := []ProviderRecord{{File: ProviderManifestFile, Reader: &manifestReader{v: v}}}
	for _, device := range devices {
		records = append(records, ProviderRecord{File: ProviderDiskFile(device), Reader: &diskReader{v: v, device: device}})
	}
	return records
}

type providerVM struct {
	c       *Client
	ctx     context.Context
	vmid    int
	pool    string
	devices []string

	once     sync.Once
	manifest []byte
	startErr error

	mu      sync.Mutex
	b       *providerBackup
	pending int
	err     error
}

func (v *providerVM) start() error {
	v.once.Do(func() {
		b, err := v.c.startProviderBackup(v.ctx, v.vmid)
		if err != nil {
			v.startErr = err
			return
		}
		b.Manifest.Pool = v.pool
		if got := b.Manifest.DeviceNames(); !slices.Equal(got, v.devices) {
			err = fmt.Errorf("vzdump backs up disks %v but the VM config lists %v", got, v.devices)
		}
		if err == nil {
			v.manifest, err = b.Manifest.Stored()
		}
		if err != nil {
			v.startErr = b.finish(err)
			return
		}
		v.mu.Lock()
		v.b = b
		allClosed := v.pending == 0
		v.mu.Unlock()
		if allClosed {
			v.startErr = b.finish(errors.New("every disk record was closed unread"))
		}
	})
	return v.startErr
}

func (v *providerVM) release(err error) error {
	v.mu.Lock()
	if err != nil && v.err == nil {
		v.err = err
	}
	v.pending--
	b, last, sessionErr := v.b, v.pending == 0, v.err
	v.mu.Unlock()

	if !last || b == nil {
		return err
	}
	return b.finish(sessionErr)
}

type manifestReader struct {
	v  *providerVM
	rd *bytes.Reader
}

func (m *manifestReader) Read(p []byte) (int, error) {
	if m.rd == nil {
		if err := m.v.start(); err != nil {
			return 0, err
		}
		m.rd = bytes.NewReader(m.v.manifest)
	}
	return m.rd.Read(p)
}

func (m *manifestReader) Close() error { return nil }

type diskReader struct {
	v      *providerVM
	device string

	open, close sync.Once
	opened      bool
	pr          *io.PipeReader
	done        chan struct{}
}

func (d *diskReader) start() {
	d.opened = true
	pr, pw := io.Pipe()
	d.pr, d.done = pr, make(chan struct{})
	go func() {
		defer close(d.done)
		err := d.v.start()
		if err == nil {
			err = d.copy(pw)
		}
		_ = pw.CloseWithError(d.v.release(err))
	}()
}

func (d *diskReader) copy(w io.Writer) error {
	b := d.v.b
	dev := b.Manifest.Devices[d.device]
	conn, err := b.c.runner.DialUnix(dev.NBDPath)
	if err != nil {
		return fmt.Errorf("%s: %w", d.device, err)
	}
	defer func() { _ = conn.Close() }()
	return nbd.Copy(&progressWriter{w: w, provider: b.progress, device: d.device, total: dev.Size}, conn, d.device, dev.Size)
}

func (d *diskReader) Read(p []byte) (int, error) {
	d.open.Do(d.start)
	return d.pr.Read(p)
}

func (d *diskReader) Close() error {
	d.close.Do(func() {
		d.open.Do(func() {})
		if !d.opened {
			_ = d.v.release(fmt.Errorf("%s: disk was never read", d.device))
			return
		}
		_ = d.pr.Close()
		<-d.done
	})
	return nil
}

type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

type providerBackup struct {
	Manifest ProviderManifest

	c        *Client
	sockPath string
	ln       net.Listener
	conn     net.Conn
	progress io.Writer

	vzdumpDone chan struct{}
	vzdumpErr  error
}

func (c *Client) fleecingStorage(ctx context.Context, vmid int) string {
	if c.cfg.FleecingStorage != "" || !c.fleecingOnBootDisk {
		return c.cfg.FleecingStorage
	}
	conf, err := c.ReadQEMUConfig(ctx, vmid)
	if err != nil {
		return ""
	}
	return StorageFromConfig("qemu", conf)
}

func (c *Client) startProviderBackup(ctx context.Context, vmid int) (*providerBackup, error) {
	session, err := randomToken()
	if err != nil {
		return nil, err
	}
	sockPath := path.Join(ProviderSessionRoot, session+".sock")

	c.vzdumpMu.Lock()
	if err := ctx.Err(); err != nil {
		c.vzdumpMu.Unlock()
		return nil, err
	}
	ln, err := c.runner.ListenUnix(sockPath)
	if err != nil {
		c.vzdumpMu.Unlock()
		return nil, fmt.Errorf("unable to listen on %s (is plakar-pve-provider installed?): %w", sockPath, err)
	}

	args := []string{"PLAKAR_SESSION=" + session, "vzdump", strconv.Itoa(vmid),
		"--storage", ProviderStorage, "--mode", c.cfg.BackupMode}
	if storage := c.fleecingStorage(ctx, vmid); storage != "" {
		args = append(args, "--fleecing", "enabled=1,storage="+storage)
	}

	stream, err := c.runner.Stream(ctx, "env", args...)
	if err != nil {
		_ = ln.Close()
		_ = c.runner.Remove(context.Background(), sockPath)
		c.vzdumpMu.Unlock()
		return nil, fmt.Errorf("vzdump start failed: %w", err)
	}

	b := &providerBackup{c: c, sockPath: sockPath, ln: ln, vzdumpDone: make(chan struct{})}
	go b.waitVzdump(stream)

	conn, err := ln.Accept()
	if err != nil {
		_ = b.finish(err)
		if b.vzdumpErr != nil {
			return nil, b.vzdumpErr
		}
		return nil, fmt.Errorf("provider never connected: %w", err)
	}
	b.conn = conn
	b.progress = &lockedWriter{w: conn}

	line, err := bufio.NewReader(io.LimitReader(conn, MaxProviderManifest)).ReadBytes('\n')
	if err == nil {
		err = json.Unmarshal(line, &b.Manifest)
	}
	if err == nil {
		err = b.Manifest.Validate()
	}
	if err != nil {
		return nil, b.finish(fmt.Errorf("invalid manifest from provider: %w", err))
	}
	return b, nil
}

func (b *providerBackup) waitVzdump(stream *runner.CommandStream) {
	var stdout, stderr bytes.Buffer
	var copies sync.WaitGroup
	copies.Add(2)
	go func() { defer copies.Done(); _, _ = io.Copy(&stdout, stream.Stdout) }()
	go func() { defer copies.Done(); _, _ = io.Copy(&stderr, stream.Stderr) }()
	copies.Wait()

	if err := stream.Finish(); err != nil {
		b.vzdumpErr = fmt.Errorf("vzdump failed: %w: %s", err, lastLines(stderr.String()+stdout.String(), 10))
	}
	_ = b.ln.Close()
	close(b.vzdumpDone)
}

// The provider logs these lines in the vzdump task; a lost line only costs a log entry.
type progressWriter struct {
	w        io.Writer
	provider io.Writer
	device   string
	total    int64
	read     int64
	next     int64
}

func (p *progressWriter) Write(b []byte) (int, error) {
	n, err := p.w.Write(b)
	p.read += int64(n)
	if p.total > 0 && p.read*100 >= p.next*p.total {
		pct := p.read * 100 / p.total
		p.next = pct - pct%5 + 5
		if pct > 0 {
			line, _ := json.Marshal(map[string]any{"progress": map[string]any{"device": p.device, "read": p.read, "total": p.total}})
			_, _ = p.provider.Write(append(line, '\n'))
		}
	}
	return n, err
}

func (b *providerBackup) finish(err error) error {
	if b.conn != nil {
		reply := map[string]string{"status": "ok"}
		if err != nil {
			reply = map[string]string{"status": "error", "message": err.Error()}
		}
		line, _ := json.Marshal(reply)
		_, _ = b.conn.Write(append(line, '\n'))
		_ = b.conn.Close()
	}

	<-b.vzdumpDone
	_ = b.ln.Close()
	_ = b.c.runner.Remove(context.Background(), b.sockPath)
	b.c.vzdumpMu.Unlock()

	if err != nil {
		return err
	}
	return b.vzdumpErr
}

func ParseProviderManifest(data []byte) (ProviderManifest, error) {
	var m ProviderManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return m, fmt.Errorf("invalid %s: %w", ProviderManifestFile, err)
	}
	return m, m.Validate()
}

type ProviderRestore struct {
	Volume   string
	Manifest ProviderManifest

	c      *Client
	dir    string
	images []string
	served map[string]bool
}

func (c *Client) PrepareProviderRestore(ctx context.Context, m ProviderManifest) (*ProviderRestore, error) {
	stamp := time.Now().Unix()
	r := &ProviderRestore{
		c:        c,
		Manifest: m,
		Volume:   fmt.Sprintf("%s:%d/qemu-%d", ProviderStorage, m.VMID, stamp),
		dir:      path.Join(ProviderSessionRoot, ProviderStorage, "restore", fmt.Sprintf("%d-qemu-%d", m.VMID, stamp)),
		served:   make(map[string]bool),
	}
	if _, stderr, err := c.runner.Run(ctx, "mkdir", "-p", "-m", "0700", "--", r.dir); err != nil {
		return nil, fmt.Errorf("unable to create restore session %s: %w: %s", r.dir, err, strings.TrimSpace(stderr))
	}
	return r, nil
}

func (r *ProviderRestore) Complete() bool {
	return len(r.served) == len(r.Manifest.Devices)
}

func (r *ProviderRestore) AddDisk(ctx context.Context, device string, size int64, data io.Reader) error {
	if dev, listed := r.Manifest.Devices[device]; !listed || r.served[device] || dev.Size != size {
		return fmt.Errorf("unexpected disk %s for VM %d (not in its manifest, repeated or wrong size)", device, r.Manifest.VMID)
	}
	image := path.Join(r.c.cfg.DumpDir, fmt.Sprintf("plakar-%d-%s%s", time.Now().UnixNano(), device, ProviderDiskSuffix))
	r.images = append(r.images, image)
	if err := r.writeFile(ctx, image, data); err != nil {
		return fmt.Errorf("unable to upload %s to %s: %w", device, image, err)
	}

	dev := r.Manifest.Devices[device]
	dev.Image = image
	r.Manifest.Devices[device] = dev
	r.served[device] = true
	return nil
}

func (r *ProviderRestore) Finish(ctx context.Context) error {
	if !r.Complete() {
		return fmt.Errorf("incomplete backup of VM %d: %d of %d disks", r.Manifest.VMID, len(r.served), len(r.Manifest.Devices))
	}
	files := map[string][]byte{ProviderGuestFile: []byte(r.Manifest.GuestConfig)}
	if r.Manifest.FirewallConfig != nil {
		files[ProviderFirewallFile] = []byte(*r.Manifest.FirewallConfig)
	}
	var err error
	files[ProviderManifestFile], err = r.Manifest.Stored()
	if err != nil {
		return err
	}
	for _, name := range []string{ProviderGuestFile, ProviderFirewallFile, ProviderManifestFile} {
		if data, ok := files[name]; ok {
			if err := r.writeFile(ctx, path.Join(r.dir, name), bytes.NewReader(data)); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *ProviderRestore) writeFile(ctx context.Context, filepath string, data io.Reader) error {
	w, err := r.c.runner.Create(ctx, filepath)
	if err != nil {
		return err
	}
	if _, err := io.Copy(w, data); err != nil {
		_ = w.Close()
		return err
	}
	return w.Close()
}

func (r *ProviderRestore) Close() {
	ctx := context.Background()
	if r.dir != "" {
		_, _, _ = r.c.runner.Run(ctx, "rm", "-rf", "--", r.dir)
	}
	for _, image := range r.images {
		if err := r.c.runner.Remove(ctx, image); err != nil {
			log.Printf("unable to remove temporary disk %s: %v", image, err)
		}
	}
}

func randomToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
