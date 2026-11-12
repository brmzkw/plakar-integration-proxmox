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
	"errors"
	"fmt"
	"io"
	"log"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/PlakarKorp/kloset/connectors"
	"github.com/PlakarKorp/kloset/connectors/exporter"
	"github.com/PlakarKorp/kloset/location"
	"github.com/gillesdubois/plakar-integration-proxmox/internal/proxmox"
	"github.com/google/uuid"
)

type ProxmoxExporter struct {
	cfg         *proxmox.Config
	client      *proxmox.Client
	temp        *proxmox.TempFiles
	restoreOpts restoreOptions
}

type vmConfigSidecar struct {
	vmType string
	data   []byte
}

type pendingRestore struct {
	record   *connectors.Record
	vmType   string
	vmid     int
	dumpBase string
	dumpPath string
}

type vmRuntimeState struct {
	exists  bool
	running bool
}

type restoreOptions struct {
	startOnRestore bool
	forceVMRestore bool
	unique         bool
	uniqueSet      bool
	newID          int
	storage        string
	pool           string
}

const protocolName = "proxmox+backup"

func init() {
	if err := exporter.Register(protocolName, 0, NewProxmoxExporter); err != nil {
		panic(err)
	}
}

func NewProxmoxExporter(ctx context.Context, opts *connectors.Options, name string, config map[string]string) (exporter.Exporter, error) {
	cfg, err := proxmox.ParseConfig(config)
	if err != nil {
		return nil, err
	}

	restoreOpts, err := parseRestoreOptions(config)
	if err != nil {
		return nil, err
	}

	client, err := proxmox.NewClient(cfg)
	if err != nil {
		return nil, err
	}

	return &ProxmoxExporter{
		cfg:         cfg,
		client:      client,
		temp:        proxmox.NewTempFiles(client),
		restoreOpts: restoreOpts,
	}, nil
}

func (p *ProxmoxExporter) Origin() string        { return p.cfg.Origin() }
func (p *ProxmoxExporter) Type() string          { return protocolName }
func (p *ProxmoxExporter) Root() string          { return "/" }
func (p *ProxmoxExporter) Flags() location.Flags { return 0 }

func (p *ProxmoxExporter) Ping(ctx context.Context) error {
	if err := p.client.Ping(ctx); err != nil {
		return err
	}
	return p.preflight(ctx)
}

func (p *ProxmoxExporter) preflight(ctx context.Context) error {
	if err := p.client.CheckCommands(ctx, "pvesh", "qm", "pct", "qmrestore", "qemu-nbd"); err != nil {
		return err
	}
	return p.client.CheckDumpDir(ctx)
}

func (p *ProxmoxExporter) Export(ctx context.Context, records <-chan *connectors.Record, results chan<- *connectors.Result) error {
	defer close(results)

	defer p.temp.Sweep(ctx)

	if err := p.preflight(ctx); err != nil {
		drainRecords(records, results, err)
		return err
	}

	sidecars := make(map[string]vmConfigSidecar)
	poolSidecars := make(map[string]string)
	pendingRestores := make([]pendingRestore, 0)

	for record := range records {
		if err := ctx.Err(); err != nil {
			results <- record.Error(err)
			continue
		}

		if record.Err != nil || record.IsXattr || !record.FileInfo.Lmode.IsRegular() {
			results <- record.Ok()
			continue
		}

		base := path.Base(record.Pathname)
		if proxmox.ProviderArchiveRegex.MatchString(base) {
			results <- resultFromRecord(record, p.restoreProvider(ctx, record))
			continue
		}

		if dumpBase, kind, ok := proxmox.ParseSidecarFilename(base); ok {
			err := collectSidecar(record, dumpBase, kind, sidecars, poolSidecars)
			if err != nil {
				_ = closeRecord(record)
			}
			results <- resultFromRecord(record, err)
			continue
		}

		vmType, vmid, err := proxmox.ParseDumpFilename(base)
		if err != nil {
			if strings.HasPrefix(base, "vzdump-") {
				results <- record.Error(err)
				continue
			}
			results <- record.Ok()
			continue
		}

		if err := p.checkTarget(ctx, vmType, p.targetVMID(vmid)); err != nil {
			results <- record.Error(err)
			continue
		}
		if err := p.client.EnsureFreeSpace(ctx, p.cfg.DumpDir, record.FileInfo.Lsize); err != nil {
			results <- record.Error(err)
			continue
		}

		dumpName := proxmox.BuildRestoreDumpFilename(base, vmType, vmid, time.Now())
		dumpPath := path.Join(p.cfg.DumpDir, dumpName)

		p.temp.Track(dumpPath)
		if err := p.writeDump(ctx, dumpPath, record.Reader); err != nil {
			results <- record.Error(err)
			continue
		}

		if err := closeRecord(record); err != nil {
			results <- resultFromRecord(record, err)
			continue
		}

		pendingRestores = append(pendingRestores, pendingRestore{
			record:   record,
			vmType:   vmType,
			vmid:     vmid,
			dumpBase: base,
			dumpPath: dumpPath,
		})
	}

	for _, pending := range pendingRestores {
		if err := ctx.Err(); err != nil {
			results <- resultFromRecord(pending.record, err)
			continue
		}

		configData, err := p.resolveConfigForDump(pending, sidecars)
		if err == nil {
			poolName, poolErr := p.resolvePoolForDump(pending, poolSidecars)
			if poolErr != nil {
				err = poolErr
			} else {
				targetVMID := pending.vmid
				if p.restoreOpts.newID != 0 {
					targetVMID = p.restoreOpts.newID
				}

				err = p.restoreDump(ctx, pending.dumpPath, pending.vmType, targetVMID, configData, poolName)
			}
		}

		if removeErr := p.temp.Remove(ctx, pending.dumpPath); removeErr != nil {
			log.Printf("unable to remove temporary dump %s: %v", pending.dumpPath, removeErr)
			if err == nil {
				err = removeErr
			}
		}

		results <- resultFromRecord(pending.record, err)
	}

	return nil
}

