package store

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"path/filepath"
	"testing"
	"time"

	"github.com/spec-studio-app/plugin-sdk/go/plugin"
	"github.com/spec-studio-app/plugin-sdk/go/protocol"
)

// R3.S1
func TestChunkedUpload(t *testing.T) {
	c, _ := serve(t, t.TempDir())
	data := make([]byte, 3<<20)
	rand.Read(data)
	sum := sha256.Sum256(data)
	sha := hex.EncodeToString(sum[:])
	var begin protocol.BlobsBeginResult
	if err := c.Call(ctx(t), protocol.MethodBlobsBegin, protocol.BlobsBeginParams{SHA256: sha, Size: int64(len(data))}, &begin); err != nil || begin.Exists {
		t.Fatalf("begin: %+v %v", begin, err)
	}
	for off := 0; off < len(data); off += 1 << 20 {
		var w protocol.BlobsWriteResult
		chunk := data[off : off+1<<20]
		if err := c.Call(ctx(t), protocol.MethodBlobsWrite, protocol.BlobsWriteParams{UploadID: begin.UploadID, Offset: int64(off), Data: chunk}, &w); err != nil {
			t.Fatal(err)
		}
	}
	var commit protocol.BlobsCommitResult
	if err := c.Call(ctx(t), protocol.MethodBlobsCommit, protocol.BlobsCommitParams{UploadID: begin.UploadID}, &commit); err != nil {
		t.Fatal(err)
	}
	has, err := c.HasBlobs(ctx(t), sha)
	if err != nil || len(has) != 1 {
		t.Fatalf("has %v %v", has, err)
	}
	got, err := c.GetBlob(ctx(t), sha)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("get: %d bytes, %v", len(got), err)
	}
	if deleted, err := c.DeleteBlob(ctx(t), sha); err != nil || !deleted {
		t.Fatalf("delete %v %v", deleted, err)
	}
	if deleted, _ := c.DeleteBlob(ctx(t), sha); deleted {
		t.Fatal("a second delete gave true")
	}
	if _, err := c.GetBlob(ctx(t), sha); !protocol.Is(err, protocol.NotFound) {
		t.Fatalf("get after delete: %v, want not_found", err)
	}
}

// blobFiles lists the files under blobs/.
func blobFiles(t *testing.T, dir string) []string {
	var out []string
	filepath.WalkDir(filepath.Join(dir, "blobs"), func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			out = append(out, p)
		}
		return nil
	})
	return out
}

// R3.S2
func TestWrongHash(t *testing.T) {
	dir := t.TempDir()
	c, _ := serve(t, dir)
	data := []byte("the real bytes")
	sum := sha256.Sum256([]byte("other bytes"))
	sha := hex.EncodeToString(sum[:])
	err := c.PutBlobAs(ctx(t), sha, data)
	if !protocol.Is(err, protocol.Refused) {
		t.Fatalf("commit: %v, want refused", err)
	}
	if has, _ := c.HasBlobs(ctx(t), sha); len(has) != 0 {
		t.Fatalf("has %v", has)
	}
	if f := blobFiles(t, dir); len(f) != 0 {
		t.Fatalf("blobs/ has %v", f)
	}
}

// R3.S3
func TestOldUpload(t *testing.T) {
	old := plugin.UploadIdle
	plugin.UploadIdle = 50 * time.Millisecond
	t.Cleanup(func() { plugin.UploadIdle = old })
	dir := t.TempDir()
	c, _ := serve(t, dir)
	sum := sha256.Sum256([]byte("abc"))
	var begin protocol.BlobsBeginResult
	if err := c.Call(ctx(t), protocol.MethodBlobsBegin, protocol.BlobsBeginParams{SHA256: hex.EncodeToString(sum[:]), Size: 3}, &begin); err != nil {
		t.Fatal(err)
	}
	var w protocol.BlobsWriteResult
	if err := c.Call(ctx(t), protocol.MethodBlobsWrite, protocol.BlobsWriteParams{UploadID: begin.UploadID, Offset: 0, Data: []byte("a")}, &w); err != nil {
		t.Fatal(err)
	}
	// The SDK sweeps once a minute; a later call sees that the upload is old.
	deadline := time.Now().Add(90 * time.Second)
	for {
		err := c.Call(ctx(t), protocol.MethodBlobsWrite, protocol.BlobsWriteParams{UploadID: begin.UploadID, Offset: 1, Data: []byte("b")}, &w)
		if protocol.Is(err, protocol.NotFound) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the old upload still takes writes: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if f := blobFiles(t, dir); len(f) != 0 {
		t.Fatalf("the data folder has %v", f)
	}
}
