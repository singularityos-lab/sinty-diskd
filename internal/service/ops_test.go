package service

import (
	"os"
	"strings"
	"testing"
	"unsafe"

	"github.com/godbus/dbus/v5"
)

func TestMkfsArgs(t *testing.T) {
	cases := []struct {
		fstype, label, tool, args string
	}{
		{"ext4", "Data", "mkfs.ext4", "-F -q -L Data /dev/sdb1"},
		{"vfat", "stick", "mkfs.vfat", "-n STICK /dev/sdb1"},
		{"exfat", "", "mkfs.exfat", "/dev/sdb1"},
		{"swap", "sw", "mkswap", "-L sw /dev/sdb1"},
		{"btrfs", "b", "mkfs.btrfs", "-f -L b /dev/sdb1"},
	}
	for _, c := range cases {
		tool, args, err := mkfsArgs(c.fstype, c.label, "/dev/sdb1")
		if err != nil || tool != c.tool || strings.Join(args, " ") != c.args {
			t.Errorf("%s: %s %v %v", c.fstype, tool, args, err)
		}
	}
	if _, _, err := mkfsArgs("vfat", "TWELVE_CHARS", "/dev/x"); err == nil {
		t.Error("a vfat label longer than 11 bytes was accepted")
	}
	if _, _, err := mkfsArgs("reiserfs", "", "/dev/x"); err == nil {
		t.Error("an unsupported filesystem was accepted")
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
