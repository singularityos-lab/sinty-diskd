package service

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/singularityos-lab/sinty-diskd/internal/model"
)

const ifEncrypted = "org.freedesktop.UDisks2.Encrypted"

// cryptsetup runs cryptsetup with each secret on its own pipe, passed as
// /dev/fd/3, /dev/fd/4 and so on, so no secret reaches a command line or a
// file.
func cryptsetup(secrets []string, args ...string) error {
	cmd := exec.Command("cryptsetup", args...)
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL=C"}
	var writers []*os.File
	for _, secret := range secrets {
		r, w, err := os.Pipe()
		if err != nil {
			return err
		}
		cmd.ExtraFiles = append(cmd.ExtraFiles, r)
		writers = append(writers, w)
		go func(w *os.File, s string) {
			w.WriteString(s)
			w.Close()
		}(w, secret)
	}
	out, err := cmd.CombinedOutput()
	for _, r := range cmd.ExtraFiles {
		r.Close()
	}
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if strings.Contains(msg, "No key available") {
			return errWrongPassphrase
		}
		return errors.New("cryptsetup: " + msg)
	}
	return nil
}

var errWrongPassphrase = errors.New("the passphrase is not correct")

func cryptFailed(err error) *dbus.Error {
	if err == errWrongPassphrase {
		return dbus.NewError("org.freedesktop.UDisks2.Error.Failed", []interface{}{err.Error()})
	}
	return failed(err)
}

func mapperFor(b *model.Block) string {
	if b.IDUUID != "" {
		return "luks-" + b.IDUUID
	}
	return "luks-" + b.Name
}

type encryptedObject struct {
	s    *Service
	name string
}

func (e *encryptedObject) target(sender dbus.Sender) (*model.Block, *dbus.Error) {
	e.s.mu.Lock()
	defer e.s.mu.Unlock()
	b := e.s.block(e.name)
	if b == nil || b.Encrypted == nil {
		return nil, failed(errors.New("not an encrypted device"))
	}
	uid, _, _, err := e.s.caller(sender)
	if err != nil {
		return nil, failed(err)
	}
	if uid != 0 && (b.HintSystem || uid < 1000) {
		return nil, notAuthorized("unlock a system device")
	}
	return b, nil
}

func (e *encryptedObject) Unlock(sender dbus.Sender, passphrase string, options map[string]dbus.Variant) (dbus.ObjectPath, *dbus.Error) {
	b, derr := e.target(sender)
	if derr != nil {
		return "", derr
	}
	if b.Encrypted.CleartextDevice != "/" {
		return "", dbus.NewError("org.freedesktop.UDisks2.Error.AlreadyUnlocked", []interface{}{"already unlocked"})
	}
	name := mapperFor(b)
	args := []string{"open", "--type", "luks", "--key-file=/dev/fd/3", "/dev/" + b.Name, name}
	if optBool(options, "read-only") || b.ReadOnly {
		args = append(args[:1], append([]string{"--readonly"}, args[1:]...)...)
	}
	if err := cryptsetup([]string{passphrase}, args...); err != nil {
		return "", cryptFailed(err)
	}
	run("udevadm", "settle", "--timeout=10")
	for i := 0; i < 20; i++ {
		e.s.Refresh()
		e.s.mu.Lock()
		cur := e.s.block(e.name)
		path := "/"
		if cur != nil && cur.Encrypted != nil {
			path = cur.Encrypted.CleartextDevice
		}
		e.s.mu.Unlock()
		if path != "/" {
			return dbus.ObjectPath(path), nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return "", failed(errors.New("the cleartext device did not appear"))
}

func (e *encryptedObject) Lock(sender dbus.Sender, options map[string]dbus.Variant) *dbus.Error {
	b, derr := e.target(sender)
	if derr != nil {
		return derr
	}
	e.s.mu.Lock()
	var clear *model.Block
	for _, x := range e.s.model.Blocks {
		if model.BlockPath(x.Name) == b.Encrypted.CleartextDevice {
			clear = x
		}
	}
	if clear != nil {
		derr = e.s.busy(clear)
	}
	e.s.mu.Unlock()
	if clear == nil {
		return dbus.NewError("org.freedesktop.UDisks2.Error.NotUnlocked", []interface{}{"not unlocked"})
	}
	if derr != nil {
		return derr
	}
	if err := cryptsetup(nil, "close", clear.MapperName()); err != nil {
		return failed(err)
	}
	run("udevadm", "settle", "--timeout=10")
	e.s.Refresh()
	return nil
}

func (e *encryptedObject) ChangePassphrase(sender dbus.Sender, oldPass, newPass string, options map[string]dbus.Variant) *dbus.Error {
	b, derr := e.target(sender)
	if derr != nil {
		return derr
	}
	if newPass == "" {
		return failed(errors.New("the new passphrase is empty"))
	}
	if err := cryptsetup([]string{oldPass, newPass}, "luksChangeKey", "--key-file=/dev/fd/3", "/dev/"+b.Name, "/dev/fd/4"); err != nil {
		return cryptFailed(err)
	}
	return nil
}

// formatEncrypted makes dev a LUKS2 container with passphrase and returns
// the cleartext device of the opened container.
func formatEncrypted(dev, uuidHint, passphrase string) (string, string, error) {
	if passphrase == "" {
		return "", "", errors.New("the passphrase is empty")
	}
	if err := cryptsetup([]string{passphrase}, "luksFormat", "--batch-mode", "--type", "luks2", "--key-file=/dev/fd/3", dev); err != nil {
		return "", "", err
	}
	name := "luks-format-" + uuidHint
	if err := cryptsetup([]string{passphrase}, "open", "--type", "luks", "--key-file=/dev/fd/3", dev, name); err != nil {
		return "", "", err
	}
	return "/dev/mapper/" + name, name, nil
}
