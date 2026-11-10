package service

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	core "github.com/singularityos-lab/libsingularity/src/system/core/go/sintysystem"
)

// TestPassphrasesTravelOnPipes formats a LUKS2 image, changes its passphrase
// and checks both, with the secrets passed only on pipes. It needs a
// cryptsetup on PATH and runs without root on an image file.
func TestPassphrasesTravelOnPipes(t *testing.T) {
	if _, err := exec.LookPath("cryptsetup"); err != nil {
		t.Skip("cryptsetup is not installed")
	}
	img := filepath.Join(t.TempDir(), "luks.img")
	if err := os.WriteFile(img, make([]byte, 32<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cryptsetup([]string{"first secret"}, "luksFormat", "--batch-mode", "--type", "luks2", "--pbkdf", "pbkdf2", "--pbkdf-force-iterations", "1000", "--key-file=/proc/self/fd/3", img); err != nil {
		t.Skipf("luksFormat unavailable here: %v", err)
	}
	if err := cryptsetup([]string{"first secret", "second secret"}, "luksChangeKey", "--pbkdf", "pbkdf2", "--pbkdf-force-iterations", "1000", "--key-file=/proc/self/fd/3", img, "/proc/self/fd/4"); err != nil {
		t.Fatal(err)
	}
	if err := cryptsetup([]string{"second secret"}, "open", "--test-passphrase", "--key-file=/proc/self/fd/3", img); err != nil {
		t.Errorf("the new passphrase does not open the volume: %v", err)
	}
	if err := cryptsetup([]string{"first secret"}, "open", "--test-passphrase", "--key-file=/proc/self/fd/3", img); err != errWrongPassphrase {
		t.Errorf("the old passphrase still opens the volume: %v", err)
	}
}

func TestViaCoreRoutesErrors(t *testing.T) {
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	fellBack := false
	fallback := func() error { fellBack = true; return nil }
	cases := []struct {
		name     string
		in, out  error
		fallback bool
	}{
		{"success", nil, nil, false},
		{"wrong passphrase", core.ErrWrongPassphrase, errWrongPassphrase, false},
		{"io error", syscall.EIO, syscall.EIO, false},
	}
	for _, c := range cases {
		fellBack = false
		err := viaCore(func() error { return c.in }, fallback)
		if !errors.Is(err, c.out) && !(err == nil && c.out == nil) {
			t.Errorf("%s: got %v", c.name, err)
		}
		if fellBack != c.fallback {
			t.Errorf("%s: fallback ran %v", c.name, fellBack)
		}
	}
	fellBack = false
	if err := viaCore(func() error { return syscall.EOPNOTSUPP }, fallback); err == nil || fellBack {
		t.Errorf("unsupported layout without cryptsetup: %v, fallback %v", err, fellBack)
	}
	if err := os.WriteFile(filepath.Join(bin, "cryptsetup"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := viaCore(func() error { return syscall.EOPNOTSUPP }, fallback); err != nil || !fellBack {
		t.Errorf("unsupported layout with cryptsetup: %v, fallback %v", err, fellBack)
	}
}

func TestCoreVolumesOpenWithCryptsetup(t *testing.T) {
	if _, err := exec.LookPath("cryptsetup"); err != nil {
		t.Skip("cryptsetup is not installed")
	}
	img := filepath.Join(t.TempDir(), "luks.img")
	if err := os.WriteFile(img, make([]byte, 32<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := core.LuksFormat(img, "first secret", ""); err != nil {
		t.Fatal(err)
	}
	if err := cryptsetup([]string{"first secret"}, "open", "--test-passphrase", "--key-file=/proc/self/fd/3", img); err != nil {
		t.Fatalf("cryptsetup rejects the core volume: %v", err)
	}
	if err := viaCore(func() error { return core.LuksChangePassphrase(img, "wrong", "x") }, nil); err != errWrongPassphrase {
		t.Errorf("a wrong passphrase gave %v", err)
	}
	if err := viaCore(func() error { return core.LuksChangePassphrase(img, "first secret", "second secret") }, nil); err != nil {
		t.Fatal(err)
	}
	if err := cryptsetup([]string{"second secret"}, "open", "--test-passphrase", "--key-file=/proc/self/fd/3", img); err != nil {
		t.Errorf("the new passphrase does not open the volume: %v", err)
	}
	if err := cryptsetup([]string{"first secret"}, "open", "--test-passphrase", "--key-file=/proc/self/fd/3", img); err != errWrongPassphrase {
		t.Errorf("the old passphrase still opens the volume: %v", err)
	}
}

func TestFormatEncryptedOnDevice(t *testing.T) {
	dev := os.Getenv("SINTY_DISKD_LUKS_DEV")
	if dev == "" {
		t.Skip("SINTY_DISKD_LUKS_DEV is not set")
	}
	clear, name, err := formatEncrypted(dev, "test", "secret")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(clear); err != nil {
		t.Errorf("no cleartext node: %v", err)
	}
	if err := core.LuksClose(name); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(clear); err == nil {
		t.Errorf("%s survived the close", clear)
	}
	if err := viaCore(func() error { return core.LuksOpen(dev, "nope", name, true) }, nil); err != errWrongPassphrase {
		t.Errorf("a wrong passphrase gave %v", err)
	}
	if err := core.LuksOpen(dev, "secret", name, true); err != nil {
		t.Fatal(err)
	}
	if err := core.LuksClose(name); err != nil {
		t.Fatal(err)
	}
}