func (p *ProxmoxExporter) restoreProvider(ctx context.Context, record *connectors.Record) error {
	if record.Reader == nil {
		return fmt.Errorf("missing record reader for %s", record.Pathname)
	}
	if err := p.client.CheckProvider(ctx); err != nil {
		_ = closeRecord(record)
		return err
	}
	session, err := p.client.PrepareProviderRestore(ctx, record.Reader, func(m proxmox.ProviderManifest) error {
		return p.checkTarget(ctx, "qemu", p.targetVMID(m.VMID))
	})
	if closeErr := closeRecord(record); err == nil && closeErr != nil {
		session.Close()
		return closeErr
	}
	if err != nil {
		return err
	}
	defer session.Close()

	return p.restoreDump(ctx, session.Volume, "qemu", p.targetVMID(session.Manifest.VMID), []byte(session.Manifest.GuestConfig), session.Manifest.Pool)
}

func (p *ProxmoxExporter) Close(ctx context.Context) error {
	p.temp.Sweep(ctx)
	return p.client.Close()
}

// drainRecords fails every remaining record: the SDK blocks until all are consumed.
func drainRecords(records <-chan *connectors.Record, results chan<- *connectors.Result, err error) {
	for record := range records {
		results <- record.Error(err)
	}
}

func (p *ProxmoxExporter) writeDump(ctx context.Context, dumpPath string, reader io.Reader) error {
	writer, err := p.client.Create(ctx, dumpPath)
	if err != nil {
		return err
	}

	if _, err := io.Copy(writer, reader); err != nil {
		_ = writer.Close()
		return err
	}
	return writer.Close()
}

func collectSidecar(record *connectors.Record, dumpBase, kind string, sidecars map[string]vmConfigSidecar, poolSidecars map[string]string) error {
	if dumpBase == "" {
		return fmt.Errorf("invalid %s sidecar filename: %s", kind, path.Base(record.Pathname))
	}

	data, err := readRecordBytes(record)
	if err != nil {
		return err
	}

	if kind == proxmox.SidecarPool {
		poolSidecars[dumpBase] = strings.TrimSpace(string(data))
	} else {
		sidecars[dumpBase] = vmConfigSidecar{vmType: kind, data: data}
	}
	return nil
}

func (p *ProxmoxExporter) resolveConfigForDump(pending pendingRestore, sidecars map[string]vmConfigSidecar) ([]byte, error) {
	sidecar, ok := sidecars[pending.dumpBase]
	if !ok {
		return nil, nil
	}
	if sidecar.vmType != pending.vmType {
		return nil, fmt.Errorf("config sidecar type mismatch for dump %s: got %s, expected %s", pending.dumpBase, sidecar.vmType, pending.vmType)
	}
	return sidecar.data, nil
}

