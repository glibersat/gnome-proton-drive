# Volume Monitor Architecture

`gvfsd-proton-volume-monitor` is a GVfs remote volume monitor daemon that makes
Proton Drive accounts appear automatically in Nautilus and other GIO clients.
It lives in `volume-monitor/` and is written in Vala.

## How GVfs remote volume monitors work

GVfs discovers volume monitors via descriptor files in
`/usr/share/gvfs/remote-volume-monitors/`.  Each descriptor names a D-Bus
service; GIO's `GProxyVolumeMonitor` launches the service on demand and
communicates with it over the `org.gtk.vfs.RemoteVolumeMonitor` D-Bus
interface.  The daemon emits `VolumeAdded` / `VolumeRemoved` signals and
responds to `List` calls so GIO can reconstruct the volume list after restart.

## Component overview

```
libsecret ──────────────────┐
                            ▼
org.gnome.ProtonDrive  ──▶  ProtonAccountWatcher
  D-Bus signal                  │ account_added(username)
  (from setup wizard)           │ account_removed(username)
                                ▼
                    ProtonRemoteVolumeMonitor
                    (org.gtk.vfs.RemoteVolumeMonitor)
                        │               │
                  VolumeAdded     VolumeRemoved
                        │
                   ProtonVolume
                    icon, display-name, UUID
                        │
                  gio mount proton://   ──▶  gvfsd-proton
```

### `ProtonAccountWatcher`

Detects accounts via two complementary paths, deduplicating with an in-memory
set so each signal fires exactly once per transition:

1. **libsecret collection watch** — connects to `Secret.Service` and reloads
   on every `collection-changed` signal.  The reload searches for all keyring
   items matching schema `org.gnome.proton.drive` / `field=uid` (one item per
   account) and diffs against the known set.  This catches accounts stored by
   any tool, not just our own setup wizard.

2. **D-Bus fast path** — subscribes to `AccountAdded` / `AccountRemoved`
   signals on interface `org.gnome.ProtonDrive` at object path
   `/org/gnome/ProtonDrive`.  The setup wizard emits these immediately after
   writing to the keyring so the volume appears in Nautilus without waiting for
   the next libsecret change notification.

The two paths are intentionally redundant: the D-Bus signal is fast, the
libsecret watch is the authoritative source of truth.

### `ProtonVolume`

Models one Proton Drive account as a `GVolume`:

| Property     | Value                                      |
|--------------|--------------------------------------------|
| Display name | `Proton Drive (<email>)`                   |
| Icon         | `proton-drive-symbolic`                    |
| UUID         | SHA-1 of the account email (stable, opaque)|
| Mount scheme | `proton://`                                |

### `ProtonRemoteVolumeMonitor`

Owns the `org.gnome.GVfs.VolumeMonitor.Proton` D-Bus name, implements
`org.gtk.vfs.RemoteVolumeMonitor`, and translates `ProtonAccountWatcher`
signals into D-Bus `VolumeAdded` / `VolumeRemoved` emissions.  Also handles
the `List` method so GIO can recover state after daemon restart.

## Keyring schema

```
Schema:     org.gnome.proton.drive
Attributes: username  (string) — Proton account email
            field     (string) — uid | refresh_token | salted_passphrase
```

The watcher searches for `field=uid` to get one result per account regardless
of how many fields are stored.  All credential reads beyond account discovery
are done by the helper (`proton-drive-helper`), not the volume monitor.

## D-Bus interface (org.gnome.ProtonDrive)

Emitted by `proton-drive-setup` after a successful login:

```xml
<interface name="org.gnome.ProtonDrive">
  <signal name="AccountAdded">
    <arg name="username" type="s"/>
  </signal>
  <signal name="AccountRemoved">
    <arg name="username" type="s"/>
  </signal>
</interface>
```

Object path: `/org/gnome/ProtonDrive`  
Bus: session bus

## Build

```sh
cd volume-monitor
meson setup _build
ninja -C _build
sudo ninja -C _build install
```

The descriptor `proton.monitor` is installed into the GVfs remote volume
monitor directory (detected via pkg-config or overridden with
`-Dgvfs-remote-volume-monitor-dir=<path>`).

## Relationship to other components

| Component            | Dependency direction                         |
|----------------------|----------------------------------------------|
| `proton-drive-setup` | emits D-Bus signals consumed by this daemon  |
| `gvfsd-proton`       | spawned by GIO when the user mounts a volume |
| `proton-drive-helper`| spawned by `gvfsd-proton`, not this daemon   |

The volume monitor has no runtime dependency on `gvfsd-proton` or the helper.
It only watches credentials and emits volume lifecycle signals.
