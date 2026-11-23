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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gillesdubois/plakar-integration-proxmox/internal/nbd/nbdtest"
	"github.com/gillesdubois/plakar-integration-proxmox/internal/runner"
)

type fakeProviderRunner struct {
	runner.Local
	root, nbdPath string
	size          int64
	reply         chan string
	progress      []string
}

func (r *fakeProviderRunner) ListenUnix(socketPath string) (net.Listener, error) {
	return net.Listen("unix", filepath.Join(r.root, filepath.Base(socketPath)))
}

func (r *fakeProviderRunner) Remove(_ context.Context, socketPath string) error {
	return os.Remove(filepath.Join(r.root, filepath.Base(socketPath)))
}

func (r *fakeProviderRunner) Stream(_ context.Context, name string, args ...string) (*runner.CommandStream, error) {
	session := strings.TrimPrefix(args[0], "PLAKAR_SESSION=")
	errCh := make(chan error, 1)

	go func() {
		conn, err := net.Dial("unix", filepath.Join(r.root, session+".sock"))
		if err != nil {
			errCh <- err
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = fmt.Fprintf(conn, `{"version":1,"vmid":100,"archive":"100/qemu-1","guest-config":"cores: 1\n","firewall-config":null,"devices":{"drive-scsi0":{"size":%d,"nbd-path":%q,"export":"drive-scsi0"}}}`+"\n", r.size, r.nbdPath)
		br := bufio.NewReader(conn)
		var line string
		for {
			line, err = br.ReadString('\n')
			if err != nil || !strings.HasPrefix(line, `{"progress"`) {
				break
			}
			r.progress = append(r.progress, line)
		}
		r.reply <- line
		if err == nil && !strings.Contains(line, `"ok"`) {
			err = errors.New("provider got an error reply")
		}
		errCh <- err
	}()

	return runner.NewCommandStream(strings.NewReader(""), strings.NewReader(""), func() error { return <-errCh }, nil), nil
}

func newBackupFixture(t *testing.T, data []byte) (*Client, *fakeProviderRunner) {
	runner := &fakeProviderRunner{
		root:    t.TempDir(),
		nbdPath: (&nbdtest.Server{Data: data, Structured: true}).Listen(t),
		size:    int64(len(data)),
		reply:   make(chan string, 1),
	}
	return &Client{cfg: &Config{BackupMode: "snapshot"}, runner: runner}, runner
}

func readAll(t *testing.T, r io.ReadCloser) []byte {
	t.Helper()
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Close()
	return data
}

func TestProviderBackup(t *testing.T) {
	data := bytes.Repeat([]byte("plakar\x00\x00"), 300000)
	client, runner := newBackupFixture(t, data)

	recs := client.ProviderBackup(context.Background(), 100, "prod", []string{"drive-scsi0"})
	if len(recs) != 2 || recs[0].File != ProviderManifestFile || recs[1].File != "vm-drive-scsi0.raw" {
		t.Fatalf("records %+v", recs)
	}

	var m ProviderManifest
	if err := json.Unmarshal(readAll(t, recs[0].Reader), &m); err != nil || m.Pool != "prod" || m.GuestConfig != "cores: 1\n" {
		t.Fatalf("manifest %+v, %v", m, err)
	}
	if m.Devices["drive-scsi0"].NBDPath != "" {
		t.Error("node socket path must not be stored")
	}
	if disk := readAll(t, recs[1].Reader); !bytes.Equal(disk, data) {
		t.Fatal("disk content differs")
	}

	if reply := <-runner.reply; !strings.Contains(reply, `"ok"`) {
		t.Fatalf("provider reply %q", reply)
	}
	if want := fmt.Sprintf(`{"progress":{"device":"drive-scsi0","read":%d,"total":%d}}`+"\n", len(data), len(data)); len(runner.progress) != 1 || runner.progress[0] != want {
		t.Fatalf("progress lines %q, want %q", runner.progress, want)
	}
	if left, _ := filepath.Glob(filepath.Join(runner.root, "*.sock")); len(left) != 0 {
		t.Fatalf("session socket left behind: %v", left)
	}
	if !client.vzdumpMu.TryLock() {
		t.Fatal("vzdump lock still held")
	}
}

func TestProviderBackupDiskReadFirst(t *testing.T) {
	data := []byte("disk")
	client, runner := newBackupFixture(t, data)

	recs := client.ProviderBackup(context.Background(), 100, "", []string{"drive-scsi0"})
	if disk := readAll(t, recs[1].Reader); !bytes.Equal(disk, data) {
		t.Fatal("disk content differs")
	}
	if !strings.Contains(<-runner.reply, `"ok"`) {
		t.Fatal("backup must succeed")
	}
	if len(readAll(t, recs[0].Reader)) == 0 {
		t.Fatal("manifest must still be readable after the session ended")
	}
}

func TestProviderBackupNothingRunsUnread(t *testing.T) {
	client, runner := newBackupFixture(t, []byte("disk"))

	for _, rec := range client.ProviderBackup(context.Background(), 100, "", []string{"drive-scsi0"}) {
		_ = rec.Reader.Close()
	}
	select {
	case reply := <-runner.reply:
		t.Fatalf("vzdump must not run, provider got %q", reply)
	default:
	}
	if !client.vzdumpMu.TryLock() {
		t.Fatal("vzdump lock taken")
	}
}

func TestProviderBackupDisksClosedBeforeManifest(t *testing.T) {
	client, runner := newBackupFixture(t, []byte("disk"))

	recs := client.ProviderBackup(context.Background(), 100, "", []string{"drive-scsi0"})
	_ = recs[1].Reader.Close()
	if _, err := io.ReadAll(recs[0].Reader); err == nil {
		t.Fatal("a session with no disk read must fail")
	}
	if !strings.Contains(<-runner.reply, `"error"`) || !client.vzdumpMu.TryLock() {
		t.Fatal("the session must be ended with an error")
	}
}

func TestProviderBackupClosedEarly(t *testing.T) {
	client, runner := newBackupFixture(t, bytes.Repeat([]byte{1}, 3*4<<20))

	recs := client.ProviderBackup(context.Background(), 100, "", []string{"drive-scsi0"})
	if _, err := recs[1].Reader.Read(make([]byte, 10)); err != nil {
		t.Fatal(err)
	}
	_ = recs[1].Reader.Close()

	if reply := <-runner.reply; !strings.Contains(reply, `"error"`) {
		t.Fatalf("an unfinished backup must be reported to the provider, got %q", reply)
	}
	if !client.vzdumpMu.TryLock() {
		t.Fatal("vzdump lock still held")
	}
}

func TestProviderBackupDeviceMismatch(t *testing.T) {
	client, runner := newBackupFixture(t, []byte("disk"))

	recs := client.ProviderBackup(context.Background(), 100, "", []string{"drive-scsi1"})
	_, err := io.ReadAll(recs[0].Reader)
	if err == nil || !strings.Contains(err.Error(), "VM config lists") {
		t.Fatalf("got %v", err)
	}
	if _, err := io.ReadAll(recs[1].Reader); err == nil {
		t.Fatal("disk read must fail too")
	}
	_ = recs[1].Reader.Close()
	if !strings.Contains(<-runner.reply, `"error"`) || !client.vzdumpMu.TryLock() {
		t.Fatal("the session must be ended with an error")
	}
}

type failingVzdumpRunner struct{ fakeProviderRunner }

func (r *failingVzdumpRunner) Stream(context.Context, string, ...string) (*runner.CommandStream, error) {
	return runner.NewCommandStream(strings.NewReader(""), strings.NewReader("VM 100 is locked (backup)\n"),
		func() error { return errors.New("exit status 255") }, nil), nil
}

func TestProviderBackupReportsVzdumpFailure(t *testing.T) {
	client := &Client{cfg: &Config{BackupMode: "snapshot"}, runner: &failingVzdumpRunner{fakeProviderRunner{root: t.TempDir()}}}

	recs := client.ProviderBackup(context.Background(), 100, "", []string{"drive-scsi0"})
	_, err := io.ReadAll(recs[0].Reader)
	if err == nil || !strings.Contains(err.Error(), "VM 100 is locked") {
		t.Fatalf("expected vzdump's stderr in the error, got %v", err)
	}
	_ = recs[1].Reader.Close()
	if !client.vzdumpMu.TryLock() {
		t.Fatal("vzdump lock still held")
	}
}

type recordingRunner struct {
	runner.Local
	commands []string
	files    map[string]*bytes.Buffer
	order    []string
	removed  []string
}

type nopWriteCloser struct{ *bytes.Buffer }

func (nopWriteCloser) Close() error { return nil }

func (r *recordingRunner) Run(_ context.Context, name string, args ...string) (string, string, error) {
	r.commands = append(r.commands, strings.Join(append([]string{name}, args...), " "))
	return "", "", nil
}

func (r *recordingRunner) Create(_ context.Context, path string) (io.WriteCloser, error) {
	r.files[path] = &bytes.Buffer{}
	r.order = append(r.order, path)
	return nopWriteCloser{r.files[path]}, nil
}

func (r *recordingRunner) Remove(_ context.Context, path string) error {
	r.removed = append(r.removed, path)
	return nil
}

func TestProviderRestore(t *testing.T) {
	runner := &recordingRunner{files: map[string]*bytes.Buffer{}}
	client := &Client{cfg: &Config{DumpDir: "/var/lib/vz/dump"}, runner: runner}
	m, err := ParseProviderManifest([]byte(`{"version":1,"vmid":100,"archive":"100/qemu-1","guest-config":"cores: 1\n","firewall-config":null,"devices":{"scsi0":{"size":4}},"pool":"prod"}`))
	if err != nil {
		t.Fatal(err)
	}

	r, err := client.PrepareProviderRestore(context.Background(), m)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.AddDisk(context.Background(), "scsi0", 4, strings.NewReader("disk")); err != nil || !r.Complete() {
		t.Fatalf("AddDisk: %v, complete %v", err, r.Complete())
	}
	if err := r.Finish(context.Background()); err != nil {
		t.Fatal(err)
	}

	if !strings.HasPrefix(r.Volume, "plakar:100/qemu-") || r.Manifest.Pool != "prod" {
		t.Fatalf("volume %q pool %q", r.Volume, r.Manifest.Pool)
	}
	session := "/run/plakar/plakar/restore/100-qemu-" + strings.TrimPrefix(r.Volume, "plakar:100/qemu-")

	image := r.images[0]
	if !strings.HasPrefix(image, "/var/lib/vz/dump/plakar-") || runner.files[image].String() != "disk" {
		t.Fatalf("image %s not uploaded to dump_dir", image)
	}
	if len(runner.commands) != 1 || !strings.HasPrefix(runner.commands[0], "mkdir ") {
		t.Fatalf("commands %q, want only mkdir", runner.commands)
	}
	if !strings.Contains(runner.files[session+"/manifest.json"].String(), `"image": "`+image+`"`) {
		t.Fatalf("manifest.json must give the image path: %s", runner.files[session+"/manifest.json"])
	}
	if last := runner.order[len(runner.order)-1]; last != session+"/manifest.json" {
		t.Fatalf("manifest.json must be written last, got %s", last)
	}
	if runner.files[session+"/guest.conf"].String() != "cores: 1\n" {
		t.Fatal("guest.conf not written")
	}

	r.Close()
	cleanup := strings.Join(runner.commands, "\n")
	if !strings.Contains(cleanup, "rm -rf -- "+session) {
		t.Fatalf("cleanup commands %q", runner.commands)
	}
	if strings.Join(runner.removed, ",") != image {
		t.Fatalf("removed %v", runner.removed)
	}
}

func TestProviderRestoreRejectsBadDisks(t *testing.T) {
	runner := &recordingRunner{files: map[string]*bytes.Buffer{}}
	client := &Client{cfg: &Config{DumpDir: "/dump"}, runner: runner}
	m, _ := ParseProviderManifest([]byte(`{"version":1,"vmid":100,"devices":{"scsi0":{"size":4}}}`))

	r, err := client.PrepareProviderRestore(context.Background(), m)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.AddDisk(context.Background(), "scsi1", 4, strings.NewReader("disk")); err == nil {
		t.Fatal("an unlisted disk must be refused")
	}
	if err := r.AddDisk(context.Background(), "scsi0", 5, strings.NewReader("disk!")); err == nil {
		t.Fatal("a disk of the wrong size must be refused")
	}
	if len(runner.files) != 0 {
		t.Fatal("nothing must be uploaded for a refused disk")
	}
	if err := r.Finish(context.Background()); err == nil || !strings.Contains(err.Error(), "0 of 1 disks") {
		t.Fatalf("an incomplete restore must fail, got %v", err)
	}
}

func TestProviderDiskFileSortsAfterManifest(t *testing.T) {
	for _, device := range []string{"drive-efidisk0", "drive-scsi0", "-x", "0", "A", "_x"} {
		name := ProviderDiskFile(device)
		if name <= ProviderManifestFile {
			t.Errorf("%s sorts before %s", name, ProviderManifestFile)
		}
		if got, ok := ParseProviderDiskFile(name); !ok || got != device {
			t.Errorf("%s: parsed %q %v", name, got, ok)
		}
	}
	for _, name := range []string{ProviderManifestFile, "drive-scsi0.raw", "vm-.raw", "vm-a/b.raw", "vm-.x.raw"} {
		if _, ok := ParseProviderDiskFile(name); ok {
			t.Errorf("%s must not parse as a disk", name)
		}
	}
}

type configRunner struct {
	runner.Local
	conf string
}

func (r *configRunner) Open(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(r.conf)), nil
}

