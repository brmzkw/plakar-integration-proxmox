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

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/PlakarKorp/kloset/connectors"
	"github.com/PlakarKorp/kloset/connectors/importer"
	"github.com/PlakarKorp/kloset/location"
	"github.com/PlakarKorp/kloset/objects"
	"github.com/gillesdubois/plakar-integration-proxmox/internal/proxmox"
)

type ProxmoxImporter struct {
	cfg       *proxmox.Config
	client    *proxmox.Client
	selection selection
}

type selection struct {
	vmid *int
	pool string
	all  bool
}

const protocolName = "proxmox+backup"
const backupSnapshotRoot = "/backup"

func init() {
	if err := importer.Register(protocolName, 0, NewProxmoxImporter); err != nil {
		panic(err)
	}
}

func NewProxmoxImporter(ctx context.Context, opts *connectors.Options, name string, config map[string]string) (importer.Importer, error) {
	cfg, err := proxmox.ParseConfig(config)
	if err != nil {
		return nil, err
	}

	selection, err := parseSelection(config)
	if err != nil {
		return nil, err
	}

	client, err := proxmox.NewClient(cfg)
	if err != nil {
		return nil, err
	}

	return &ProxmoxImporter{
		cfg:       cfg,
		client:    client,
		selection: selection,
	}, nil
}

func (p *ProxmoxImporter) Origin() string        { return p.cfg.Origin() }
func (p *ProxmoxImporter) Type() string          { return protocolName }
func (p *ProxmoxImporter) Root() string          { return "/" }
func (p *ProxmoxImporter) Flags() location.Flags { return location.FLAG_STREAM }

func (p *ProxmoxImporter) Ping(ctx context.Context) error {
	if err := p.client.Ping(ctx); err != nil {
		return err
	}
	return p.preflight(ctx)
}

func (p *ProxmoxImporter) preflight(ctx context.Context) error {
	if err := p.client.CheckCommands(ctx, "pvesh", "vzdump"); err != nil {
		return err
	}
	return p.client.CheckNode(ctx)
}

func (p *ProxmoxImporter) Import(ctx context.Context, records chan<- *connectors.Record, _ <-chan *connectors.Result) error {
	defer close(records)

	if err := p.preflight(ctx); err != nil {
		return err
	}

	vmids, err := p.resolveVMIDs(ctx)
	if err != nil {
		return err
	}
	if len(vmids) == 0 {
		return fmt.Errorf("no VM/CT found for selection")
	}
	if err := p.checkProvider(ctx, vmids); err != nil {
		return err
	}

	var (
		succeeded int
		failed    int
		lastErr   error
	)

	for _, vmid := range vmids {
		if err := ctx.Err(); err != nil {
			return err
		}

		if err := p.importVM(ctx, records, vmid); err != nil {
			if ctx.Err() != nil {
				return err
			}

			failed++
			lastErr = err
			log.Printf("backup of vmid %d failed: %v", vmid, err)

			failure := connectors.NewError(vmFailurePath(vmid), err)
			if emitErr := p.emitRecord(ctx, records, failure); emitErr != nil {
				return emitErr
			}
			continue
		}
		succeeded++
	}

	if succeeded == 0 {
		return fmt.Errorf("all %d VM/CT backup(s) failed, last error: %w", failed, lastErr)
	}
	if failed > 0 {
		log.Printf("%d of %d VM/CT failed to back up", failed, failed+succeeded)
	}
	return nil
}

func (p *ProxmoxImporter) Close(ctx context.Context) error {
	return p.client.Close()
}

func (p *ProxmoxImporter) importVM(ctx context.Context, records chan<- *connectors.Record, vmid int) error {
	vmType, err := p.client.VMType(ctx, vmid)
	if err != nil {
		return err
	}

	vmName, err := p.client.VMName(ctx, vmid)
	if err != nil {
		return err
	}

	if vmType == "qemu" {
		return p.importVMProvider(ctx, records, vmid, vmName)
	}

	configData, err := p.client.ReadLXCConfig(ctx, vmid)
	if err != nil {
		return err
	}
	pool, err := p.client.VMPool(ctx, vmid)
	if err != nil {
		return err
	}

	record, archiveName := p.buildBackupRecord(ctx, vmid, vmName)
	if err := p.emitRecord(ctx, records, record); err != nil {
		return err
	}
	if err := p.emitSidecar(ctx, records, vmid, vmName, proxmox.SidecarFilename(archiveName, proxmox.SidecarLXC), configData); err != nil {
		return err
	}
	if pool == "" {
		return nil
	}
	return p.emitSidecar(ctx, records, vmid, vmName, proxmox.SidecarFilename(archiveName, proxmox.SidecarPool), []byte(pool))
}

func (p *ProxmoxImporter) checkProvider(ctx context.Context, vmids []int) error {
	for _, vmid := range vmids {
		if vmType, err := p.client.VMType(ctx, vmid); err == nil && vmType == "qemu" {
			return p.client.CheckProvider(ctx)
		}
	}
	return nil
}

