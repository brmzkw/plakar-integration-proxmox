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

package runner

import (
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestKnownHostsAcceptNew(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "known_hosts")
	addr := "10.0.0.10:22"
	remote := &net.TCPAddr{IP: net.ParseIP("10.0.0.10"), Port: 22}

	newKey := func() ssh.PublicKey {
		pub, _, _ := ed25519.GenerateKey(rand.Reader)
		key, _ := ssh.NewPublicKey(pub)
		return key
	}
	key := newKey()

	callback, err := knownHostsCallback(path, addr)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := callback(addr, remote, key); err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	content, _ := os.ReadFile(path)
	if strings.Count(string(content), "\n") != 1 {
		t.Fatalf("expected one known_hosts entry, got %q", content)
	}

	if err := callback(addr, remote, newKey()); err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("a changed host key must be rejected, got %v", err)
	}
}

func TestKnownHostKeyAlgorithms(t *testing.T) {
	path := filepath.Join(t.TempDir(), "known_hosts")
	addr := "10.0.0.10:22"
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	key, _ := ssh.NewPublicKey(pub)
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}

	if algos := knownHostKeyAlgorithms(path, addr); algos != nil {
		t.Fatalf("unknown host must keep the defaults, got %v", algos)
	}

	if err := addKnownHost(path, addr, key); err != nil {
		t.Fatal(err)
	}
	if algos := knownHostKeyAlgorithms(path, addr); len(algos) != 1 || algos[0] != ssh.KeyAlgoED25519 {
		t.Fatalf("got %v, want [%s]", algos, ssh.KeyAlgoED25519)
	}
	if algos := knownHostKeyAlgorithms(path, "10.0.0.11:22"); algos != nil {
		t.Fatalf("another host must keep the defaults, got %v", algos)
	}
}
