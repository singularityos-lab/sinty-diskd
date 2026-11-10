package service

import (
	"os"
	"strings"
	"testing"
	"unsafe"

	"github.com/godbus/dbus/v5"
	core "github.com/singularityos-lab/libsingularity/src/system/core/go/sintysystem"
)

func TestMkfsArgs(t *testing.T) {
	cases := []struct {
		fstype, label, tool, args string
	}{
		{"ext3", "Data", "mkfs.ext3", "-F -q -L Data /dev/sdb1"},
		{"ntfs", "", "mkfs.ntfs", "-f -F /dev/sdb1"},
		{"btrfs", "b", "mkfs.btrfs", "-f -L b /dev/sdb1"},
		{"f2fs", "f", "mkfs.f2fs", "-f -l f /dev/sdb1"},
	}
	for _, c := range cases {
		tool, args := mkfsArgs(c.fstype, c.label, "/dev/sdb1")
		if tool != c.tool || strings.Join(args, " ") != c.args {
			t.Errorf("%s: %s %v", c.fstype, tool, args)
		}
	}
	if err := checkFormat("vfat", "TWELVE_CHARS"); err == nil {
		t.Error("a vfat label longer than 11 bytes was accepted")
	}
	if err := checkFormat("reiserfs", ""); err == nil {
		t.Error("an unsupported filesystem was accepted")
	}
}

func TestMakeFilesystemOnImage(t *testing.T) {
	path := t.TempDir() + "/fs.img"
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, 64<<20); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ fstype, label, want string }{
		{"ext4", "data", "data"}, {"vfat", "stick", "STICK"}, {"exfat", "Média", "Média"}, {"swap", "sw", "sw"},
	} {
		if err := makeFilesystem(c.fstype, c.label, path); err != nil {
			t.Fatalf("%s: %v", c.fstype, err)
		}
		info, err := core.ProbeFilesystem(path)
		if err != nil || info.Type != c.fstype || info.Label != c.want {
			t.Fatalf("%s: %+v %v", c.fstype, info, err)
		}
	}
}

func TestValidPartType(t *testing.T) {
	for typ, want := range map[string]bool{
		"0fc63daf-8483-4772-8e79-3d69d8477de4": true, "0x0c": true, "0x83": true,
		"0x100": false, "linux": false, "0fc63daf-8483-4772-8e79-3d69d8477de$": false, "": false,
	} {
		if validPartType(typ) != want {
			t.Errorf("validPartType(%q) != %v", typ, want)
		}
	}
}

func TestEditFstab(t *testing.T) {
	path := t.TempDir() + "/fstab"
	os.WriteFile(path, []byte("# comment\nUUID=a / ext4 defaults 0 1\n"), 0o644)
	item := map[string]dbus.Variant{
		"fsname": dbus.MakeVariant([]byte("UUID=b\x00")), "dir": dbus.MakeVariant([]byte("/mnt/My Data\x00")),
		"type": dbus.MakeVariant([]byte("ext4\x00")), "opts": dbus.MakeVariant([]byte("nofail\x00")),
		"freq": dbus.MakeVariant(int32(0)), "passno": dbus.MakeVariant(int32(2)),
	}
	if err := editFstab(path, nil, item); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if !strings.HasSuffix(string(data), "UUID=b /mnt/My\\040Data ext4 nofail 0 2\n") {
		t.Errorf("after add:\n%s", data)
	}
	if err := editFstab(path, item, nil); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(path)
	if string(data) != "# comment\nUUID=a / ext4 defaults 0 1\n" {
		t.Errorf("after remove:\n%s", data)
	}
	if err := editFstab(path, item, nil); err == nil {
		t.Error("removing a missing item succeeded")
	}
}

func TestLoopConfigMatchesKernelLayout(t *testing.T) {
	if s := unsafe.Sizeof(loopConfig{}); s != 304 {
		t.Fatalf("loop_config is %d bytes, the kernel uses 304", s)
	}
}
