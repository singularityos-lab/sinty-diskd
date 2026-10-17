package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/godbus/dbus/v5"

	"github.com/singularityos-lab/sinty-diskd/internal/model"
)

func run(name string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL=C"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("%s: %s", name, msg)
	}
	return nil
}

func runInput(input, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL=C"}
	cmd.Stdin = strings.NewReader(input)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %s", name, strings.TrimSpace(string(out)))
	}
	return nil
}

func optBool(options map[string]dbus.Variant, key string) bool {
	if v, ok := options[key]; ok {
		b, _ := v.Value().(bool)
		return b
	}
	return false
}

// mayModify reports whether uid may change b: root may change any device,
// a user the devices that are not part of the system.
func (s *Service) mayModify(sender dbus.Sender, b *model.Block) (uint32, *dbus.Error) {
	uid, _, _, err := s.caller(sender)
	if err != nil {
		return 0, failed(err)
	}
	if uid != 0 && (b.HintSystem || uid < 1000) {
		return uid, notAuthorized("modify /dev/" + b.Name)
	}
	return uid, nil
}

func (s *Service) busy(b *model.Block) *dbus.Error {
	for _, x := range s.model.Blocks {
		if x.Name == b.Name || (b.PartitionTable != nil && x.Partition != nil && x.Partition.Table == model.BlockPath(b.Name)) {
			if x.Filesystem != nil && len(x.Filesystem.MountPoints) > 0 {
				return dbus.NewError("org.freedesktop.UDisks2.Error.DeviceBusy", []interface{}{"/dev/" + x.Name + " is mounted"})
			}
			if x.Swapspace != nil && x.Swapspace.Active {
				return dbus.NewError("org.freedesktop.UDisks2.Error.DeviceBusy", []interface{}{"/dev/" + x.Name + " is in use as swap"})
			}
		}
	}
	return nil
}

func validLabel(label string, max int) error {
	if !utf8.ValidString(label) || strings.ContainsAny(label, "\x00\n") || len(label) > max {
		return errors.New("invalid label")
	}
	return nil
}

// mkfsArgs builds the command that creates a filesystem of fstype with label.
func mkfsArgs(fstype, label, dev string) (string, []string, error) {
	limits := map[string]int{"ext2": 16, "ext3": 16, "ext4": 16, "vfat": 11, "exfat": 15, "ntfs": 128, "btrfs": 255, "xfs": 12, "f2fs": 512, "swap": 16}
	max, ok := limits[fstype]
	if !ok {
		return "", nil, fmt.Errorf("formatting as %s is not supported", fstype)
	}
	if err := validLabel(label, max); err != nil {
		return "", nil, err
	}
	var args []string
	switch fstype {
	case "ext2", "ext3", "ext4":
		args = []string{"-F", "-q"}
		if label != "" {
			args = append(args, "-L", label)
		}
	case "vfat":
		if label != "" {
			args = append(args, "-n", strings.ToUpper(label))
		}
	case "exfat":
		if label != "" {
			args = append(args, "-L", label)
		}
	case "ntfs":
		args = []string{"-f", "-F"}
		if label != "" {
			args = append(args, "-L", label)
		}
	case "btrfs", "xfs":
		args = []string{"-f"}
		if label != "" {
			args = append(args, "-L", label)
		}
	case "f2fs":
		args = []string{"-f"}
		if label != "" {
			args = append(args, "-l", label)
		}
	case "swap":
		if label != "" {
			args = append(args, "-L", label)
		}
		return "mkswap", append(args, dev), nil
	}
	return mkfsTools[fstype], append(args, dev), nil
}

// gptTypes is the partition type a fresh filesystem gets when the caller asks
// to update the partition type.
var gptTypes = map[string]string{
	"vfat": "ebd0a0a2-b9e5-4433-87c0-68b6b72699c7", "exfat": "ebd0a0a2-b9e5-4433-87c0-68b6b72699c7",
	"ntfs": "ebd0a0a2-b9e5-4433-87c0-68b6b72699c7", "swap": "0657fd6d-a4ab-43c4-84e5-0933c84b4f4f",
	"ext2": "0fc63daf-8483-4772-8e79-3d69d8477de4", "ext3": "0fc63daf-8483-4772-8e79-3d69d8477de4",
	"ext4": "0fc63daf-8483-4772-8e79-3d69d8477de4", "btrfs": "0fc63daf-8483-4772-8e79-3d69d8477de4",
	"xfs": "0fc63daf-8483-4772-8e79-3d69d8477de4", "f2fs": "0fc63daf-8483-4772-8e79-3d69d8477de4",
}

