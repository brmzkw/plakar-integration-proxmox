# Commands executed on the Proxmox node

Every command the integration runs on the node, and every file it reads or writes there. Nothing
else is executed.

## Execution context

- `mode=remote`: one SSH connection to `location` as `conn_username`. The host key is checked
  against `~/.ssh/known_hosts`, and unknown nodes are added on first use.
- `mode=local`: commands are started directly by the Plakar process, as the user running Plakar.
  Files are read and removed directly, without `cat` or `rm`.
- `vzdump`, `qm`, `pct`, `qmrestore`, `pvesm` and `pvesh` must run as root.
- A cancelled job sends SIGTERM to the running command, so vzdump and qmrestore can release their
  locks and snapshots. The command is killed if it is still running 5 minutes later.

## Connection check (`ping`, and the start of every job)

| Command | Purpose |
|---|---|
| `pvesh get /version --output-format json` | Proxmox is reachable and `pvesh` works |

## Backup (importer)

| Command | When | Purpose |
|---|---|---|
| `sh -c 'command -v <tool> >/dev/null 2>&1'` for `pvesh`, `vzdump` | start of the job | tools present |
| `uname -n` | start of the job | name of the connected node |
| `pvesh get /pools --poolid <pool> --output-format json` | `pool=<pool>` | VMs and CTs of the pool |
| `pvesh get /cluster/resources --type vm --output-format json` | once per job (cached 15 s) | VM/CT list for `all`, and the type, name, node and pool of each |
| `pvesh get /version --output-format json`, `pvesm status --storage plakar` | a QEMU VM is selected | PVE 8.4 or later, provider storage active |
| `pvesm status --storage <fleecing_storage>`, or `cat -- /etc/vzdump.conf` without it | a QEMU VM is selected | fleecing storage active, or whether the node defaults enable fleecing |
| `cat -- /etc/pve/qemu-server/<vmid>.conf` | each QEMU VM, when neither gives a fleecing storage | boot disk storage, used for fleecing |
| `cat -- /etc/pve/lxc/<vmid>.conf` | each LXC CT | `_lxc.conf` sidecar |
| `vzdump <vmid> --stdout --mode <backup_mode> --compress 0` | each LXC CT | dump streamed into Plakar |
| `env PLAKAR_SESSION=<session> vzdump <vmid> --storage plakar --mode <backup_mode> [--fleecing enabled=1,storage=<storage>]` | each QEMU VM | provider backup, disks read over NBD |
| `rm -f -- /run/plakar/<session>.sock` | after each QEMU VM | remove the session socket |

## Restore (exporter)

| Command | When | Purpose |
|---|---|---|
| `sh -c 'command -v <tool> >/dev/null 2>&1'` for `pvesh`, `qm`, `pct`, `qmrestore` | start of the job | tools present |
| `test -d <dump_dir>`, `test -w <dump_dir>` | start of the job | `dump_dir` exists and is writable |
| `pvesh get /version --output-format json`, `pvesm status --storage plakar` | each QEMU VM | PVE 8.4 or later, provider storage active |
| `df -Pk <dump_dir>` | before each LXC upload | enough free space |
| `dd of=<dump_dir>/<archive> bs=1M iflag=fullblock conv=sparse status=none` | each LXC archive | upload the archive |
| `dd of=<dump_dir>/plakar-<time>-<device>.raw ...` (as above) | each QEMU disk | upload the disk image, sparse |
| `mkdir -p -m 0700 -- <session>`, `dd of=<session>/{guest.conf,firewall.conf,manifest.json} ...` | each QEMU VM | prepare the restore session |
| `qm status <vmid>` / `pct status <vmid>` | each guest, before its upload and again before the restore | target exists? running? |
| `pvesh get /pools --output-format json` | a pool is recorded or `pool` is set | the pool exists |
| `qm stop <vmid>` / `pct stop <vmid>` | `force_vm_restore=true` and target running | stop before overwrite |
| `pct restore <vmid> <dump_dir>/<archive> [--force] [--unique] [--storage <s>] [--pool <p>]` | each LXC CT | restore |
| `qmrestore plakar:<vmid>/qemu-<time> <vmid> [--force] [--unique] [--storage <s>] [--pool <p>]` | each QEMU VM | restore through the provider |
| `cat -- /etc/pve/qemu-server/<vmid>.conf`, `qm set <vmid> --smbios1 uuid=<new>,...` | QEMU, when `unique` applies | new SMBIOS UUID |
| `qm start <vmid>` / `pct start <vmid>` | `start_on_restore=true` | start after restore |
| `rm -f -- <dump_dir>/<archive>` | after each LXC CT, success or not | remove the uploaded archive |
| `rm -f -- <image>`, `rm -rf -- <session>` | after each QEMU VM, success or not | clean up |

## Files touched on the node

| Path | Access | When |
|---|---|---|
| `/etc/pve/lxc/<vmid>.conf` | read | LXC backup |
| `/etc/pve/qemu-server/<vmid>.conf` | read | QEMU restore with `unique` |
| `<dump_dir>/<archive>`, `<dump_dir>/plakar-*.raw` | written, then removed | restore |
| `/run/plakar/<session>.sock`, `/run/plakar/plakar/restore/<vmid>-qemu-<time>/` | written, then removed | QEMU backup, QEMU restore |

The integration writes nothing in `dump_dir` during a backup. vzdump still uses its own dump
directory (from `/etc/vzdump.conf`, by default the `local` storage):

- it creates a small working directory `vzdump-<type>-<vmid>-<time>.tmp`, which it removes at the
  end;
- it writes its log `vzdump-<type>-<vmid>-<time>.log` and leaves it there;
- for LXC in `suspend` mode, or when snapshots are not possible, it copies the container to a
  temporary directory under `/var/tmp`;
- it sends the usual end-of-job notification configured on the node.