func TestFleecingStorage(t *testing.T) {
	r := &configRunner{conf: "boot: order=scsi0\nscsi0: local-zfs:vm-100-disk-0,size=8G\n"}
	for name, tc := range map[string]struct {
		client *Client
		want   string
	}{
		"option wins":            {&Client{cfg: &Config{FleecingStorage: "fast"}, runner: r, fleecingOnBootDisk: true}, "fast"},
		"vzdump.conf left alone": {&Client{cfg: &Config{}, runner: r}, ""},
		"boot disk by default":   {&Client{cfg: &Config{}, runner: r, fleecingOnBootDisk: true}, "local-zfs"},
	} {
		if got := tc.client.fleecingStorage(context.Background(), 100); got != tc.want {
			t.Errorf("%s: got %q, want %q", name, got, tc.want)
		}
	}
}

func TestProgressWriterEveryFivePercent(t *testing.T) {
	var disk, lines bytes.Buffer
	p := &progressWriter{w: &disk, provider: &lines, device: "drive-scsi0", total: 1000}
	for range 1000 {
		_, _ = p.Write([]byte{1})
	}
	got := strings.Split(strings.TrimSpace(lines.String()), "\n")
	if len(got) != 20 || disk.Len() != 1000 {
		t.Fatalf("%d lines for %d bytes: %q", len(got), disk.Len(), got)
	}
	if got[0] != `{"progress":{"device":"drive-scsi0","read":50,"total":1000}}` || got[19] != `{"progress":{"device":"drive-scsi0","read":1000,"total":1000}}` {
		t.Fatalf("lines %q ... %q", got[0], got[19])
	}
}