func (p *ProxmoxExporter) resolvePoolForDump(pending pendingRestore, sidecars map[string]string) (string, error) {
	poolName, ok := sidecars[pending.dumpBase]
	if !ok {
		return "", nil
	}
	return strings.TrimSpace(poolName), nil
}

func (p *ProxmoxExporter) targetVMID(vmid int) int {
	if p.restoreOpts.newID != 0 {
		return p.restoreOpts.newID
	}
	return vmid
}

func (p *ProxmoxExporter) errTargetExists(vmType string, vmid int) error {
	return fmt.Errorf("refusing restore for %s %d: it already exists on %s (pass -o force_vm_restore=true to overwrite it, or -o newid=<id> to restore next to it)", vmType, vmid, p.cfg.Origin())
}

func (p *ProxmoxExporter) checkTarget(ctx context.Context, vmType string, vmid int) error {
	if p.restoreOpts.forceVMRestore {
		return nil
	}
	state, err := p.vmState(ctx, vmType, vmid)
	if err != nil {
		return err
	}
	if state.exists {
		return p.errTargetExists(vmType, vmid)
	}
	return nil
}

func (p *ProxmoxExporter) restoreDump(ctx context.Context, dumpPath, vmType string, vmid int, configData []byte, poolName string) error {
	state, err := p.vmState(ctx, vmType, vmid)
	if err != nil {
		return err
	}

	opts, err := p.resolveRestoreOptions(ctx, vmType, state.exists, configData, poolName)
	if err != nil {
		return err
	}

	if state.exists {
		if !p.restoreOpts.forceVMRestore {
			return p.errTargetExists(vmType, vmid)
		}

		if state.running {
			if err := p.stopVM(ctx, vmType, vmid); err != nil {
				return err
			}
			state, err = p.vmState(ctx, vmType, vmid)
			if err != nil {
				return err
			}
			if state.running {
				return fmt.Errorf("refusing restore for %s %d: VM/CT is still running after stop request", vmType, vmid)
			}
		}
	}

	if err := p.runRestoreDump(ctx, dumpPath, vmType, vmid, opts); err != nil {
		return err
	}

	if opts.unique && vmType == "qemu" {
		if err := p.regenerateSMBIOSUUID(ctx, vmid); err != nil {
			return err
		}
	}

	if p.restoreOpts.startOnRestore {
		if err := p.startVM(ctx, vmType, vmid); err != nil {
			return err
		}
	}

	return nil
}

func (p *ProxmoxExporter) resolveRestoreOptions(ctx context.Context, vmType string, targetExists bool, configData []byte, poolName string) (restoreOptions, error) {
	opts := p.restoreOpts

	if !opts.uniqueSet {
		opts.unique = opts.newID != 0
	}

	if !targetExists {
		if opts.storage == "" {
			opts.storage = proxmox.StorageFromConfig(vmType, configData)
		}
		if opts.pool == "" && poolName != "" {
			exists, err := p.client.PoolExists(ctx, poolName)
			if err != nil {
				return restoreOptions{}, err
			}
			if exists {
				opts.pool = poolName
			} else {
				log.Printf("pool %q recorded in the backup no longer exists on %s: the restored VM/CT will not belong to any pool (use -o pool=<name> to pick another one)", poolName, p.cfg.Origin())
			}
		}
	}

	if opts.pool != "" {
		exists, err := p.client.PoolExists(ctx, opts.pool)
		if err != nil {
			return restoreOptions{}, err
		}
		if !exists {
			return restoreOptions{}, fmt.Errorf("restore pool does not exist: %s", opts.pool)
		}
	}

	return opts, nil
}

func (p *ProxmoxExporter) runRestoreDump(ctx context.Context, dumpPath, vmType string, vmid int, opts restoreOptions) error {
	vmidStr := strconv.Itoa(vmid)
	var cmd string
	var args []string
	switch vmType {
	case "qemu":
		cmd = "qmrestore"
		args = []string{dumpPath, vmidStr}
	case "lxc":
		cmd = "pct"
		args = []string{"restore", vmidStr, dumpPath}
	default:
		return fmt.Errorf("unsupported backup type: %s", vmType)
	}

	if opts.forceVMRestore {
		args = append(args, "--force")
	}
	if opts.unique {
		args = append(args, "--unique")
	}
	if opts.storage != "" {
		args = append(args, "--storage", opts.storage)
	}
	if opts.pool != "" {
		args = append(args, "--pool", opts.pool)
	}

	_, stderr, err := p.client.Run(ctx, cmd, args...)
	if err != nil {
		return fmt.Errorf("restore failed: %w: %s", err, strings.TrimSpace(stderr))
	}

	return nil
}

