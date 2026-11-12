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
	"regexp"
	"testing"
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
