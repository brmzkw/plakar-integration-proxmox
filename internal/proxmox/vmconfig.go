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
	"regexp"
	"sort"
	"strings"
)

var qemuDiskKeyRegex = regexp.MustCompile(`^(scsi|virtio|sata|ide|nvme)\d+$`)

func StorageFromConfig(vmType string, configData []byte) string {
	if len(configData) == 0 {
		return ""
	}

	switch vmType {
	case "qemu":
		return parseQEMUStorage(configData)
	case "lxc":
		return parseLXCStorage(configData)
	default:
		return ""
	}
}

func ActiveConfigEntries(configData []byte) [][2]string {
	entries := make([][2]string, 0)

	for _, line := range strings.Split(string(configData), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			break
		}

		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}

		entries = append(entries, [2]string{
			strings.TrimSpace(strings.ToLower(key)),
			strings.TrimSpace(value),
		})
	}

	return entries
}

func parseQEMUStorage(configData []byte) string {
	disks := make(map[string]string)
	keys := make([]string, 0)
	var bootOrder []string

	for _, entry := range ActiveConfigEntries(configData) {
		key, value := entry[0], entry[1]

		if key == "boot" {
			bootOrder = parseBootOrder(value)
			continue
		}
		if !qemuDiskKeyRegex.MatchString(key) || isCDROMVolume(value) {
			continue
		}

		storage := parseStorageFromVolumeSpec(value)
		if storage == "" {
			continue
		}
		disks[key] = storage
		keys = append(keys, key)
	}

	for _, device := range bootOrder {
		if storage, ok := disks[device]; ok {
			return storage
		}
	}

	sort.Strings(keys)
	if len(keys) > 0 {
		return disks[keys[0]]
	}
	return ""
}

func parseLXCStorage(configData []byte) string {
	for _, entry := range ActiveConfigEntries(configData) {
		if entry[0] == "rootfs" {
			return parseStorageFromVolumeSpec(entry[1])
		}
	}
	return ""
}

func parseBootOrder(value string) []string {
	for _, part := range strings.Split(value, ",") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(part), "order=")
		if !ok {
			continue
		}

		devices := make([]string, 0)
		for _, device := range strings.Split(rest, ";") {
			if device = strings.ToLower(strings.TrimSpace(device)); device != "" {
				devices = append(devices, device)
			}
		}
		return devices
	}
	return nil
}

func isCDROMVolume(spec string) bool {
	for _, part := range strings.Split(spec, ",") {
		if strings.EqualFold(strings.TrimSpace(part), "media=cdrom") {
			return true
		}
	}
	return false
}

func parseStorageFromVolumeSpec(spec string) string {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return ""
	}

	volume := strings.Split(spec, ",")[0]
	volume = strings.TrimSpace(volume)
	if volume == "" {
		return ""
	}

	storage, _, ok := strings.Cut(volume, ":")
	if !ok {
		return ""
	}
	storage = strings.TrimSpace(storage)
	if storage == "" {
		return ""
	}

	if strings.EqualFold(storage, "none") {
		return ""
	}
	return storage
}