func (p *ProxmoxExporter) regenerateSMBIOSUUID(ctx context.Context, vmid int) error {
	configData, err := p.client.ReadQEMUConfig(ctx, vmid)
	if err != nil {
		return fmt.Errorf("restore of qemu %d succeeded but its smbios settings could not be read: %w", vmid, err)
	}

	smbios, err := uniqueSMBIOS(configData)
	if err != nil {
		return fmt.Errorf("restore of qemu %d succeeded but a new smbios uuid could not be generated: %w", vmid, err)
	}

	if _, stderr, err := p.client.Run(ctx, "qm", "set", strconv.Itoa(vmid), "--smbios1", smbios); err != nil {
		return fmt.Errorf("restore of qemu %d succeeded but its smbios uuid could not be replaced: %w: %s", vmid, err, strings.TrimSpace(stderr))
	}
	return nil
}

func uniqueSMBIOS(configData []byte) (string, error) {
	id, err := uuid.NewRandom()
	if err != nil {
		return "", err
	}

	var current string
	for _, entry := range proxmox.ActiveConfigEntries(configData) {
		if entry[0] == "smbios1" {
			current = entry[1]
			break
		}
	}
	if current == "" {
		return "uuid=" + id.String(), nil
	}

	fields := strings.Split(current, ",")
	for i, field := range fields {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(field)), "uuid=") {
			fields[i] = "uuid=" + id.String()
			return strings.Join(fields, ","), nil
		}
	}
	return strings.Join(append(fields, "uuid="+id.String()), ","), nil
}

func (p *ProxmoxExporter) vmState(ctx context.Context, vmType string, vmid int) (vmRuntimeState, error) {
	cmd, err := vmCommand(vmType)
	if err != nil {
		return vmRuntimeState{}, err
	}

	vmidStr := strconv.Itoa(vmid)
	stdout, stderr, err := p.client.Run(ctx, cmd, "status", vmidStr)
	output := preferredOutput(stdout, stderr)
	if err != nil {
		if isMissingVMError(output) {
			return vmRuntimeState{exists: false, running: false}, nil
		}
		return vmRuntimeState{}, fmt.Errorf("status failed for %s %d: %w: %s", vmType, vmid, err, output)
	}

	status := parseStatusValue(stdout + "\n" + stderr)
	switch status {
	case "running", "paused", "suspended":
		return vmRuntimeState{exists: true, running: true}, nil
	case "stopped":
		return vmRuntimeState{exists: true, running: false}, nil
	default:
		return vmRuntimeState{}, fmt.Errorf("unable to parse status for %s %d: %s", vmType, vmid, preferredOutput(stdout, stderr))
	}
}

func (p *ProxmoxExporter) startVM(ctx context.Context, vmType string, vmid int) error {
	cmd, err := vmCommand(vmType)
	if err != nil {
		return err
	}

	vmidStr := strconv.Itoa(vmid)
	stdout, stderr, err := p.client.Run(ctx, cmd, "start", vmidStr)
	if err != nil {
		output := preferredOutput(stdout, stderr)
		if isIgnorableStartError(output) {
			return nil
		}
		return fmt.Errorf("start failed for %s %d: %w: %s", vmType, vmid, err, output)
	}

	return nil
}

func (p *ProxmoxExporter) stopVM(ctx context.Context, vmType string, vmid int) error {
	cmd, err := vmCommand(vmType)
	if err != nil {
		return err
	}

	vmidStr := strconv.Itoa(vmid)
	stdout, stderr, err := p.client.Run(ctx, cmd, "stop", vmidStr)
	if err != nil {
		output := preferredOutput(stdout, stderr)
		if isIgnorableStopError(output) {
			return nil
		}
		return fmt.Errorf("stop failed for %s %d: %w: %s", vmType, vmid, err, output)
	}

	return p.waitUntilVMStopped(ctx, vmType, vmid)
}