var dosTypes = map[string]string{"vfat": "0x0c", "exfat": "0x07", "ntfs": "0x07", "swap": "0x82",
	"ext2": "0x83", "ext3": "0x83", "ext4": "0x83", "btrfs": "0x83", "xfs": "0x83", "f2fs": "0x83"}

func (b *blockObject) Format(sender dbus.Sender, fstype string, options map[string]dbus.Variant) *dbus.Error {
	s := b.s
	s.mu.Lock()
	blk := s.block(b.name)
	if blk == nil {
		s.mu.Unlock()
		return failed(errors.New("no such device"))
	}
	uid, derr := s.mayModify(sender, blk)
	if derr == nil {
		derr = s.busy(blk)
	}
	var table *model.Block
	if blk.Partition != nil {
		for _, x := range s.model.Blocks {
			if model.BlockPath(x.Name) == blk.Partition.Table {
				table = x
			}
		}
	}
	part := blk.Partition
	s.mu.Unlock()
	if derr != nil {
		return derr
	}
	dev := "/dev/" + b.name
	if err := run("wipefs", "-a", "-q", dev); err != nil {
		return failed(err)
	}
	if optString(options, "erase") == "zero" {
		if err := zero(dev); err != nil {
			return failed(err)
		}
	}
	switch fstype {
	case "empty":
	case "dos", "gpt":
		if err := runInput("label: "+fstype+"\n", "sfdisk", "--quiet", "--wipe", "always", dev); err != nil {
			return failed(err)
		}
	default:
		target := dev
		if v, ok := options["encrypt.passphrase"]; ok {
			pass, _ := v.Value().(string)
			clear, name, err := formatEncrypted(dev, b.name, pass)
			if err != nil {
				return failed(err)
			}
			defer cryptsetup(nil, "close", name)
			target = clear
		}
		tool, args, err := mkfsArgs(fstype, optString(options, "label"), target)
		if err != nil {
			return dbus.NewError("org.freedesktop.UDisks2.Error.NotSupported", []interface{}{err.Error()})
		}
		if err := run(tool, args...); err != nil {
			return failed(err)
		}
		if optBool(options, "update-partition-type") && part != nil && table != nil && table.PartitionTable != nil {
			t := gptTypes[fstype]
			if table.PartitionTable.Type == "dos" {
				t = dosTypes[fstype]
			}
			if t != "" {
				run("sfdisk", "--part-type", "/dev/"+table.Name, strconv.Itoa(int(part.Number)), t)
			}
		}
		if optBool(options, "take-ownership") && uid != 0 {
			if err := takeOwnership(target, fstype, uid); err != nil {
				return failed(err)
			}
		}
	}
	run("udevadm", "trigger", "--action=change", "--sysname-match="+b.name)
	run("udevadm", "settle", "--timeout=10")
	s.Refresh()
	return nil
}

