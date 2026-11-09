package service

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/singularityos-lab/sinty-diskd/internal/model"
)

func privateSystemBus(t *testing.T) string {
	daemon, err := exec.LookPath("dbus-daemon")
	if err != nil {
		t.Skip("dbus-daemon not available")
	}
	dir, err := os.MkdirTemp("", "db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "bus")
	conf := filepath.Join(dir, "bus.conf")
	os.WriteFile(conf, []byte(`<!DOCTYPE busconfig PUBLIC "-//freedesktop//DTD D-BUS Bus Configuration 1.0//EN" "http://www.freedesktop.org/standards/dbus/1.0/busconfig.dtd">
<busconfig><type>system</type><listen>unix:path=`+sock+`</listen><auth>EXTERNAL</auth>
<policy context="default"><allow send_destination="*" eavesdrop="true"/><allow eavesdrop="true"/><allow own="*"/><allow user="*"/></policy></busconfig>`), 0o644)
	cmd := exec.Command(daemon, "--config-file="+conf, "--nofork", "--nopidfile", "--print-address=1")
	out, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Skip("dbus-daemon did not start")
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
	})
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(line)
}

func fakeUsbStick(t *testing.T) model.Sources {
	root := t.TempDir()
	usb := filepath.Join(root, "sys/devices/pci0000:00/0000:00:14.0/usb2/2-1")
	dev := filepath.Join(usb, "2-1:1.0/host6/target6:0:0/6:0:0:0/block/sdb")
	os.MkdirAll(filepath.Join(dev, "queue"), 0o755)
	os.WriteFile(filepath.Join(usb, "idVendor"), []byte("0781\n"), 0o644)
	for name, value := range map[string]string{"dev": "8:16", "size": "4194304", "removable": "1", "ro": "0",
		"queue/rotational": "1", "stat": "0 0 0 0 0 0 0 0 0 0 0"} {
		os.WriteFile(filepath.Join(dev, name), []byte(value+"\n"), 0o644)
	}
	os.MkdirAll(filepath.Join(root, "sys/class/block"), 0o755)
	os.Symlink(dev, filepath.Join(root, "sys/class/block/sdb"))
	udev := filepath.Join(root, "udev")
	os.MkdirAll(udev, 0o755)
	os.WriteFile(filepath.Join(udev, "b8:16"), []byte("E:ID_BUS=usb\nE:ID_VENDOR=SanDisk\nE:ID_MODEL=Cruzer_Blade\nE:ID_SERIAL_SHORT=4C530001\nE:DEVTYPE=disk\n"), 0o644)
	empty := filepath.Join(root, "empty")
	os.WriteFile(empty, nil, 0o644)
	return model.Sources{Sys: filepath.Join(root, "sys"), UdevData: udev, MountInfo: empty, Fstab: empty, Crypttab: empty, Swaps: empty}
}

type healthChange struct {
	state string
	alert bool
}

func TestHealthFollowsKernelOverTheBus(t *testing.T) {
	addr := privateSystemBus(t)
	t.Setenv("DBUS_SYSTEM_BUS_ADDRESS", addr)
	src := fakeUsbStick(t)
	s, err := New(src)
	if err != nil {
		t.Fatal(err)
	}
	kmsg := filepath.Join(t.TempDir(), "kmsg")
	if err := syscall.Mkfifo(kmsg, 0o600); err != nil {
		t.Fatal(err)
	}
	store := filepath.Join(t.TempDir(), "health.json")
	if err := s.StartHealth(store, kmsg); err != nil {
		t.Fatal(err)
	}
	s.health.devDir = t.TempDir()
	s.Refresh()

	client, err := dbus.Dial(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.Auth(nil); err != nil {
		t.Fatal(err)
	}
	if err := client.Hello(); err != nil {
		t.Fatal(err)
	}
	client.AddMatchSignal(dbus.WithMatchInterface(ifHealth), dbus.WithMatchMember("Changed"))
	signals := make(chan *dbus.Signal, 16)
	client.Signal(signals)

	w, err := os.OpenFile(kmsg, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	emit := func(line string) {
		w.WriteString("3,100,200,-;" + line + "\n")
		time.Sleep(60 * time.Millisecond)
	}
	next := func() healthChange {
		select {
		case sig := <-signals:
			return healthChange{sig.Body[1].(string), sig.Body[3].(bool)}
		case <-time.After(3 * time.Second):
			t.Fatal("no Changed signal")
		}
		return healthChange{}
	}

	emit("usb 2-1: new high-speed USB device number 3 using xhci_hcd")
	emit("I/O error, dev sdb, sector 2048 op 0x1:(WRITE) flags 0x800 phys_seg 1 prio class 2")
	if c := next(); c.state != "watch" || c.alert {
		t.Fatalf("one I/O error: %+v", c)
	}
	for i := 0; i < 4; i++ {
		emit("Buffer I/O error on dev sdb1, logical block 7, lost async page write")
	}
	if c := next(); c.state != "worn" || !c.alert {
		t.Fatalf("repeated I/O errors: %+v", c)
	}
	emit("FAT-fs (sdb1): Filesystem has been set read-only")
	if c := next(); c.state != "failing" || !c.alert {
		t.Fatalf("read-only remount: %+v", c)
	}

	obj := client.Object(healthName, healthPath)
	var drives []DriveHealth
	if err := obj.Call(ifHealth+".Drives", 0).Store(&drives); err != nil {
		t.Fatal(err)
	}
	if len(drives) != 1 || drives[0].Device != "/dev/sdb" || drives[0].Name != "SanDisk Cruzer Blade" ||
		drives[0].Serial != "4C530001" || drives[0].State != "failing" || drives[0].Size != 4194304*512 {
		t.Fatalf("drives %+v", drives)
	}
	var one DriveHealth
	if err := obj.Call(ifHealth+".Health", 0, "/dev/sdb1").Store(&one); err != nil || one.Key != drives[0].Key {
		t.Fatalf("health by partition: %+v %v", one, err)
	}
	if os.Getuid() != 0 {
		err := obj.Call(ifHealth+".Report", 0, "/dev/sdb", "startup-key", true, "").Err
		if e, ok := err.(dbus.Error); !ok || e.Name != "org.freedesktop.UDisks2.Error.NotAuthorized" {
			t.Fatalf("startup key report from a user: %v", err)
		}
		if err := obj.Call(ifHealth+".Report", 0, "/dev/sdb1", "backup", true, "").Err; err != nil {
			t.Fatalf("backup report from a user: %v", err)
		}
	}

	data, err := os.ReadFile(store)
	if err != nil {
		t.Fatal(err)
	}
	var saved []map[string]interface{}
	if err := json.Unmarshal(data, &saved); err != nil || len(saved) != 1 {
		t.Fatalf("history %s", data)
	}
	if saved[0]["serial"] != "4C530001" || saved[0]["alerted"] != "failing" || len(saved[0]["events"].([]interface{})) != 7 {
		t.Fatalf("history %s", data)
	}
	info, _ := os.Stat(store)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("history mode %v", info.Mode())
	}
}
