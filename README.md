# sinty-diskd

The disk daemon of SintyOS. It publishes the drives, block devices,
partitions, filesystems and swap of the machine on `org.freedesktop.UDisks2`,
with the same objects and properties UDisks2 publishes, and mounts, unmounts,
ejects and powers off removable media for the user.

## Why it exists

SintyOS replaces the system daemons it inherited with small ones that share
one view of the hardware: sinty-devd owns the devices and the udev database,
and sinty-diskd builds its disk model from that database instead of carrying
its own block device library.

Clients do not change. libudisks2, the GIO volume monitor and the desktop
find the same object manager, objects and properties on the system bus.

## What it implements

- **The object manager** on `/org/freedesktop/UDisks2` with
  `GetManagedObjects`, `InterfacesAdded` and `InterfacesRemoved`, updated on
  block device uevents and mount changes.
- **`Manager`**: `Version`, `SupportedFilesystems`,
  `SupportedEncryptionTypes`, `DefaultEncryptionType`, `GetBlockDevices`,
  `ResolveDevice` and `CanFormat`.
- **`Block`, `Partition`, `PartitionTable`, `Filesystem`, `Swapspace`,
  `Loop` and `Drive`**, with every property UDisks2 publishes for them: the
  device names and links, sizes, the identity from the udev database, the
  `UDISKS_*` hints, the fstab configuration, partition geometry and flags,
  mount points and the drive identity, media and connection. On a real
  machine the published objects and values match UDisks2 2.11 one for one.
- **Actions**: `Filesystem.Mount` under `/run/media/<user>` with the safe
  default options UDisks2 uses, `Filesystem.Unmount`, `Drive.Eject`,
  `Drive.PowerOff` for USB drives and `Block.Rescan`. Users may act on
  removable media; system devices are reserved to root.

Not implemented yet: formatting, partitioning, the SMART and NVMe
interfaces, encryption and loop device setup. Their methods are not
published, so clients see them as unavailable rather than failing.

## Usage

    sinty-diskd [--udev-data /run/udev/data]

## Compatibility

`go test ./internal/model` compares the model with what UDisks2 published on
the same machine:

    SINTY_DISKD_REF=/path/to/capture go test ./internal/model

The capture directory holds the udev database, mountinfo, fstab, swaps and
the `GetManagedObjects` reply of UDisks2 in `managed-s.json`.

## Build

    make build            # ./sinty-diskd
    make static           # stripped static binary for the image
    make test
    go build ./...        # plain build

Requires Go >= 1.25 and Linux. The non-stdlib dependency is
`github.com/godbus/dbus/v5`.
