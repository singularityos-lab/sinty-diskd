package service

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/godbus/dbus/v5"

	"github.com/singularityos-lab/sinty-diskd/internal/model"
)

var mkfsTools = map[string]string{
	"ext2": "mkfs.ext2", "ext3": "mkfs.ext3", "ext4": "mkfs.ext4", "xfs": "mkfs.xfs", "vfat": "mkfs.vfat",
	"ntfs": "mkfs.ntfs", "f2fs": "mkfs.f2fs", "exfat": "mkfs.exfat", "btrfs": "mkfs.btrfs", "swap": "mkswap",
}

func have(tool string) bool {
	_, err := exec.LookPath(tool)
	return err == nil
}

func supportedFilesystems() []string {
	return []string{"ext2", "ext3", "ext4", "xfs", "vfat", "ntfs", "f2fs", "nilfs2", "exfat", "btrfs", "udf", "swap"}
}

func (s *Service) block(name string) *model.Block {
	if s.model == nil {
		return nil
	}
	return s.model.Blocks[name]
}

type blockObject struct {
	s    *Service
	name string
}

func (b *blockObject) Rescan(options map[string]dbus.Variant) *dbus.Error {
	b.s.Refresh()
	return nil
}

type filesystemObject struct {
	s    *Service
	name string
}

// allowed reports whether uid may act on b: root may act on every device,
// other users only on devices that are not part of the system.
func allowed(b *model.Block, uid uint32) bool {
	return uid == 0 || !b.HintSystem
}

func mountName(b *model.Block) string {
	name := b.IDLabel
	if name == "" {
		name = b.IDUUID
	}
	if name == "" {
		name = b.Name
	}
	return strings.Map(func(r rune) rune {
		if r == '/' || r < 0x20 {
			return '_'
		}
		return r
	}, name)
}

func optString(options map[string]dbus.Variant, key string) string {
	if v, ok := options[key]; ok {
		if s, ok := v.Value().(string); ok {
			return s
		}
	}
	return ""
}

var safeOptions = map[string]bool{
	"ro": true, "rw": true, "noatime": true, "relatime": true, "nodiratime": true, "sync": true, "dirsync": true,
	"noexec": true, "flush": true, "utf8": true, "shortname=mixed": true, "shortname=lower": true, "discard": true,
}

func (f *filesystemObject) Mount(sender dbus.Sender, options map[string]dbus.Variant) (string, *dbus.Error) {
	s := f.s
	uid, username, gid, err := s.caller(sender)
	if err != nil {
		return "", failed(err)
	}
	s.mu.Lock()
	b := s.block(f.name)
	if b == nil || b.Filesystem == nil {
		s.mu.Unlock()
		return "", failed(errors.New("not a filesystem"))
	}
	if len(b.Filesystem.MountPoints) > 0 {
		s.mu.Unlock()
		return "", dbus.NewError("org.freedesktop.UDisks2.Error.AlreadyMounted", []interface{}{"already mounted at " + b.Filesystem.MountPoints[0]})
	}
	if !allowed(b, uid) {
		s.mu.Unlock()
		return "", notAuthorized("mount a system device")
	}
	fstype := b.IDType
	if t := optString(options, "fstype"); t != "" {
		fstype = t
	}
	opts := []string{"nosuid", "nodev", "uhelper=udisks2"}
	for _, o := range strings.Split(optString(options, "options"), ",") {
		if o != "" {
			if !safeOptions[o] {
				s.mu.Unlock()
				return "", dbus.NewError("org.freedesktop.UDisks2.Error.OptionNotPermitted", []interface{}{"mount option not permitted: " + o})
			}
			opts = append(opts, o)
		}
	}
	switch fstype {
	case "vfat", "exfat", "ntfs", "ntfs3", "iso9660", "udf":
		opts = append(opts, "uid="+strconv.Itoa(int(uid)), "gid="+strconv.Itoa(int(gid)))
		if fstype == "vfat" {
			opts = append(opts, "shortname=mixed", "utf8=1", "showexec", "flush")
		}
	}
	base := "/run/media/root"
	if username != "" {
		base = "/run/media/" + username
	}
	dir := filepath.Join(base, mountName(b))
	for i := 1; ; i++ {
		if _, err := os.Lstat(dir); os.IsNotExist(err) {
			break
		}
		dir = filepath.Join(base, mountName(b)+strconv.Itoa(i))
	}
	device := b.Device
	s.mu.Unlock()

	os.MkdirAll("/run/media", 0o755)
	if err := os.MkdirAll(base, 0o750); err != nil {
		return "", failed(err)
	}
	os.Chown(base, 0, int(gid))
	os.Chmod(base, 0o750)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return "", failed(err)
	}
	os.Chown(dir, int(uid), int(gid))
	flags := uintptr(syscall.MS_NOSUID | syscall.MS_NODEV)
	data := strings.Join(filterOut(opts, "nosuid", "nodev", "ro", "rw", "uhelper=udisks2"), ",")
	if contains(opts, "ro") || b.ReadOnly {
		flags |= syscall.MS_RDONLY
	}
	if contains(opts, "noexec") {
		flags |= syscall.MS_NOEXEC
		data = strings.Join(filterOut(strings.Split(data, ","), "noexec"), ",")
	}
	if err := syscall.Mount(device, dir, fstype, flags, data); err != nil {
		os.Remove(dir)
		return "", failed(err)
	}
	s.Refresh()
	return dir, nil
}

