package store

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/spec-studio-app/plugin-json/internal/files"
	"github.com/spec-studio-app/plugin-sdk/go/protocol"
)

// blobPath is blobs/<first two hex characters>/<sha256>. The caller checked
// the hash, so it is one safe path segment.
func (s *Store) blobPath(sha string) string {
	return filepath.Join(s.dir, "blobs", sha[:2], sha)
}

func checkSHA(sha string) error {
	if !protocol.ValidSHA256(sha) {
		return protocol.NewError(protocol.Refused, "The hash is not a SHA-256 in lowercase hex.")
	}
	return nil
}

// HasBlobs gives the hashes that the store has.
func (s *Store) HasBlobs(ctx context.Context, sha256s []string) ([]string, error) {
	present := []string{}
	for _, sha := range sha256s {
		if checkSHA(sha) != nil {
			continue
		}
		if _, err := os.Stat(s.blobPath(sha)); err == nil {
			present = append(present, sha)
		}
	}
	return present, nil
}

// PutBlob stores a blob whose size and hash the SDK checked. The bytes go to
// a temporary file and a rename makes the blob, so a blob is never half there.
func (s *Store) PutBlob(ctx context.Context, sha string, size int64, r io.Reader) error {
	if err := checkSHA(sha); err != nil {
		return err
	}
	if err := files.WriteAtomicFrom(s.blobPath(sha), r); err != nil {
		return unavailable("The plugin cannot write the blob %s. Check the free space and the access of the data folder.", sha)
	}
	return nil
}

// GetBlob gives length bytes at offset (fewer at the end) and the full size.
func (s *Store) GetBlob(ctx context.Context, sha string, offset int64, length int) ([]byte, int64, error) {
	if err := checkSHA(sha); err != nil {
		return nil, 0, err
	}
	f, err := os.Open(s.blobPath(sha))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, 0, protocol.NewError(protocol.NotFound, "The blob %s does not exist.", sha)
	}
	if err != nil {
		return nil, 0, unavailable("The plugin cannot read the blob %s.", sha)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, 0, unavailable("The plugin cannot read the blob %s.", sha)
	}
	size := info.Size()
	if offset >= size || length <= 0 {
		return []byte{}, size, nil
	}
	if rest := size - offset; int64(length) > rest {
		length = int(rest)
	}
	buf := make([]byte, length)
	n, err := f.ReadAt(buf, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, 0, unavailable("The plugin cannot read the blob %s.", sha)
	}
	return buf[:n], size, nil
}

// DeleteBlob is false when the store did not have the blob.
func (s *Store) DeleteBlob(ctx context.Context, sha string) (bool, error) {
	if err := checkSHA(sha); err != nil {
		return false, err
	}
	err := os.Remove(s.blobPath(sha))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, unavailable("The plugin cannot remove the blob %s.", sha)
	}
	return true, nil
}
