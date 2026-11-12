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

import "testing"

func TestParseQEMUStorage(t *testing.T) {
	for name, tc := range map[string]struct{ config, want string }{
		"boot order wins over efidisk and cdrom": {
			"boot: order=scsi0;ide2;net0\nefidisk0: fast:vm-100-disk-0,size=4M\nide2: local:iso/debian.iso,media=cdrom\nscsi0: ceph:vm-100-disk-1,size=32G\n",
			"ceph",
		},
		"legacy boot falls back to first disk by name": {
			"boot: cdn\nvirtio1: slow:vm-100-disk-2\nscsi0: ceph:vm-100-disk-1\n",
			"ceph",
		},
		"snapshot sections are ignored": {
			"scsi0: ceph:vm-100-disk-1\n\n[before-upgrade]\nscsi0: old:vm-100-disk-1\n",
			"ceph",
		},
		"cdrom only": {
			"ide2: local:iso/debian.iso,media=cdrom\n",
			"",
		},
		"none volume": {
			"ide2: none,media=cdrom\nscsi0: none\n",
			"",
		},
	} {
		if got := StorageFromConfig("qemu", []byte(tc.config)); got != tc.want {
			t.Errorf("%s: got %q, want %q", name, got, tc.want)
		}
	}

	if got := StorageFromConfig("lxc", []byte("rootfs: zfs:subvol-200-disk-0,size=8G\n")); got != "zfs" {
		t.Errorf("lxc rootfs: got %q", got)
	}
}
