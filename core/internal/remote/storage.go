package remote

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"agentkate/internal/fsperm"
	"golang.org/x/sys/unix"
)

// ensurePrivateDir applies the same on-disk discipline as the session and
// Cowork stores. Remote credentials are no less sensitive than either.
func ensurePrivateDir(dir string) error { return fsperm.MkdirAll(dir) }

// openPrivate opens a regular credential/audit/lock file without following a
// planted symlink. Existing loose files are tightened before use; a symlink or
// non-regular path fails closed through fsperm.HardenFile.
func openPrivate(path string, flags int) (*os.File, error) {
	if err := ensurePrivateDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	fd, err := unix.Open(path, flags|unix.O_NOFOLLOW|unix.O_CLOEXEC, uint32(fsperm.FileMode))
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	if _, err := fsperm.HardenFile(path); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// writePrivateAtomic publishes b through an unpredictable 0600 temporary file
// in the store directory. A predictable *.tmp name is a same-uid symlink and
// clobber target, so it is not used for credentials, certificates, or devices.
func writePrivateAtomic(path string, b []byte) (err error) {
	if err := ensurePrivateDir(filepath.Dir(path)); err != nil {
		return err
	}
	if _, err := fsperm.HardenFile(path); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(fsperm.FileMode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	_, err = fsperm.HardenFile(path)
	return err
}

func readPrivate(path string) ([]byte, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	if _, err := fsperm.HardenFile(path); err != nil {
		_ = f.Close()
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

func copyPrivate(dst string, src io.Reader) error {
	b, err := io.ReadAll(src)
	if err != nil {
		return fmt.Errorf("remote: read private data: %w", err)
	}
	return writePrivateAtomic(dst, b)
}
