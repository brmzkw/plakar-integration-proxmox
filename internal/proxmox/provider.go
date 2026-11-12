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
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PlakarKorp/kloset/connectors"
	"github.com/gillesdubois/plakar-integration-proxmox/internal/nbd"
	"github.com/gillesdubois/plakar-integration-proxmox/internal/runner"
)

const ProviderManifestVersion = 2

const (
	ProviderManifestFile = "manifest.json"
	ProviderGuestFile    = "guest.conf"
	ProviderFirewallFile = "firewall.conf"
	ProviderDiskSuffix   = ".raw"

	maxProviderManifest = 4 << 20
)

var providerDeviceRegex = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9_.-]*$`)

var ProviderArchiveRegex = regexp.MustCompile(`^qemu-\d+\.tar$`)

func ProviderArchiveName(t time.Time) string {
	return fmt.Sprintf("qemu-%d.tar", t.Unix())
}

type ProviderDevice struct {
	Size    int64  `json:"size"`
	NBDPath string `json:"nbd-path,omitempty"`
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
		stored.Devices[name] = ProviderDevice{Size: dev.Size}
	}
	return json.MarshalIndent(&stored, "", "  ")
}

func (c *Client) ProviderBackupStream(ctx context.Context, vmid int, pool string) io.ReadCloser {
	return connectors.NewLazyReader(func() (io.ReadCloser, error) {
		b, err := c.startProviderBackup(ctx, vmid)
		if err != nil {
			return nil, err
		}
		b.Manifest.Pool = pool

		pr, pw := io.Pipe()
		done := make(chan struct{})
		go func() {
			defer close(done)
			_ = pw.CloseWithError(b.finish(b.writeTar(pw)))
		}()
		return &providerStream{PipeReader: pr, done: done}, nil
	})
}

type providerStream struct {
	*io.PipeReader
	done chan struct{}
}

func (s *providerStream) Close() error {
	_ = s.PipeReader.Close()
	<-s.done
	return nil
}

type providerBackup struct {
	Manifest ProviderManifest

	c        *Client
	sockPath string
	ln       net.Listener
	conn     net.Conn

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

	line, err := bufio.NewReader(io.LimitReader(conn, maxProviderManifest)).ReadBytes('\n')
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

func (b *providerBackup) writeTar(w io.Writer) error {
	manifest, err := b.Manifest.Stored()
	if err != nil {
		return err
	}

	tw := tar.NewWriter(w)
	now := time.Now()
	header := func(name string, size int64) error {
		return tw.WriteHeader(&tar.Header{Name: name, Size: size, Mode: 0600, ModTime: now, Typeflag: tar.TypeReg})
	}

	if err := header(ProviderManifestFile, int64(len(manifest))); err != nil {
		return err
	}
	if _, err := tw.Write(manifest); err != nil {
		return err
	}

	for _, device := range b.Manifest.DeviceNames() {
		dev := b.Manifest.Devices[device]
		if err := header(device+ProviderDiskSuffix, dev.Size); err != nil {
			return err
		}
		conn, err := b.c.runner.DialUnix(dev.NBDPath)
		if err != nil {
			return fmt.Errorf("%s: %w", device, err)
		}
		err = nbd.Copy(tw, conn, device, dev.Size)
		_ = conn.Close()
		if err != nil {
			return err
		}
	}
	return tw.Close()
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

type ProviderRestore struct {
	Volume   string
	Manifest ProviderManifest

	c        *Client
	dir      string
	images   []string
	pidFiles []string
}

func (c *Client) PrepareProviderRestore(ctx context.Context, archive io.Reader) (*ProviderRestore, error) {
	r := &ProviderRestore{c: c}
	if err := r.prepare(ctx, archive); err != nil {
		r.Close()
		return nil, err
	}
	return r, nil
}

func (r *ProviderRestore) prepare(ctx context.Context, archive io.Reader) error {
	tr := tar.NewReader(archive)

	hdr, err := tr.Next()
	if err != nil {
		return fmt.Errorf("invalid provider archive: %w", err)
	}
	if hdr.Name != ProviderManifestFile || hdr.Size > maxProviderManifest {
		return fmt.Errorf("invalid provider archive: %s must come first", ProviderManifestFile)
	}
	data, err := io.ReadAll(tr)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, &r.Manifest); err != nil {
		return fmt.Errorf("invalid %s: %w", ProviderManifestFile, err)
	}
	if err := r.Manifest.Validate(); err != nil {
		return err
	}

	stamp := time.Now().Unix()
	r.Volume = fmt.Sprintf("%s:%d/qemu-%d", ProviderStorage, r.Manifest.VMID, stamp)
	r.dir = path.Join(ProviderSessionRoot, ProviderStorage, "restore", fmt.Sprintf("%d-qemu-%d", r.Manifest.VMID, stamp))
	if _, stderr, err := r.c.runner.Run(ctx, "mkdir", "-p", "-m", "0700", "--", r.dir); err != nil {
		return fmt.Errorf("unable to create restore session %s: %w: %s", r.dir, err, strings.TrimSpace(stderr))
	}

	served := make(map[string]bool)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("invalid provider archive: %w", err)
		}
		device, ok := strings.CutSuffix(hdr.Name, ProviderDiskSuffix)
		if dev, listed := r.Manifest.Devices[device]; !ok || !listed || served[device] || dev.Size != hdr.Size {
			return fmt.Errorf("invalid provider archive: unexpected entry %s", hdr.Name)
		}
		if err := r.serveDisk(ctx, device, tr); err != nil {
			return err
		}
		served[device] = true
	}
	if len(served) != len(r.Manifest.Devices) {
		return fmt.Errorf("invalid provider archive: %d of %d disks", len(served), len(r.Manifest.Devices))
	}

	files := map[string][]byte{ProviderGuestFile: []byte(r.Manifest.GuestConfig)}
	if r.Manifest.FirewallConfig != nil {
		files[ProviderFirewallFile] = []byte(*r.Manifest.FirewallConfig)
	}
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

func (r *ProviderRestore) serveDisk(ctx context.Context, device string, data io.Reader) error {
	image := path.Join(r.c.cfg.DumpDir, fmt.Sprintf("plakar-%d-%s%s", time.Now().UnixNano(), device, ProviderDiskSuffix))
	r.images = append(r.images, image)
	if err := r.writeFile(ctx, image, data); err != nil {
		return fmt.Errorf("unable to upload %s to %s: %w", device, image, err)
	}

	pidFile := image + ".pid"
	r.pidFiles = append(r.pidFiles, pidFile)
	_, stderr, err := r.c.runner.Run(ctx, "qemu-nbd", "--fork", "--persistent", "--read-only", "--format", "raw",
		"--export-name", device, "--pid-file", pidFile, "--socket", path.Join(r.dir, device+".sock"), image)
	if err != nil {
		return fmt.Errorf("unable to serve device %s: %w: %s", device, err, strings.TrimSpace(stderr))
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
	for _, pidFile := range r.pidFiles {
		_, _, _ = r.c.runner.Run(ctx, "pkill", "-F", pidFile)
		_ = r.c.runner.Remove(ctx, pidFile)
	}
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
