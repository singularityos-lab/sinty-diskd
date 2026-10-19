package model

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestObjectPathEscapesLikeUDisks(t *testing.T) {
	for in, want := range map[string]string{"dm-0": "dm_2d0", "nvme0n1p2": "nvme0n1p2", "sr0": "sr0", "a.b": "a_2eb"} {
		if got := ObjectPath(in); got != want {
			t.Errorf("ObjectPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDecode(t *testing.T) {
	if got := Decode(`My\x20Disk\x2fA`); got != "My Disk/A" {
		t.Errorf("Decode = %q", got)
	}
}

func TestMountsByNumberAndSource(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mountinfo")
	os.WriteFile(path, []byte(
		"30 1 0:27 / / rw - btrfs /dev/mapper/vg-root rw\n"+
			"31 30 259:2 / /boot/efi rw - vfat /dev/nvme0n1p2 rw\n"+
			"32 30 0:27 /x /srv/with\\040space rw - btrfs /dev/mapper/vg-root rw\n"), 0o644)
	mounts := readMounts(path)
	root := &Block{Device: "/dev/dm-0", PreferredDevice: "/dev/vg/root", major: 254, minor: 0, props: map[string]string{"DM_NAME": "vg-root"}}
	if got := mountsOf(root, mounts); !reflect.DeepEqual(got, []string{"/", "/srv/with space"}) {
		t.Errorf("btrfs mounts %v", got)
	}
	esp := &Block{Device: "/dev/nvme0n1p2", PreferredDevice: "/dev/nvme0n1p2", major: 259, minor: 2, props: map[string]string{}}
	if got := mountsOf(esp, mounts); !reflect.DeepEqual(got, []string{"/boot/efi"}) {
		t.Errorf("esp mounts %v", got)
	}
}

func TestHideSystemLoops(t *testing.T) {
	m := &Model{Blocks: map[string]*Block{
		"loop0":     {Name: "loop0", disk: "loop0", Loop: &Loop{BackingFile: "/sysdata/boot/rootfs/rootfs-active.erofs"}},
		"loop1":     {Name: "loop1", disk: "loop1", Loop: &Loop{BackingFile: "/sysdata/boot/rootfs/rootfs-active.hash"}},
		"dm-0":      {Name: "dm-0", disk: "dm-0", slaves: []string{"loop0", "loop1"}},
		"loop2":     {Name: "loop2", disk: "loop2", Loop: &Loop{BackingFile: "/home/user/disk.iso"}},
		"loop2p1":   {Name: "loop2p1", disk: "loop2"},
		"loop3":     {Name: "loop3", disk: "loop3", Loop: &Loop{BackingFile: "/var/lib/sxi/app.img"}},
		"loop3p1":   {Name: "loop3p1", disk: "loop3"},
		"dm-1":      {Name: "dm-1", disk: "dm-1", slaves: []string{"nvme0n1p3"}},
		"nvme0n1":   {Name: "nvme0n1", disk: "nvme0n1"},
		"nvme0n1p3": {Name: "nvme0n1p3", disk: "nvme0n1"},
		"sda1":      {Name: "sda1", disk: "sda"},
	}}
	m.HideSystemLoops(func(name string) uint32 {
		if name == "loop2" {
			return 1000
		}
		return 0
	})
	want := map[string]bool{"loop0": true, "loop1": true, "dm-0": true, "loop3": true, "loop3p1": true}
	for name, b := range m.Blocks {
		if b.HintIgnore != want[name] {
			t.Errorf("%s HintIgnore = %v, want %v", name, b.HintIgnore, want[name])
		}
	}
}