func filterOut(list []string, drop ...string) []string {
	var out []string
	for _, x := range list {
		if !contains(drop, x) {
			out = append(out, x)
		}
	}
	return out
}

func (f *filesystemObject) Unmount(sender dbus.Sender, options map[string]dbus.Variant) *dbus.Error {
	s := f.s
	uid, _, _, err := s.caller(sender)
	if err != nil {
		return failed(err)
	}
	s.mu.Lock()
	b := s.block(f.name)
	if b == nil || b.Filesystem == nil || len(b.Filesystem.MountPoints) == 0 {
		s.mu.Unlock()
		return dbus.NewError("org.freedesktop.UDisks2.Error.NotMounted", []interface{}{"not mounted"})
	}
	if !allowed(b, uid) {
		s.mu.Unlock()
		return notAuthorized("unmount a system device")
	}
	points := append([]string(nil), b.Filesystem.MountPoints...)
	s.mu.Unlock()
	force := false
	if v, ok := options["force"]; ok {
		force, _ = v.Value().(bool)
	}
	for _, p := range points {
		flags := 0
		if force {
			flags = syscall.MNT_DETACH
		}
		if err := syscall.Unmount(p, flags); err != nil {
			if err == syscall.EBUSY {
				return dbus.NewError("org.freedesktop.UDisks2.Error.DeviceBusy", []interface{}{p + " is busy"})
			}
			return failed(err)
		}
		if strings.HasPrefix(p, "/run/media/") {
			os.Remove(p)
		}
	}
	s.Refresh()
	return nil
}

type driveObject struct {
	s      *Service
	object string
}

func (d *driveObject) drive() (*model.Drive, string) {
	if d.s.model == nil {
		return nil, ""
	}
	dr := d.s.model.Drives[d.object]
	if dr == nil {
		return nil, ""
	}
	for _, b := range d.s.model.Blocks {
		if b.Drive == drivesPrefix+d.object && b.Partition == nil {
			return dr, b.Name
		}
	}
	return dr, ""
}

func (d *driveObject) checkIdle(disk string) *dbus.Error {
	for _, b := range d.s.model.Blocks {
		if b.Drive == drivesPrefix+d.object && b.Filesystem != nil && len(b.Filesystem.MountPoints) > 0 {
			return dbus.NewError("org.freedesktop.UDisks2.Error.DeviceBusy", []interface{}{"/dev/" + b.Name + " is mounted"})
		}
	}
	return nil
}

func (d *driveObject) Eject(sender dbus.Sender, options map[string]dbus.Variant) *dbus.Error {
	uid, _, _, err := d.s.caller(sender)
	if err != nil {
		return failed(err)
	}
	d.s.mu.Lock()
	dr, disk := d.drive()
	if dr == nil || disk == "" {
		d.s.mu.Unlock()
		return failed(errors.New("no such drive"))
	}
	if uid != 0 && !dr.Removable {
		d.s.mu.Unlock()
		return notAuthorized("eject a system drive")
	}
	if e := d.checkIdle(disk); e != nil {
		d.s.mu.Unlock()
		return e
	}
	d.s.mu.Unlock()
	if out, err := exec.Command("eject", "/dev/"+disk).CombinedOutput(); err != nil {
		return failed(errors.New(strings.TrimSpace(string(out))))
	}
	d.s.Refresh()
	return nil
}

func (d *driveObject) PowerOff(sender dbus.Sender, options map[string]dbus.Variant) *dbus.Error {
	uid, _, _, err := d.s.caller(sender)
	if err != nil {
		return failed(err)
	}
	d.s.mu.Lock()
	dr, disk := d.drive()
	if dr == nil || disk == "" || !dr.CanPowerOff {
		d.s.mu.Unlock()
		return failed(errors.New("this drive cannot be powered off"))
	}
	if uid != 0 && !dr.Removable {
		d.s.mu.Unlock()
		return notAuthorized("power off a system drive")
	}
	if e := d.checkIdle(disk); e != nil {
		d.s.mu.Unlock()
		return e
	}
	sys := d.s.src.Sys
	d.s.mu.Unlock()
	syscall.Sync()
	dev, err := filepath.EvalSymlinks(filepath.Join(sys, "class/block", disk, "device"))
	if err != nil {
		return failed(err)
	}
	for dir := dev; dir != "/" && dir != "."; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, "remove")); err == nil && filepath.Base(filepath.Dir(dir)) != "" {
			if _, err := os.Stat(filepath.Join(dir, "idVendor")); err == nil {
				if err := os.WriteFile(filepath.Join(dir, "remove"), []byte("1"), 0); err != nil {
					return failed(err)
				}
				d.s.Refresh()
				return nil
			}
		}
	}
	return failed(errors.New("no USB device to power off"))
}
