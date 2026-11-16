# Design

How the integration works inside. For what runs on the node, see [COMMANDS.md](COMMANDS.md).

## Backup (importer)

1. Read and validate the config and the selection (`vmid`, `pool` or `all`).
2. Preflight: `pvesh` and `vzdump` are present, and the connected node is read with `uname -n`.
   vzdump only backs up guests of the node it runs on, and exits 0 without doing anything for the
   others, so the guest list is filtered to that node and a `node` option naming another node is
   refused.
3. List the guests with `pvesh get /cluster/resources` or `pvesh get /pools --poolid`. Guests the
   cluster reports as `unknown` (node unreachable) are skipped with a warning.
4. If a QEMU VM is selected, check PVE 8.4 or later, the `plakar` storage, and that
   `fleecing_storage` is active when set.
5. Each guest becomes records under `/backup/<type>/<vmid>_<name>/`. A guest that fails is reported
   as an error entry at `/backup/<vmid>`, and the job carries on with the others. The job only fails
   if every guest failed.

### Containers

The CT config and pool are read first, so a CT that fails never leaves a dump without its
sidecars. Then the dump record is emitted, and `vzdump --stdout --compress 0` only starts when
Plakar begins reading it. A vzdump that started before Plakar reads its record would take the
node-wide vzdump lock while the record still waits in Plakar's queue, and the next guest's vzdump
would block on that lock. Only one vzdump runs at a time. A vzdump failure, or a vzdump that
produced no data, makes the read of the record fail with vzdump's stderr.

### QEMU VMs through plakar-pve-provider

One tar record per VM. Plakar may hold records back until the importer is done, so several
records waiting on one shared vzdump session could never be read.

1. When Plakar starts reading the record, it listens on `/run/plakar/<session>.sock`. That is a
   remote Unix socket forward over SSH in `mode=remote`, and a local socket in `mode=local`.
2. It runs `env PLAKAR_SESSION=<session> vzdump <vmid> --storage plakar`. vzdump refuses provider
   backups without fleecing, so it adds `--fleecing enabled=1,storage=<storage>`: `fleecing_storage`
   if set, else nothing when `/etc/vzdump.conf` enables fleecing, else the VM's boot disk storage.
3. The provider sends the manifest, and Plakar writes it first into the tar. Plakar then reads each
   disk over its NBD socket.
   - Holes are not transferred (NBD structured replies).
   - Each read must cover the whole range requested, or the backup fails.
   - What has been read is trimmed when the export allows it, so the fleecing image does not grow.
   - Every 5% of a disk, a progress line goes to the provider, which logs it in the vzdump task.
4. Once every disk is in the tar, Plakar replies `ok` (or `error`), and vzdump releases the
   snapshot. The record only ends once vzdump has exited, and a failure fails the record.

## Restore (exporter)

Records are read in order. A guest whose target VMID already exists is refused before anything is
uploaded, unless `force_vm_restore` is set. Sidecars are collected, and LXC archives are uploaded to
`dump_dir` (after a free-space check) and restored once every record has been read. QEMU tars are
restored as they arrive; their disks are written sparse, so there is no free-space check up front.

For each guest:

1. `qm status` / `pct status` tells whether the target exists. Only the precise "does not exist"
   messages of Proxmox count as missing. A quorum or permission error must never be read as "free
   VMID", or the restore would overwrite a real VM.
2. An existing target is refused unless `force_vm_restore=true`. With it, a running target is
   stopped first, then overwritten with `--force`.
3. When the target does not exist and no `storage`/`pool` is given, they come from the backup:
   - storage: the boot disk of the QEMU config, or the CT's `rootfs`;
   - pool: the pool sidecar or the manifest, dropped with a warning if it no longer exists.
4. `unique` (default with `newid`) passes `--unique`, and for QEMU also replaces the SMBIOS UUID
   with `qm set --smbios1`.
5. Uploaded files are removed whether the restore succeeded or not.

### QEMU restore through the provider

Plakar reads the tar in order. Each disk image is uploaded to `dump_dir`, written sparse so only
the data takes room. Plakar then:

- creates the session `/run/plakar/plakar/restore/<vmid>-qemu-<time>/` (`manifest.json` last,
  since the provider looks for it, `guest.conf`, `firewall.conf`); `manifest.json` gives each
  image path (`devices.<device>.image`);
- runs `qmrestore plakar:<vmid>/qemu-<time> <vmid>`. The provider hands each image path to
  `qemu-img`, which skips the holes of the sparse image.

Afterwards the images and session are removed.

## Cancellation

A cancelled job sends SIGTERM to the command running on the node (`session.Signal` over SSH, since
closing an SSH channel does not signal a command run without a tty). vzdump and qmrestore then
release their locks, snapshots and half-restored disks. The command is killed if it is still
running 5 minutes later.