func (p *ProxmoxExporter) waitUntilVMStopped(ctx context.Context, vmType string, vmid int) error {
	deadline := time.Now().Add(60 * time.Second)
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout while waiting for %s %d to stop", vmType, vmid)
		}

		state, err := p.vmState(ctx, vmType, vmid)
		if err != nil {
			return err
		}
		if !state.running {
			return nil
		}

		time.Sleep(1 * time.Second)
	}
}

func vmCommand(vmType string) (string, error) {
	switch vmType {
	case "qemu":
		return "qm", nil
	case "lxc":
		return "pct", nil
	default:
		return "", fmt.Errorf("unsupported backup type: %s", vmType)
	}
}

func isIgnorableStartError(output string) bool {
	normalized := strings.ToLower(output)
	return strings.Contains(normalized, "already running")
}

func isIgnorableStopError(output string) bool {
	normalized := strings.ToLower(output)
	if strings.Contains(normalized, "already stopped") || strings.Contains(normalized, "already down") {
		return true
	}
	return isMissingVMError(output)
}

func parseBoolOption(value string) (bool, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return false, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("invalid boolean value: %s", value)
	}
	return parsed, nil
}

func parseRestoreOptions(config map[string]string) (restoreOptions, error) {
	var opts restoreOptions

	startOnRestore, err := parseBoolOption(config["start_on_restore"])
	if err != nil {
		return restoreOptions{}, err
	}
	opts.startOnRestore = startOnRestore

	forceVMRestore, err := parseBoolOption(config["force_vm_restore"])
	if err != nil {
		return restoreOptions{}, err
	}
	opts.forceVMRestore = forceVMRestore

	if uniqueRaw, ok := config["unique"]; ok && strings.TrimSpace(uniqueRaw) != "" {
		unique, err := parseBoolOption(uniqueRaw)
		if err != nil {
			return restoreOptions{}, fmt.Errorf("invalid unique value: %s", strings.TrimSpace(uniqueRaw))
		}
		opts.unique = unique
		opts.uniqueSet = true
	}

	opts.storage = strings.TrimSpace(config["storage"])
	opts.pool = strings.TrimSpace(config["pool"])

	newIDRaw, hasNewID := config["newid"]
	if hasNewID {
		newIDRaw = strings.TrimSpace(newIDRaw)
		if newIDRaw != "" {
			newID, err := strconv.Atoi(newIDRaw)
			if err != nil {
				return restoreOptions{}, fmt.Errorf("invalid newid value: %s", newIDRaw)
			}
			if newID <= 0 {
				return restoreOptions{}, fmt.Errorf("newid must be a positive integer: %s", newIDRaw)
			}
			opts.newID = newID
		}
	}

	return opts, nil
}

var missingVMPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)configuration file '[^']*' does not exist`),
	regexp.MustCompile(`(?i)unable to find configuration file for (vm|ct) \d+`),
	regexp.MustCompile(`(?i)\b(vm|ct|container) \d+ does not exist\b`),
	regexp.MustCompile(`(?i)\bno such (vm|container)\b`),
}

func isMissingVMError(output string) bool {
	if strings.TrimSpace(output) == "" {
		return false
	}

	for _, pattern := range missingVMPatterns {
		if pattern.MatchString(output) {
			return true
		}
	}
	return false
}

func preferredOutput(stdout, stderr string) string {
	output := strings.TrimSpace(stderr)
	if output == "" {
		output = strings.TrimSpace(stdout)
	}
	return output
}

func parseStatusValue(output string) string {
	for _, line := range strings.Split(strings.ToLower(output), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "status:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "status:"))
		}
	}
	return ""
}

func readRecordBytes(record *connectors.Record) ([]byte, error) {
	if record.Reader == nil {
		return nil, fmt.Errorf("missing record reader for %s", record.Pathname)
	}

	data, readErr := io.ReadAll(record.Reader)
	closeErr := closeRecord(record)
	if readErr != nil && closeErr != nil {
		return nil, errors.Join(readErr, closeErr)
	}
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}

	return data, nil
}

func closeRecord(record *connectors.Record) error {
	if record.Reader == nil {
		return nil
	}
	err := record.Close()
	record.Reader = nil
	return err
}

func resultFromRecord(record *connectors.Record, err error) *connectors.Result {
	return &connectors.Result{
		Record: *record,
		Err:    err,
	}
}
