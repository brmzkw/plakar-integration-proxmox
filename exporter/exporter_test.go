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

package exporter

import (
	"context"
	"io"
	"regexp"
	"strings"
	"testing"

	"github.com/PlakarKorp/kloset/connectors"
	"github.com/PlakarKorp/kloset/objects"
	"github.com/gillesdubois/plakar-integration-proxmox/internal/proxmox"
)

func TestIsMissingVMError(t *testing.T) {
	for output, want := range map[string]bool{
		"Configuration file 'nodes/pve1/qemu-server/100.conf' does not exist": true,
		"unable to find configuration file for VM 100 - no such machine":      true,
		"CT 200 does not exist":          true,
		"no such VM ('100')":             true,
		"":                               false,
		"cluster not ready - no quorum?": false,
		"unable to open file '/etc/pve/nodes/pve1/qemu-server/100.conf.tmp.1234' - Permission denied": false,
		"storage 'local' does not exist": false,
	} {
		if got := isMissingVMError(output); got != want {
			t.Errorf("isMissingVMError(%q) = %v, want %v", output, got, want)
		}
	}
}

func TestUniqueSMBIOS(t *testing.T) {
	uuidRe := `[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`
	for config, pattern := range map[string]string{
		"cores: 1\n": `^uuid=` + uuidRe + `$`,
		"smbios1: uuid=11111111-1111-1111-1111-111111111111,manufacturer=QUNNRQ==,base64=1\n": `^uuid=` + uuidRe + `,manufacturer=QUNNRQ==,base64=1$`,
		"smbios1: serial=abc\n[snap]\nsmbios1: uuid=x\n":                                      `^serial=abc,uuid=` + uuidRe + `$`,
	} {
		got, err := uniqueSMBIOS([]byte(config))
		if err != nil {
			t.Fatal(err)
		}
		if !regexp.MustCompile(pattern).MatchString(got) || regexp.MustCompile("11111111-1111-1111").MatchString(got) {
			t.Errorf("uniqueSMBIOS(%q) = %q", config, got)
		}
	}
}

func fileRecord(pathname, content string) *connectors.Record {
	return &connectors.Record{
		Pathname: pathname,
		FileInfo: objects.FileInfo{Lmode: 0600, Lsize: int64(len(content))},
		Reader:   io.NopCloser(strings.NewReader(content)),
	}
}

func TestExportRequiresVMID(t *testing.T) {
	records := make(chan *connectors.Record, 1)
	results := make(chan *connectors.Result, 1)
	records <- fileRecord("/backup/qemu/100_a/qemu-2026_11_18-14_00_00/manifest.json", "{}")
	close(records)

	err := (&ProxmoxExporter{temp: proxmox.NewTempFiles(nil)}).Export(context.Background(), records, results)
	if err == nil || !strings.Contains(err.Error(), "vmid") {
		t.Fatalf("got %v", err)
	}
	if res := <-results; res.Err == nil {
		t.Fatal("the record must fail too")
	}
}

func TestProviderRunSkipsOtherVMs(t *testing.T) {
	results := make(chan *connectors.Result, 10)
	run := &providerRun{p: &ProxmoxExporter{restoreOpts: restoreOptions{vmid: 200}}, ctx: context.Background(), results: results}
	dir := "/backup/qemu/100_a/qemu-2026_11_18-14_00_00/"

	run.handle(fileRecord(dir+"manifest.json", `{"version":1,"vmid":100,"devices":{"drive-scsi0":{"size":4}}}`))
	run.handle(fileRecord(dir+"vm-drive-scsi0.raw", "disk"))
	run.end()
	close(results)

	n := 0
	for res := range results {
		n++
		if res.Err != nil {
			t.Errorf("%s: %v", res.Record.Pathname, res.Err)
		}
	}
	if n != 2 || run.found {
		t.Fatalf("%d results, found %v", n, run.found)
	}
}

func TestProviderRunDiskBeforeManifest(t *testing.T) {
	results := make(chan *connectors.Result, 1)
	run := &providerRun{p: &ProxmoxExporter{restoreOpts: restoreOptions{vmid: 100}}, ctx: context.Background(), results: results}

	run.handle(fileRecord("/backup/qemu/100_a/qemu-2026_11_18-14_00_00/vm-drive-scsi0.raw", "disk"))
	if res := <-results; res.Err == nil || !strings.Contains(res.Err.Error(), "comes before") {
		t.Fatalf("got %v", res.Err)
	}
}

func TestParseRestoreVMID(t *testing.T) {
	if opts, err := parseRestoreOptions(map[string]string{"vmid": "101"}); err != nil || opts.vmid != 101 {
		t.Fatalf("got %+v, %v", opts, err)
	}
	for _, bad := range []string{"0", "-1", "abc"} {
		if _, err := parseRestoreOptions(map[string]string{"vmid": bad}); err == nil {
			t.Errorf("vmid=%s must be refused", bad)
		}
	}
}
