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
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
)

const FreeSpaceMargin = 256 << 20

func (c *Client) CheckCommands(ctx context.Context, names ...string) error {
	for _, name := range names {
		if _, _, err := c.runner.Run(ctx, "sh", "-c", "command -v "+name+" >/dev/null 2>&1"); err != nil {
			return fmt.Errorf("%s not found on %s (mode=%s): is this host a Proxmox node?", name, c.cfg.Origin(), c.cfg.Mode)
		}
	}
	return nil
}

// vzdump --node naming another node exits 0 and does nothing.
func (c *Client) CheckNode(ctx context.Context) error {
	stdout, stderr, err := c.runner.Run(ctx, "uname", "-n")
	if err != nil {
		return fmt.Errorf("unable to read the node name: %w: %s", err, strings.TrimSpace(stderr))
	}
	local, _, _ := strings.Cut(strings.TrimSpace(stdout), ".")
	if c.cfg.Node != "" && c.cfg.Node != local {
		return fmt.Errorf("node=%s, but %s is node %s: connect to the node that runs the guests", c.cfg.Node, c.cfg.Origin(), local)
	}
	c.cfg.Node = local
	return nil
}

func (c *Client) CheckProvider(ctx context.Context) error {
	stdout, err := c.runPvesh(ctx, "pvesh unavailable", "get", "/version", "--output-format", "json")
	if err != nil {
		return err
	}
	var v struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal([]byte(stdout), &v); err != nil {
		return fmt.Errorf("unable to parse PVE version: %w", err)
	}
	if !atLeast(v.Version, 8, 4) {
		return fmt.Errorf("QEMU backups need Proxmox VE 8.4 or later, %s runs %s", c.cfg.Origin(), v.Version)
	}

	stdout, stderr, err := c.runner.Run(ctx, "pvesm", "status", "--storage", ProviderStorage)
	if err != nil || !storageActive(stdout, ProviderStorage) {
		return fmt.Errorf("storage %q is missing or inactive on %s: install plakar-pve-provider and run `pvesm add plakar plakar`: %s",
			ProviderStorage, c.cfg.Origin(), strings.TrimSpace(stderr+stdout))
	}

	// The provider reads disks over NBD, which vzdump refuses without fleecing.
	if s := c.cfg.FleecingStorage; s != "" {
		stdout, stderr, err := c.runner.Run(ctx, "pvesm", "status", "--storage", s)
		if err != nil || !storageActive(stdout, s) {
			return fmt.Errorf("fleecing_storage %q is missing or inactive on %s: %s", s, c.cfg.Origin(), strings.TrimSpace(stderr+stdout))
		}
	} else {
		stdout, _, _ := c.runner.Run(ctx, "cat", "--", "/etc/vzdump.conf")
		c.fleecingOnBootDisk = !fleecingEnabled(stdout)
	}
	return nil
}

func fleecingEnabled(conf string) bool {
	for _, line := range strings.Split(conf, "\n") {
		value, ok := strings.CutPrefix(strings.TrimSpace(line), "fleecing:")
		if !ok {
			continue
		}
		for _, part := range strings.Split(value, ",") {
			key, val, hasKey := strings.Cut(strings.TrimSpace(part), "=")
			if !hasKey {
				key, val = "enabled", key
			}
			if key == "enabled" {
				switch strings.ToLower(strings.TrimSpace(val)) {
				case "1", "yes", "true", "on":
					return true
				}
			}
		}
	}
	return false
}

func atLeast(version string, major, minor int) bool {
	var gotMajor, gotMinor int
	if _, err := fmt.Sscanf(version, "%d.%d", &gotMajor, &gotMinor); err != nil {
		return false
	}
	return gotMajor > major || (gotMajor == major && gotMinor >= minor)
}

func storageActive(output, storage string) bool {
	for _, line := range strings.Split(output, "\n") {
		if fields := strings.Fields(line); len(fields) >= 3 && fields[0] == storage {
			return fields[2] == "active"
		}
	}
	return false
}

func (c *Client) CheckDumpDir(ctx context.Context) error {
	if _, _, err := c.runner.Run(ctx, "test", "-d", c.cfg.DumpDir); err != nil {
		return fmt.Errorf("dump_dir %s does not exist on %s", c.cfg.DumpDir, c.cfg.Origin())
	}
	if _, _, err := c.runner.Run(ctx, "test", "-w", c.cfg.DumpDir); err != nil {
		return fmt.Errorf("dump_dir %s is not writable on %s", c.cfg.DumpDir, c.cfg.Origin())
	}
	return nil
}

func (c *Client) AvailableBytes(ctx context.Context, dir string) (int64, error) {
	stdout, stderr, err := c.runner.Run(ctx, "df", "-Pk", dir)
	if err != nil {
		return 0, fmt.Errorf("df failed for %s: %w: %s", dir, err, strings.TrimSpace(stderr))
	}
	return parseDfAvailable(stdout)
}

func (c *Client) EnsureFreeSpace(ctx context.Context, dir string, size int64) error {
	if size <= 0 {
		return nil
	}

	available, err := c.AvailableBytes(ctx, dir)
	if err != nil {
		log.Printf("unable to check free space on %s: %v", dir, err)
		return nil
	}

	required := size + FreeSpaceMargin
	if available < required {
		return fmt.Errorf("not enough free space in %s: %s available, %s required",
			dir, humanBytes(available), humanBytes(required))
	}
	return nil
}

func parseDfAvailable(output string) (int64, error) {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) < 2 {
		return 0, fmt.Errorf("unexpected df output: %s", strings.TrimSpace(output))
	}

	last := strings.TrimSpace(lines[len(lines)-1])
	fields := strings.Fields(last)
	if len(fields) < 4 {
		return 0, fmt.Errorf("unexpected df output: %s", last)
	}

	blocks, err := strconv.ParseInt(fields[3], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid df available value %q", fields[3])
	}
	return blocks * 1024, nil
}

func humanBytes(size int64) string {
	const unit = 1024
	if size < unit {
		return fmt.Sprintf("%d B", size)
	}

	div, exp := int64(unit), 0
	for n := size / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(size)/float64(div), "KMGTPE"[exp])
}
