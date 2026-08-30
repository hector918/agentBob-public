package warrant

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestLocalFS_SymlinkEscapeRejected pins the confinement fix: a symlink created
// INSIDE the space (the exec channel shares the dir, so the model can make one)
// must not let a read/AbsPath reach a file OUTSIDE the space — the deliver_file /
// fs-cat exfiltration hole.
func TestLocalFS_SymlinkEscapeRejected(t *testing.T) {
	root := t.TempDir()
	secretDir := t.TempDir() // a DIFFERENT tree, not under the space
	secret := filepath.Join(secretDir, "secret.txt")
	if err := os.WriteFile(secret, []byte("TOPSECRET"), 0o644); err != nil {
		t.Fatal(err)
	}
	ch, err := newLocalFileChannel("s", root)
	if err != nil {
		t.Fatal(err)
	}

	// A normal in-space file resolves fine.
	if err := os.WriteFile(filepath.Join(root, "ok.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ch.AbsPath("ok.txt"); err != nil {
		t.Fatalf("normal in-space file should resolve: %v", err)
	}

	// A symlink pointing OUT of the space must be rejected by every read path.
	if err := os.Symlink(secret, filepath.Join(root, "link")); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}
	if _, err := ch.AbsPath("link"); err == nil {
		t.Error("AbsPath followed a symlink OUT of the space (exfil hole)")
	}
	if _, err := ch.Read(context.Background(), "link"); err == nil {
		t.Error("Read followed a symlink OUT of the space (exfil hole)")
	}

	// A symlinked DIRECTORY escape (link/secret.txt) must also be rejected.
	if err := os.Symlink(secretDir, filepath.Join(root, "dlink")); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}
	if _, err := ch.Read(context.Background(), "dlink/secret.txt"); err == nil {
		t.Error("Read escaped via a symlinked directory (exfil hole)")
	}
}
