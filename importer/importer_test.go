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

package importer

import "testing"

func TestParseSelection(t *testing.T) {
	for _, tc := range []struct {
		config  map[string]string
		wantErr bool
		all     bool
	}{
		{map[string]string{"vmid": "101"}, false, false},
		{map[string]string{"pool": "prod"}, false, false},
		{map[string]string{"all": ""}, false, true},
		{map[string]string{"all": "true"}, false, true},
		{map[string]string{}, true, false},
		{map[string]string{"all": "false"}, true, false},
		{map[string]string{"all": "ture"}, true, false},
		{map[string]string{"vmid": "abc"}, true, false},
		{map[string]string{"vmid": "101", "pool": "prod"}, true, false},
	} {
		sel, err := parseSelection(tc.config)
		if (err != nil) != tc.wantErr || (err == nil && sel.all != tc.all) {
			t.Errorf("%v: got %+v, %v", tc.config, sel, err)
		}
	}
}
