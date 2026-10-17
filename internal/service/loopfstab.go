package service

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"github.com/godbus/dbus/v5"
)

const (
	loopCtlGetFree  = 0x4C82
	loopConfigure   = 0x4C0A
	loopClrFd       = 0x4C01
	loopSetStatus64 = 0x4C04
	loopGetStatus64 = 0x4C05
	loFlagsReadOnly = 1
	loFlagsAutoclr  = 4
	loFlagsPartscan = 8
)

type loopInfo64 struct {
	device, inode, rdevice, offset, sizelimit  uint64
	number, encryptType, encryptKeySize, flags uint32
	fileName, cryptName                        [64]byte
	encryptKey                                 [32]byte
	init                                       [2]uint64
}

type loopConfig struct {
	fd, blockSize uint32
	info          loopInfo64
	reserved      [8]uint64
}

func ioctlPtr(fd uintptr, req uintptr, p unsafe.Pointer) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, req, uintptr(p)); errno != 0 {
		return errno
	}
	return nil
}

func optUint64(options map[string]dbus.Variant, key string) uint64 {
	if v, ok := options[key]; ok {
		if n, ok := v.Value().(uint64); ok {
			return n
		}
	}
	return 0
}

func (m *managerObject) LoopSetup(sender dbus.Sender, fd dbus.UnixFD, options map[string]dbus.Variant) (dbus.ObjectPath, *dbus.Error) {
	s := m.s
	defer syscall.Close(int(fd))
	uid, _, _, err := s.caller(sender)
	if err != nil {
		return "", failed(err)
	}
	if uid != 0 && uid < 1000 {
		return "", notAuthorized("set up a loop device")
	}
	backing, _ := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", fd))
	ctl, err := os.OpenFile("/dev/loop-control", os.O_RDWR, 0)
	if err != nil {
		return "", failed(err)
	}
	defer ctl.Close()
	n, _, errno := syscall.Syscall(syscall.SYS_IOCTL, ctl.Fd(), loopCtlGetFree, 0)
	if errno != 0 {
		return "", failed(errno)
	}
	name := "loop" + strconv.Itoa(int(n))
	dev, err := os.OpenFile("/dev/"+name, os.O_RDWR, 0)
	if err != nil {
		return "", failed(err)
	}
	defer dev.Close()
	cfg := loopConfig{fd: uint32(fd)}
	cfg.info.offset = optUint64(options, "offset")
	cfg.info.sizelimit = optUint64(options, "size")
	if optBool(options, "read-only") {
		cfg.info.flags |= loFlagsReadOnly
	}
	if !optBool(options, "no-part-scan") {
		cfg.info.flags |= loFlagsPartscan
	}
	copy(cfg.info.fileName[:63], backing)
	if err := ioctlPtr(dev.Fd(), loopConfigure, unsafe.Pointer(&cfg)); err != nil {
		return "", failed(err)
	}
	s.mu.Lock()
	s.loopOwner[name] = uid
	s.mu.Unlock()
	run("udevadm", "settle", "--timeout=10")
	s.Refresh()
	return dbus.ObjectPath("/org/freedesktop/UDisks2/block_devices/" + name), nil
}

type loopObject struct {
	s    *Service
	name string
}

func (l *loopObject) authorized(sender dbus.Sender) *dbus.Error {
	uid, _, _, err := l.s.caller(sender)
	if err != nil {
		return failed(err)
	}
	l.s.mu.Lock()
	owner, ok := l.s.loopOwner[l.name]
	l.s.mu.Unlock()
	if uid != 0 && (!ok || owner != uid) {
		return notAuthorized("change a loop device set up by someone else")
	}
	return nil
}

func (l *loopObject) Delete(sender dbus.Sender, options map[string]dbus.Variant) *dbus.Error {
	if err := l.authorized(sender); err != nil {
		return err
	}
	l.s.mu.Lock()
	b := l.s.block(l.name)
	var derr *dbus.Error
	if b != nil {
		derr = l.s.busy(b)
	}
	l.s.mu.Unlock()
	if derr != nil {
		return derr
	}
	dev, err := os.OpenFile("/dev/"+l.name, os.O_RDONLY, 0)
	if err != nil {
		return failed(err)
	}
	defer dev.Close()
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, dev.Fd(), loopClrFd, 0); errno != 0 {
		return failed(errno)
	}
	l.s.mu.Lock()
	delete(l.s.loopOwner, l.name)
	l.s.mu.Unlock()
	l.s.Refresh()
	return nil
}

