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
		_, _ = fmt.Fprintf(conn, `{"version":2,"vmid":100,"archive":"100/qemu-1","guest-config":"cores: 1\n","firewall-config":null,"devices":{"drive-scsi0":{"size":%d,"nbd-path":%q,"export":"drive-scsi0"}}}`+"\n", r.size, r.nbdPath)
		line, err := bufio.NewReader(conn).ReadString('\n')
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

func TestProviderBackupStream(t *testing.T) {
	data := bytes.Repeat([]byte("plakar\x00\x00"), 300000)
	client, runner := newBackupFixture(t, data)

	stream := client.ProviderBackupStream(context.Background(), 100, "prod")
	archive, err := io.ReadAll(stream)
	if err != nil {
		t.Fatal(err)
	}
	_ = stream.Close()

	tr := tar.NewReader(bytes.NewReader(archive))
	hdr, err := tr.Next()
	if err != nil || hdr.Name != ProviderManifestFile {
		t.Fatalf("first entry %v, %v", hdr, err)
	}
	var m ProviderManifest
	if err := json.NewDecoder(tr).Decode(&m); err != nil || m.Pool != "prod" || m.GuestConfig != "cores: 1\n" {
		t.Fatalf("manifest %+v, %v", m, err)
	}
	if m.Devices["drive-scsi0"].NBDPath != "" {
		t.Error("node socket path must not be stored")
	}
	hdr, err = tr.Next()
	if err != nil || hdr.Name != "drive-scsi0.raw" {
		t.Fatalf("second entry %v, %v", hdr, err)
	}
	if disk, _ := io.ReadAll(tr); !bytes.Equal(disk, data) {
		t.Fatal("disk content differs")
	}

	if reply := <-runner.reply; !strings.Contains(reply, `"ok"`) {
		t.Fatalf("provider reply %q", reply)
	}
	if left, _ := filepath.Glob(filepath.Join(runner.root, "*.sock")); len(left) != 0 {
		t.Fatalf("session socket left behind: %v", left)
	}
	if !client.vzdumpMu.TryLock() {
		t.Fatal("vzdump lock still held")
	}
}

func TestProviderBackupStreamClosedEarly(t *testing.T) {
	client, runner := newBackupFixture(t, bytes.Repeat([]byte{1}, 3*4<<20))

	stream := client.ProviderBackupStream(context.Background(), 100, "")
	if _, err := stream.Read(make([]byte, 10)); err != nil {
		t.Fatal(err)
	}
	_ = stream.Close()

	if reply := <-runner.reply; !strings.Contains(reply, `"error"`) {
		t.Fatalf("an unfinished backup must be reported to the provider, got %q", reply)
	}
	if !client.vzdumpMu.TryLock() {
		t.Fatal("vzdump lock still held")
	}
}

type failingVzdumpRunner struct{ fakeProviderRunner }

func (r *failingVzdumpRunner) Stream(context.Context, string, ...string) (*runner.CommandStream, error) {
	return runner.NewCommandStream(strings.NewReader(""), strings.NewReader("VM 100 is locked (backup)\n"),
		func() error { return errors.New("exit status 255") }, nil), nil
}

func TestProviderBackupReportsVzdumpFailure(t *testing.T) {
	client := &Client{cfg: &Config{BackupMode: "snapshot"}, runner: &failingVzdumpRunner{fakeProviderRunner{root: t.TempDir()}}}

	_, err := io.ReadAll(client.ProviderBackupStream(context.Background(), 100, ""))
	if err == nil || !strings.Contains(err.Error(), "VM 100 is locked") {
		t.Fatalf("expected vzdump's stderr in the error, got %v", err)
	}
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

func providerArchive(t *testing.T, manifest string, disks map[string][]byte) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	add := func(name string, data []byte) {
		if err := tw.WriteHeader(&tar.Header{Name: name, Size: int64(len(data)), Mode: 0600}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write(data)
	}
	add(ProviderManifestFile, []byte(manifest))
	for name, data := range disks {
		add(name, data)
	}
	_ = tw.Close()
	return buf.Bytes()
}

func TestProviderRestore(t *testing.T) {
	runner := &recordingRunner{files: map[string]*bytes.Buffer{}}
	client := &Client{cfg: &Config{DumpDir: "/var/lib/vz/dump"}, runner: runner}
	manifest := `{"version":2,"vmid":100,"archive":"100/qemu-1","guest-config":"cores: 1\n","firewall-config":null,"devices":{"scsi0":{"size":4}},"pool":"prod"}`

	r, err := client.PrepareProviderRestore(context.Background(),
		bytes.NewReader(providerArchive(t, manifest, map[string][]byte{"scsi0.raw": []byte("disk")})))
	if err != nil {
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
	want := strings.Join([]string{"qemu-nbd", "--fork", "--persistent", "--read-only", "--format", "raw", "--export-name", "scsi0",
		"--pid-file", image + ".pid", "--socket", session + "/scsi0.sock", image}, " ")
	if len(runner.commands) != 2 || runner.commands[1] != want {
		t.Fatalf("commands %q, want qemu-nbd %q", runner.commands, want)
	}
	if last := runner.order[len(runner.order)-1]; last != session+"/manifest.json" {
		t.Fatalf("manifest.json must be written last, got %s", last)
	}
	if runner.files[session+"/guest.conf"].String() != "cores: 1\n" {
		t.Fatal("guest.conf not written")
	}

	r.Close()
	cleanup := strings.Join(runner.commands, "\n")
	if !strings.Contains(cleanup, "pkill -F "+image+".pid") || !strings.Contains(cleanup, "rm -rf -- "+session) {
		t.Fatalf("cleanup commands %q", runner.commands)
	}
	if strings.Join(runner.removed, ",") != image+".pid,"+image {
		t.Fatalf("removed %v", runner.removed)
	}
}

func TestProviderRestoreRejectsUnlistedDisk(t *testing.T) {
	runner := &recordingRunner{files: map[string]*bytes.Buffer{}}
	client := &Client{cfg: &Config{DumpDir: "/dump"}, runner: runner}
	manifest := `{"version":2,"vmid":100,"devices":{"scsi0":{"size":4}}}`

	_, err := client.PrepareProviderRestore(context.Background(),
		bytes.NewReader(providerArchive(t, manifest, map[string][]byte{"scsi1.raw": []byte("disk")})))
	if err == nil || !strings.Contains(err.Error(), "unexpected entry") {
		t.Fatalf("got %v", err)
	}
	if len(runner.files) != 0 {
		t.Fatal("nothing must be uploaded for an unlisted disk")
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
