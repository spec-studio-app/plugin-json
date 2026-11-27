package files

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// tempSuffix ends the name of each temporary file. A start removes the ones
// that a crash left.
const tempSuffix = ".tmp"

// AfterTempWrite is a test hook. It runs after the temporary file is written
// and before the rename. When it returns an error, the write stops there and
// leaves the temporary file, as a crash does.
var AfterTempWrite func(temp string) error

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// tempName is <path>.<pid>.<random>.tmp, in the folder of path.
func tempName(path string) string {
	return fmt.Sprintf("%s.%d.%s%s", path, os.Getpid(), randomHex(4), tempSuffix)
}

// WriteAtomic writes data to path: a temporary file in the same folder with a
// flush, a rename over path, and a flush of the folder. A reader sees the old
// file or the new file, never a part of one.
func WriteAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	temp := tempName(path)
	f, err := os.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(temp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(temp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(temp)
		return err
	}
	return finish(temp, path)
}

// WriteAtomicFrom copies r to path, with the same steps as WriteAtomic.
func WriteAtomicFrom(path string, r io.Reader) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	temp := tempName(path)
	f, err := os.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		os.Remove(temp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(temp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(temp)
		return err
	}
	return finish(temp, path)
}

func finish(temp, path string) error {
	if AfterTempWrite != nil {
		if err := AfterTempWrite(temp); err != nil {
			return err
		}
	}
	if err := os.Rename(temp, path); err != nil {
		os.Remove(temp)
		return err
	}
	return SyncDir(filepath.Dir(path))
}

// SyncDir flushes a folder, so that a rename or a remove in it is durable.
func SyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	// Some file systems do not flush a folder; the rename is still atomic.
	_ = d.Sync()
	return nil
}

// SweepTemps removes the temporary files under root that no live process
// writes: the ones of this process (a crash of an earlier store in the same
// process, in tests) and the ones of a process that no longer runs.
func SweepTemps(root string) error {
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), tempSuffix) {
			return nil
		}
		if pid, ok := tempPID(d.Name()); ok && pid != os.Getpid() && alive(pid) {
			return nil
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	})
	return err
}

// tempPID reads the pid from <name>.<pid>.<random>.tmp.
func tempPID(name string) (int, bool) {
	parts := strings.Split(strings.TrimSuffix(name, tempSuffix), ".")
	if len(parts) < 3 {
		return 0, false
	}
	pid, err := strconv.Atoi(parts[len(parts)-2])
	return pid, err == nil && pid > 0
}

func alive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