func zero(dev string) error {
	f, err := os.OpenFile(dev, os.O_WRONLY|syscall.O_EXCL, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	buf := make([]byte, 1<<20)
	for {
		if _, err := f.Write(buf); err != nil {
			if errors.Is(err, syscall.ENOSPC) {
				return f.Sync()
			}
			return err
		}
	}
}

// takeOwnership gives the root directory of a fresh filesystem to uid, so a
// user formatting a stick can write to it without root.
func takeOwnership(dev, fstype string, uid uint32) error {
	switch fstype {
	case "vfat", "exfat", "ntfs", "swap":
		return nil
	}
	dir, err := os.MkdirTemp("/run", "sinty-diskd-")
	if err != nil {
		return err
	}
	defer os.Remove(dir)
	if err := syscall.Mount(dev, dir, fstype, syscall.MS_NOSUID|syscall.MS_NODEV, ""); err != nil {
		return err
	}
	defer syscall.Unmount(dir, 0)
	gid := uid
	return os.Chown(dir, int(uid), int(gid))
}

func (b *blockObject) partition(sender dbus.Sender) (*model.Block, *model.Block, *dbus.Error) {
	s := b.s
	s.mu.Lock()
	defer s.mu.Unlock()
	blk := s.block(b.name)
	if blk == nil || blk.Partition == nil {
		return nil, nil, failed(errors.New("not a partition"))
	}
	var table *model.Block
	for _, x := range s.model.Blocks {
		if model.BlockPath(x.Name) == blk.Partition.Table {
			table = x
		}
	}
	if table == nil {
		return nil, nil, failed(errors.New("partition table not found"))
	}
	if _, err := s.mayModify(sender, blk); err != nil {
		return nil, nil, err
	}
	return blk, table, nil
}

type partitionObject struct{ blockObject }

func (p *partitionObject) Delete(sender dbus.Sender, options map[string]dbus.Variant) *dbus.Error {
	blk, table, err := p.partition(sender)
	if err != nil {
		return err
	}
	p.s.mu.Lock()
	err = p.s.busy(blk)
	p.s.mu.Unlock()
	if err != nil {
		return err
	}
	if e := run("sfdisk", "--quiet", "--delete", "/dev/"+table.Name, strconv.Itoa(int(blk.Partition.Number))); e != nil {
		return failed(e)
	}
	p.s.settle(table.Name)
	return nil
}

func (p *partitionObject) SetType(sender dbus.Sender, typ string, options map[string]dbus.Variant) *dbus.Error {
	blk, table, err := p.partition(sender)
	if err != nil {
		return err
	}
	if !validPartType(typ) {
		return failed(errors.New("invalid partition type"))
	}
	if e := run("sfdisk", "--quiet", "--part-type", "/dev/"+table.Name, strconv.Itoa(int(blk.Partition.Number)), typ); e != nil {
		return failed(e)
	}
	p.s.settle(table.Name)
	return nil
}

func validPartType(t string) bool {
	if len(t) == 36 {
		return strings.Count(t, "-") == 4 && strings.Trim(strings.ToLower(t), "0123456789abcdef-") == ""
	}
	if strings.HasPrefix(t, "0x") {
		_, err := strconv.ParseUint(t[2:], 16, 8)
		return err == nil
	}
	return false
}

func (p *partitionObject) SetName(sender dbus.Sender, name string, options map[string]dbus.Variant) *dbus.Error {
	blk, table, err := p.partition(sender)
	if err != nil {
		return err
	}
	if e := validLabel(name, 36); e != nil {
		return failed(e)
	}
	if e := run("sfdisk", "--quiet", "--part-label", "/dev/"+table.Name, strconv.Itoa(int(blk.Partition.Number)), name); e != nil {
		return failed(e)
	}
	p.s.settle(table.Name)
	return nil
}

func (p *partitionObject) SetFlags(sender dbus.Sender, flags uint64, options map[string]dbus.Variant) *dbus.Error {
	blk, table, err := p.partition(sender)
	if err != nil {
		return err
	}
	num := strconv.Itoa(int(blk.Partition.Number))
	var e error
	if table.PartitionTable.Type == "gpt" {
		var attrs []string
		for bit := 0; bit < 64; bit++ {
			if flags&(1<<bit) == 0 {
				continue
			}
			switch bit {
			case 0:
				attrs = append(attrs, "RequiredPartition")
			case 1:
				attrs = append(attrs, "NoBlockIOProtocol")
			case 2:
				attrs = append(attrs, "LegacyBIOSBootable")
			default:
				attrs = append(attrs, strconv.Itoa(bit))
			}
		}
		e = run("sfdisk", "--quiet", "--part-attrs", "/dev/"+table.Name, num, strings.Join(attrs, ","))
	} else {
		if flags&0x80 != 0 {
			e = run("sfdisk", "--quiet", "--activate", "/dev/"+table.Name, num)
		} else {
			e = run("sfdisk", "--quiet", "--activate", "/dev/"+table.Name, "-")
		}
	}
	if e != nil {
		return failed(e)
	}
	p.s.settle(table.Name)
	return nil
}

func (p *partitionObject) Resize(sender dbus.Sender, size uint64, options map[string]dbus.Variant) *dbus.Error {
	blk, table, err := p.partition(sender)
	if err != nil {
		return err
	}
	sectors := size / 512
	if size == 0 {
		if e := runInput(", +\n", "sfdisk", "--quiet", "--no-reread", "-N", strconv.Itoa(int(blk.Partition.Number)), "/dev/"+table.Name); e != nil {
			return failed(e)
		}
	} else if e := runInput(fmt.Sprintf(", %d\n", sectors), "sfdisk", "--quiet", "--no-reread", "-N", strconv.Itoa(int(blk.Partition.Number)), "/dev/"+table.Name); e != nil {
		return failed(e)
	}
	run("partx", "-u", "/dev/"+table.Name)
	p.s.settle(table.Name)
	return nil
}

type tableObject struct{ blockObject }

func (t *tableObject) create(sender dbus.Sender, offset, size uint64, typ, name string) (dbus.ObjectPath, *dbus.Error) {
	s := t.s
	s.mu.Lock()
	blk := s.block(t.name)
	if blk == nil || blk.PartitionTable == nil {
		s.mu.Unlock()
		return "", failed(errors.New("no partition table"))
	}
	if _, err := s.mayModify(sender, blk); err != nil {
		s.mu.Unlock()
		return "", err
	}
	before := map[uint32]bool{}
	for _, x := range s.model.Blocks {
		if x.Partition != nil && x.Partition.Table == model.BlockPath(blk.Name) {
			before[x.Partition.Number] = true
		}
	}
	tableType := blk.PartitionTable.Type
	s.mu.Unlock()
	if typ != "" && !validPartType(typ) {
		return "", failed(errors.New("invalid partition type"))
	}
	const mib = 1 << 20
	start := (offset + mib - 1) / mib * mib
	if start < mib {
		start = mib
	}
	line := fmt.Sprintf("start=%d", start/512)
	if size > 0 {
		line += fmt.Sprintf(", size=%d", (size-(start-offset))/512)
	}
	if typ != "" {
		line += ", type=" + typ
	}
	if name != "" && tableType == "gpt" {
		if err := validLabel(name, 36); err != nil {
			return "", failed(err)
		}
		line += ", name=\"" + strings.ReplaceAll(name, "\"", "") + "\""
	}
	if err := runInput(line+"\n", "sfdisk", "--quiet", "--append", "/dev/"+t.name); err != nil {
		return "", failed(err)
	}
	s.settle(t.name)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, x := range s.model.Blocks {
		if x.Partition != nil && x.Partition.Table == model.BlockPath(t.name) && !before[x.Partition.Number] {
			return dbus.ObjectPath(model.BlockPath(x.Name)), nil
		}
	}
	return "", failed(errors.New("the new partition did not appear"))
}

func (t *tableObject) CreatePartition(sender dbus.Sender, offset, size uint64, typ, name string, options map[string]dbus.Variant) (dbus.ObjectPath, *dbus.Error) {
	return t.create(sender, offset, size, typ, name)
}

func (t *tableObject) CreatePartitionAndFormat(sender dbus.Sender, offset, size uint64, typ, name string, options map[string]dbus.Variant, fstype string, formatOptions map[string]dbus.Variant) (dbus.ObjectPath, *dbus.Error) {
	path, err := t.create(sender, offset, size, typ, name)
	if err != nil {
		return "", err
	}
	part := filepath.Base(string(path))
	t.s.mu.Lock()
	var dev string
	for _, x := range t.s.model.Blocks {
		if model.ObjectPath(x.Name) == part {
			dev = x.Name
		}
	}
	t.s.mu.Unlock()
	if err := (&blockObject{t.s, dev}).Format(sender, fstype, formatOptions); err != nil {
		return "", err
	}
	return path, nil
}

// settle waits for the kernel and sinty-devd to catch up with a change to the
// partitions of disk, then republishes.
func (s *Service) settle(disk string) {
	run("partx", "-u", "/dev/"+disk)
	run("udevadm", "trigger", "--action=change", "--sysname-match="+disk+"*")
	run("udevadm", "settle", "--timeout=10")
	s.Refresh()
}

func (f *filesystemObject) SetLabel(sender dbus.Sender, label string, options map[string]dbus.Variant) *dbus.Error {
	s := f.s
	s.mu.Lock()
	blk := s.block(f.name)
	if blk == nil || blk.Filesystem == nil {
		s.mu.Unlock()
		return failed(errors.New("not a filesystem"))
	}
	_, derr := s.mayModify(sender, blk)
	fstype := blk.IDType
	s.mu.Unlock()
	if derr != nil {
		return derr
	}
	dev := "/dev/" + f.name
	var err error
	switch fstype {
	case "ext2", "ext3", "ext4":
		if err = validLabel(label, 16); err == nil {
			err = run("e2label", dev, label)
		}
	case "vfat":
		if err = validLabel(label, 11); err == nil {
			err = run("fatlabel", dev, strings.ToUpper(label))
		}
	case "exfat":
		if err = validLabel(label, 15); err == nil {
			err = run("exfatlabel", dev, label)
		}
	case "btrfs":
		if err = validLabel(label, 255); err == nil {
			err = run("btrfs", "filesystem", "label", dev, label)
		}
	case "xfs":
		if err = validLabel(label, 12); err == nil {
			err = run("xfs_admin", "-L", label, dev)
		}
	case "ntfs":
		if err = validLabel(label, 128); err == nil {
			err = run("ntfslabel", dev, label)
		}
	default:
		return dbus.NewError("org.freedesktop.UDisks2.Error.NotSupported", []interface{}{"cannot label " + fstype})
	}
	if err != nil {
		return failed(err)
	}
	s.settle(f.name)
	return nil
}

func (f *filesystemObject) fsck(sender dbus.Sender, repair bool) (bool, *dbus.Error) {
	s := f.s
	s.mu.Lock()
	blk := s.block(f.name)
	if blk == nil || blk.Filesystem == nil {
		s.mu.Unlock()
		return false, failed(errors.New("not a filesystem"))
	}
	_, derr := s.mayModify(sender, blk)
	if derr == nil && len(blk.Filesystem.MountPoints) > 0 {
		derr = dbus.NewError("org.freedesktop.UDisks2.Error.DeviceBusy", []interface{}{"the filesystem is mounted"})
	}
	fstype := blk.IDType
	s.mu.Unlock()
	if derr != nil {
		return false, derr
	}
	dev := "/dev/" + f.name
	var cmd *exec.Cmd
	switch fstype {
	case "ext2", "ext3", "ext4":
		if repair {
			cmd = exec.Command("e2fsck", "-f", "-y", dev)
		} else {
			cmd = exec.Command("e2fsck", "-f", "-n", dev)
		}
	case "vfat":
		if repair {
			cmd = exec.Command("fsck.vfat", "-a", dev)
		} else {
			cmd = exec.Command("fsck.vfat", "-n", dev)
		}
	case "exfat":
		if repair {
			cmd = exec.Command("fsck.exfat", "-y", dev)
		} else {
			cmd = exec.Command("fsck.exfat", "-n", dev)
		}
	default:
		return false, dbus.NewError("org.freedesktop.UDisks2.Error.NotSupported", []interface{}{"cannot check " + fstype})
	}
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL=C"}
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code := ee.ExitCode()
		if repair {
			return code <= 1, nil
		}
		return false, nil
	}
	if err != nil {
		return false, failed(err)
	}
	return true, nil
}

