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