// One record per VM: several records waiting on one vzdump session would deadlock.
func (p *ProxmoxImporter) importVMProvider(ctx context.Context, records chan<- *connectors.Record, vmid int, vmName string) error {
	pool, err := p.client.VMPool(ctx, vmid)
	if err != nil {
		return err
	}

	name := proxmox.ProviderArchiveName(time.Now())
	return p.emitRecord(ctx, records, &connectors.Record{
		Pathname: buildBackupSnapshotPath("qemu", vmid, vmName, name),
		FileInfo: objects.FileInfo{
			Lname:    name,
			Lsize:    -1,
			Lmode:    0600,
			LmodTime: time.Now(),
			Ldev:     1,
		},
		Reader: p.client.ProviderBackupStream(ctx, vmid, pool),
	})
}

func (p *ProxmoxImporter) resolveVMIDs(ctx context.Context) ([]int, error) {
	switch {
	case p.selection.vmid != nil:
		return []int{*p.selection.vmid}, nil
	case p.selection.pool != "":
		return p.client.ListPoolVMIDs(ctx, p.selection.pool)
	default:
		return p.client.ListAllVMIDs(ctx)
	}
}

func (p *ProxmoxImporter) buildBackupRecord(ctx context.Context, vmid int, vmName string) (*connectors.Record, string) {
	archiveName, reader := p.client.BackupLXCStream(ctx, vmid)

	return &connectors.Record{
		Pathname: buildBackupSnapshotPath("lxc", vmid, vmName, archiveName),
		FileInfo: objects.FileInfo{
			Lname:    archiveName,
			Lsize:    -1,
			Lmode:    0600,
			LmodTime: time.Now(),
			Ldev:     1,
		},
		Reader: reader,
	}, archiveName
}

func (p *ProxmoxImporter) emitSidecar(ctx context.Context, records chan<- *connectors.Record, vmid int, vmName, name string, data []byte) error {
	return p.emitRecord(ctx, records, &connectors.Record{
		Pathname: buildBackupSnapshotPath("lxc", vmid, vmName, name),
		FileInfo: objects.FileInfo{
			Lname:    name,
			Lsize:    int64(len(data)),
			Lmode:    0600,
			LmodTime: time.Now(),
			Ldev:     1,
		},
		Reader: io.NopCloser(bytes.NewReader(data)),
	})
}

func (p *ProxmoxImporter) emitRecord(ctx context.Context, records chan<- *connectors.Record, record *connectors.Record) error {
	select {
	case <-ctx.Done():
		_ = record.Close()
		return ctx.Err()
	case records <- record:
	}
	return nil
}

func vmFailurePath(vmid int) string {
	return path.Join(backupSnapshotRoot, strconv.Itoa(vmid))
}

func buildBackupSnapshotPath(vmType string, vmid int, vmName, filename string) string {
	return path.Join(backupSnapshotRoot, vmType, buildBackupSnapshotDir(vmid, vmName), filename)
}

func buildBackupSnapshotDir(vmid int, vmName string) string {
	name := sanitizeSnapshotDirComponent(vmName)
	if name == "" {
		name = "unnamed"
	}
	return fmt.Sprintf("%d_%s", vmid, name)
}

var unsafeDirChars = regexp.MustCompile(`[^A-Za-z0-9.-]+`)

func sanitizeSnapshotDirComponent(value string) string {
	return strings.Trim(unsafeDirChars.ReplaceAllString(strings.TrimSpace(value), "_"), "._-")
}

func parseSelection(config map[string]string) (selection, error) {
	var sel selection

	if vmidStr, ok := config["vmid"]; ok {
		vmidStr = strings.TrimSpace(vmidStr)
		if vmidStr != "" {
			vmid, err := strconv.Atoi(vmidStr)
			if err != nil {
				return sel, fmt.Errorf("invalid vmid: %s", vmidStr)
			}
			sel.vmid = &vmid
		}
	}

	if pool, ok := config["pool"]; ok {
		pool = strings.TrimSpace(pool)
		if pool != "" {
			sel.pool = pool
		}
	}

	if all, ok := config["all"]; ok {
		all = strings.TrimSpace(all)
		sel.all = true
		if all != "" {
			parsed, err := strconv.ParseBool(all)
			if err != nil {
				return sel, fmt.Errorf("invalid all value: %s", all)
			}
			sel.all = parsed
		}
	}

	setCount := 0
	if sel.vmid != nil {
		setCount++
	}
	if sel.pool != "" {
		setCount++
	}
	if sel.all {
		setCount++
	}

	if setCount == 0 {
		return sel, fmt.Errorf("missing backup selection: set one of vmid, pool or all=true")
	}
	if setCount > 1 {
		return sel, fmt.Errorf("backup selection must specify only one of vmid, pool or all")
	}

	return sel, nil
}