func (f *filesystemObject) Check(sender dbus.Sender, options map[string]dbus.Variant) (bool, *dbus.Error) {
	return f.fsck(sender, false)
}

func (f *filesystemObject) Repair(sender dbus.Sender, options map[string]dbus.Variant) (bool, *dbus.Error) {
	ok, err := f.fsck(sender, true)
	f.s.Refresh()
	return ok, err
}

func (f *filesystemObject) TakeOwnership(sender dbus.Sender, options map[string]dbus.Variant) *dbus.Error {
	s := f.s
	uid, _, _, err := s.caller(sender)
	if err != nil {
		return failed(err)
	}
	s.mu.Lock()
	blk := s.block(f.name)
	if blk == nil || blk.Filesystem == nil {
		s.mu.Unlock()
		return failed(errors.New("not a filesystem"))
	}
	_, derr := s.mayModify(sender, blk)
	fstype := blk.IDType
	points := append([]string(nil), blk.Filesystem.MountPoints...)
	s.mu.Unlock()
	if derr != nil {
		return derr
	}
	if len(points) > 0 {
		if err := os.Chown(points[0], int(uid), int(uid)); err != nil {
			return failed(err)
		}
		return nil
	}
	if err := takeOwnership("/dev/"+f.name, fstype, uid); err != nil {
		return failed(err)
	}
	return nil
}

