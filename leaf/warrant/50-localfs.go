package warrant

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"agentbob/contract"
)

// localFileChannel is a contract.FileChannel rooted at one local directory. Every
// op resolves its path UNDER the root and refuses any escape — the sandbox
// confinement (no reading /etc/passwd). Stateless: Close is a no-op.
type localFileChannel struct {
	root     string
	realRoot string // root with symlinks resolved — the confinement boundary
}

func newLocalFileChannel(space, root string) (*localFileChannel, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("warrant: prepare space %q: %w", space, scrubRoot(err, root))
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("warrant: prepare space %q: %w", space, scrubRoot(err, root))
	}
	return &localFileChannel{
		root:     root,
		realRoot: realRoot,
	}, nil
}

func (c *localFileChannel) Alive() bool  { return true }
func (c *localFileChannel) Close() error { return nil }

// resolve cleans a space-relative path and joins it under the root, refusing any
// result that escapes the root (.. traversal / absolute paths are neutralized by
// rooting the clean at "/").
func (c *localFileChannel) resolve(rel string) (string, error) {
	clean := filepath.Clean("/" + rel) // force-root: ".." can't climb above "/"
	abs := filepath.Join(c.root, clean)
	if abs != c.root && !strings.HasPrefix(abs, c.root+string(os.PathSeparator)) {
		return "", fmt.Errorf("path %q escapes the space", rel)
	}
	// Symlink confinement: the lexical check stops ".." / absolute paths, but a
	// symlink INSIDE the space can point outside (the exec channel shares this dir,
	// so the model can create one). Resolve symlinks on the deepest EXISTING
	// ancestor and require its real path to stay under the real root — so
	// `ln -s /etc/passwd x` then cat/deliver_file can't exfiltrate host files.
	for probe := abs; ; {
		real, err := filepath.EvalSymlinks(probe)
		if err == nil {
			if real != c.realRoot && !strings.HasPrefix(real, c.realRoot+string(os.PathSeparator)) {
				return "", fmt.Errorf("path %q escapes the space via a symlink", rel)
			}
			break
		}
		parent := filepath.Dir(probe)
		if parent == probe { // climbed to the fs root without resolving — bail
			break
		}
		probe = parent
	}
	return abs, nil
}

func (c *localFileChannel) List(_ context.Context, path string) ([]contract.FileEntry, error) {
	abs, err := c.resolve(path)
	if err != nil {
		return nil, err
	}
	ents, err := os.ReadDir(abs)
	if err != nil {
		return nil, scrubRoot(err, c.root)
	}
	out := make([]contract.FileEntry, 0, len(ents))
	for _, e := range ents {
		fe := contract.FileEntry{Name: e.Name(), IsDir: e.IsDir()}
		if info, ierr := e.Info(); ierr == nil {
			fe.Size = info.Size()
		}
		out = append(out, fe)
	}
	return out, nil
}

func (c *localFileChannel) Read(_ context.Context, path string) ([]byte, error) {
	abs, err := c.resolve(path)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(abs)
	return b, scrubRoot(err, c.root)
}

func (c *localFileChannel) Write(_ context.Context, path string, data []byte) error {
	abs, err := c.resolve(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return scrubRoot(err, c.root)
	}
	return scrubRoot(os.WriteFile(abs, data, 0o644), c.root)
}

func (c *localFileChannel) Mkdir(_ context.Context, path string) error {
	abs, err := c.resolve(path)
	if err != nil {
		return err
	}
	return scrubRoot(os.MkdirAll(abs, 0o755), c.root)
}

func (c *localFileChannel) Remove(_ context.Context, path string) error {
	abs, err := c.resolve(path)
	if err != nil {
		return err
	}
	if abs == c.root {
		return fmt.Errorf("refusing to remove the space root")
	}
	return scrubRoot(os.RemoveAll(abs), c.root)
}

func (c *localFileChannel) Rename(_ context.Context, oldPath, newPath string) error {
	oldAbs, err := c.resolve(oldPath)
	if err != nil {
		return err
	}
	newAbs, err := c.resolve(newPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(newAbs), 0o755); err != nil {
		return scrubRoot(err, c.root)
	}
	return scrubRoot(os.Rename(oldAbs, newAbs), c.root)
}

// AbsPath resolves a space-relative path to a real local filesystem path for
// out-of-band delivery (deliver_file). It reuses resolve — the same escape
// check every read/write op runs — so a path climbing out of the space root is
// refused, then stats the result so a non-existent file errors here rather than
// downstream at send time. Returns the validated absolute path.
func (c *localFileChannel) AbsPath(path string) (string, error) {
	abs, err := c.resolve(path)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(abs); err != nil {
		return "", scrubRoot(err, c.root)
	}
	return abs, nil
}

// scrubRoot rewrites a filesystem error so the model never sees the $BOB_HOME-rooted
// absolute path — only the space-relative remainder. os.ReadFile/ReadDir/Rename/etc.
// embed the absolute path in the *PathError/*LinkError they return, and that error
// flows straight back to the model through the fs/terminal tools; the model works in
// space-relative terms and must not learn bob's on-disk layout (info hygiene). The
// inner error is mutated in place, so a wrapped error is scrubbed too.
func scrubRoot(err error, root string) error {
	if err == nil {
		return nil
	}
	rel := func(p string) string {
		p = strings.TrimPrefix(p, root)
		p = strings.TrimPrefix(p, string(os.PathSeparator))
		if p == "" {
			return "."
		}
		return p
	}
	var pe *os.PathError
	if errors.As(err, &pe) {
		pe.Path = rel(pe.Path)
	}
	var le *os.LinkError
	if errors.As(err, &le) {
		le.Old, le.New = rel(le.Old), rel(le.New)
	}
	return err
}

var _ contract.FileChannel = (*localFileChannel)(nil)