func (l *loopObject) SetAutoclear(sender dbus.Sender, value bool, options map[string]dbus.Variant) *dbus.Error {
	if err := l.authorized(sender); err != nil {
		return err
	}
	dev, err := os.OpenFile("/dev/"+l.name, os.O_RDONLY, 0)
	if err != nil {
		return failed(err)
	}
	defer dev.Close()
	var info loopInfo64
	if err := ioctlPtr(dev.Fd(), loopGetStatus64, unsafe.Pointer(&info)); err != nil {
		return failed(err)
	}
	if value {
		info.flags |= loFlagsAutoclr
	} else {
		info.flags &^= loFlagsAutoclr
	}
	if err := ioctlPtr(dev.Fd(), loopSetStatus64, unsafe.Pointer(&info)); err != nil {
		return failed(err)
	}
	l.s.Refresh()
	return nil
}

func fieldString(fields map[string]dbus.Variant, key string) string {
	v, ok := fields[key]
	if !ok {
		return ""
	}
	switch x := v.Value().(type) {
	case []byte:
		return strings.TrimRight(string(x), "\x00")
	case string:
		return x
	}
	return ""
}

func fieldInt(fields map[string]dbus.Variant, key string) int {
	if v, ok := fields[key]; ok {
		switch x := v.Value().(type) {
		case int32:
			return int(x)
		case uint32:
			return int(x)
		}
	}
	return 0
}

func escapeTab(s string) string {
	r := strings.NewReplacer(" ", `\040`, "\t", `\011`, "\n", `\012`, `\`, `\134`)
	return r.Replace(s)
}

func fstabLine(fields map[string]dbus.Variant) (string, error) {
	fsname, dir := fieldString(fields, "fsname"), fieldString(fields, "dir")
	typ, opts := fieldString(fields, "type"), fieldString(fields, "opts")
	if fsname == "" || dir == "" || !filepath.IsAbs(dir) && dir != "none" {
		return "", errors.New("an fstab item needs fsname and an absolute dir")
	}
	if typ == "" {
		typ = "auto"
	}
	if opts == "" {
		opts = "defaults"
	}
	return fmt.Sprintf("%s %s %s %s %d %d", escapeTab(fsname), escapeTab(dir), escapeTab(typ), escapeTab(opts),
		fieldInt(fields, "freq"), fieldInt(fields, "passno")), nil
}

func sameEntry(line string, fields map[string]dbus.Variant) bool {
	f := strings.Fields(line)
	return len(f) >= 2 && !strings.HasPrefix(line, "#") &&
		f[0] == escapeTab(fieldString(fields, "fsname")) && f[1] == escapeTab(fieldString(fields, "dir"))
}

func editFstab(fstabPath string, remove, add map[string]dbus.Variant) error {
	data, err := os.ReadFile(fstabPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(data) == 0 {
		lines = nil
	}
	var out []string
	removed := remove == nil
	for _, l := range lines {
		if !removed && sameEntry(strings.TrimSpace(l), remove) {
			removed = true
			continue
		}
		out = append(out, l)
	}
	if !removed {
		return errors.New("the fstab item was not found")
	}
	if add != nil {
		line, err := fstabLine(add)
		if err != nil {
			return err
		}
		out = append(out, line)
	}
	tmp := fstabPath + ".sinty-diskd"
	if err := os.WriteFile(tmp, []byte(strings.Join(out, "\n")+"\n"), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, fstabPath)
}

func (b *blockObject) configItem(sender dbus.Sender, item confItem, remove bool, replacement *confItem) *dbus.Error {
	uid, _, _, err := b.s.caller(sender)
	if err != nil {
		return failed(err)
	}
	if uid != 0 {
		return notAuthorized("change the system configuration")
	}
	if item.Kind != "fstab" || (replacement != nil && replacement.Kind != "fstab") {
		return dbus.NewError("org.freedesktop.UDisks2.Error.NotSupported", []interface{}{"only fstab items are supported"})
	}
	var rm, add map[string]dbus.Variant
	if remove {
		rm = item.Fields
		if replacement != nil {
			add = replacement.Fields
		}
	} else {
		add = item.Fields
	}
	if err := editFstab(b.s.src.Fstab, rm, add); err != nil {
		return failed(err)
	}
	b.s.Refresh()
	return nil
}

func (b *blockObject) AddConfigurationItem(sender dbus.Sender, item confItem, options map[string]dbus.Variant) *dbus.Error {
	return b.configItem(sender, item, false, nil)
}

func (b *blockObject) RemoveConfigurationItem(sender dbus.Sender, item confItem, options map[string]dbus.Variant) *dbus.Error {
	return b.configItem(sender, item, true, nil)
}

func (b *blockObject) UpdateConfigurationItem(sender dbus.Sender, old, updated confItem, options map[string]dbus.Variant) *dbus.Error {
	return b.configItem(sender, old, true, &updated)
}
