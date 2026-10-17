package service

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
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
	if err := cryptsetup([]string{"first secret"}, "luksFormat", "--batch-mode", "--type", "luks2", "--pbkdf", "pbkdf2", "--pbkdf-force-iterations", "1000", "--key-file=/dev/fd/3", img); err != nil {
		t.Skipf("luksFormat unavailable here: %v", err)
	}
	if err := cryptsetup([]string{"first secret", "second secret"}, "luksChangeKey", "--pbkdf", "pbkdf2", "--pbkdf-force-iterations", "1000", "--key-file=/dev/fd/3", img, "/dev/fd/4"); err != nil {
		t.Fatal(err)
	}
	if err := cryptsetup([]string{"second secret"}, "open", "--test-passphrase", "--key-file=/dev/fd/3", img); err != nil {
		t.Errorf("the new passphrase does not open the volume: %v", err)
	}
	if err := cryptsetup([]string{"first secret"}, "open", "--test-passphrase", "--key-file=/dev/fd/3", img); err != errWrongPassphrase {
		t.Errorf("the old passphrase still opens the volume: %v", err)
	}
}
