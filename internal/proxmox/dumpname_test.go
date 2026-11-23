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
	"testing"
	"time"
)

func TestParseSidecarFilename(t *testing.T) {
	cases := []struct{ name, archive, kind string }{
		{"/backup/lxc/1_a/vzdump-lxc-1-x.tar.zst_lxc.conf", "vzdump-lxc-1-x.tar.zst", SidecarLXC},
		{"vzdump-qemu-1-x.vma_QEMU.conf", "vzdump-qemu-1-x.vma", SidecarQEMU},
		{"dump_pool.conf", "dump", SidecarPool},
		{"_pool.conf", "", SidecarPool},
	}
	for _, c := range cases {
		archive, kind, ok := ParseSidecarFilename(c.name)
		if !ok || archive != c.archive || kind != c.kind {
			t.Errorf("%s: got %q %q %v", c.name, archive, kind, ok)
		}
		if c.archive != "" && SidecarFilename(c.archive, c.kind) != c.archive+"_"+c.kind+".conf" {
			t.Errorf("%s: SidecarFilename does not round-trip", c.name)
		}
	}
	if _, _, ok := ParseSidecarFilename("vzdump-lxc-1-x.tar.zst"); ok {
		t.Error("an archive is not a sidecar")
	}
}

func TestProviderBackupDirName(t *testing.T) {
	name := ProviderBackupDirName(time.Date(2026, 11, 12, 14, 40, 38, 0, time.UTC))
	if name != "qemu-2026_11_12-14_40_38" || !ProviderBackupDirRegex.MatchString(name) {
		t.Fatalf("got %q", name)
	}
	if ProviderBackupDirRegex.MatchString("qemu-2026_11_12-14_40_38.tar") {
		t.Fatal("the old tar name must not match")
	}
}
