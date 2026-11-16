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
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/gillesdubois/plakar-integration-proxmox/internal/runner"
)

func TestAtLeast(t *testing.T) {
	for version, want := range map[string]bool{"8.4.1": true, "8.4": true, "9.0.3": true, "8.3.5": false, "7.4-3": false, "": false} {
		if got := atLeast(version, 8, 4); got != want {
			t.Errorf("atLeast(%q) = %v", version, got)
		}
	}
}

func TestStorageActive(t *testing.T) {
	out := "Name         Type     Status           Total            Used       Available        %\n" +
		"plakar     plakar     active               0               0               0    0.00%\n"
	if !storageActive(out, "plakar") {
		t.Error("active storage not detected")
	}
	if storageActive("Name Type Status\nplakar plakar disabled 0 0 0 N/A\n", "plakar") || storageActive("", "plakar") {
		t.Error("disabled or missing storage reported active")
	}
}

func TestFilterVMIDsKeepsLocalNode(t *testing.T) {
	resources := []vmResource{
		{VMID: 101, Type: "qemu", Node: "pve1"},
		{VMID: 102, Type: "lxc", Node: "pve2"},
		{VMID: 103, Type: "lxc", Node: "pve1", Status: "unknown"},
		{VMID: 104, Type: "storage", Node: "pve1"},
		{VMID: 100, Type: "lxc", Node: "pve1"},
	}
	if got := filterVMIDs(resources, "pve1", true); !reflect.DeepEqual(got, []int{100, 101}) {
		t.Fatalf("got %v", got)
	}
}

type unameRunner struct {
	runner.Local
	name string
}

func (r *unameRunner) Run(context.Context, string, ...string) (string, string, error) {
	return r.name + "\n", "", nil
}

func TestCheckNode(t *testing.T) {
	cfg := &Config{}
	client := &Client{cfg: cfg, runner: &unameRunner{name: "pve1.example.com"}}
	if err := client.CheckNode(context.Background()); err != nil || cfg.Node != "pve1" {
		t.Fatalf("node %q, %v", cfg.Node, err)
	}

	client.cfg = &Config{Node: "pve2"}
	if err := client.CheckNode(context.Background()); err == nil {
		t.Fatal("a node option naming another node must be refused")
	}
}

func TestFleecingEnabled(t *testing.T) {
	for conf, want := range map[string]bool{
		"bwlimit: 0\nfleecing: enabled=1,storage=local-lvm\n": true,
		"fleecing: 1,storage=local-zfs":                       true,
		"  fleecing: storage=x,enabled=yes":                   true,
		"fleecing: enabled=0,storage=local-lvm":               false,
		"#fleecing: enabled=1":                                false,
		"":                                                    false,
	} {
		if got := fleecingEnabled(conf); got != want {
			t.Errorf("fleecingEnabled(%q) = %v", conf, got)
		}
	}
}

type catRunner struct {
	runner.Local
	out string
	err error
}

func (r *catRunner) Run(context.Context, string, ...string) (string, string, error) {
	return r.out, "", r.err
}

func TestCheckProviderProtocol(t *testing.T) {
	for _, tc := range []struct {
		runner  *catRunner
		wantErr string
	}{
		{&catRunner{out: "1\n"}, ""},
		{&catRunner{err: errors.New("exit status 1")}, "does not publish its protocol"},
	} {
		err := (&Client{cfg: &Config{}, runner: tc.runner}).checkProviderProtocol(context.Background())
		if (tc.wantErr == "" && err != nil) || (tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr))) {
			t.Errorf("out %q: got %v, want %q", tc.runner.out, err, tc.wantErr)
		}
	}
}