type swapObject struct {
	s    *Service
	name string
}

func (w *swapObject) Start(sender dbus.Sender, options map[string]dbus.Variant) *dbus.Error {
	return w.toggle(sender, true)
}

func (w *swapObject) Stop(sender dbus.Sender, options map[string]dbus.Variant) *dbus.Error {
	return w.toggle(sender, false)
}

func (w *swapObject) toggle(sender dbus.Sender, on bool) *dbus.Error {
	w.s.mu.Lock()
	blk := w.s.block(w.name)
	if blk == nil || blk.Swapspace == nil {
		w.s.mu.Unlock()
		return failed(errors.New("not a swap device"))
	}
	_, derr := w.s.mayModify(sender, blk)
	w.s.mu.Unlock()
	if derr != nil {
		return derr
	}
	dev := "/dev/" + w.name
	var err error
	if on {
		err = run("swapon", dev)
	} else {
		err = run("swapoff", dev)
	}
	if err != nil {
		return failed(err)
	}
	w.s.Refresh()
	return nil
}

func (b *blockObject) open(sender dbus.Sender, flags int) (dbus.UnixFD, *dbus.Error) {
	s := b.s
	s.mu.Lock()
	blk := s.block(b.name)
	if blk == nil {
		s.mu.Unlock()
		return 0, failed(errors.New("no such device"))
	}
	_, derr := s.mayModify(sender, blk)
	if derr == nil && flags != os.O_RDONLY {
		derr = s.busy(blk)
	}
	s.mu.Unlock()
	if derr != nil {
		return 0, derr
	}
	fd, err := syscall.Open("/dev/"+b.name, flags|syscall.O_CLOEXEC, 0)
	if err != nil {
		return 0, failed(err)
	}
	time.AfterFunc(30*time.Second, func() { syscall.Close(fd) })
	return dbus.UnixFD(fd), nil
}

func (b *blockObject) OpenForBackup(sender dbus.Sender, options map[string]dbus.Variant) (dbus.UnixFD, *dbus.Error) {
	return b.open(sender, os.O_RDONLY)
}

func (b *blockObject) OpenForRestore(sender dbus.Sender, options map[string]dbus.Variant) (dbus.UnixFD, *dbus.Error) {
	return b.open(sender, os.O_WRONLY|syscall.O_EXCL)
}

func (b *blockObject) OpenForBenchmark(sender dbus.Sender, options map[string]dbus.Variant) (dbus.UnixFD, *dbus.Error) {
	return b.open(sender, os.O_RDONLY|syscall.O_DIRECT)
}

func (f *filesystemObject) Resize(sender dbus.Sender, size uint64, options map[string]dbus.Variant) *dbus.Error {
	s := f.s
	s.mu.Lock()
	blk := s.block(f.name)
	if blk == nil || blk.Filesystem == nil {
		s.mu.Unlock()
		return failed(errors.New("not a filesystem"))
	}
	_, derr := s.mayModify(sender, blk)
	fstype := blk.IDType
	s.mu.Unlock()
	if derr != nil {
		return derr
	}
	dev := "/dev/" + f.name
	switch fstype {
	case "ext2", "ext3", "ext4":
		args := []string{dev}
		if size > 0 {
			args = append(args, strconv.FormatUint(size/1024, 10)+"K")
		}
		if err := run("resize2fs", args...); err != nil {
			return failed(err)
		}
	default:
		return dbus.NewError("org.freedesktop.UDisks2.Error.NotSupported", []interface{}{"cannot resize " + fstype})
	}
	s.settle(f.name)
	return nil
}
