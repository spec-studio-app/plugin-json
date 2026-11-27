package files

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// maxFileName is the longest encoded name that the store uses as it is. A
// longer one becomes a hash, so that each file name stays under the 255-byte
// limit of macOS and Linux.
const maxFileName = 200

// FileName gives the file name of a protocol name (a collection or a key).
//
// The SDK checked the name first: it has only A-Z a-z 0-9 . _ : @ - and
// starts with a letter or a digit, so it is never "." or "..". The file name
// keeps a-z 0-9 . _ - and writes each other byte as %XX. So two names that
// differ only in case are two files on a disk that ignores case, and ":" never
// gets to the Finder. The file always holds its key, so a list never reads a
// key from a file name.
func FileName(name string) string {
	var b strings.Builder
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02x", c)
		}
	}
	if b.Len() > maxFileName {
		sum := sha256.Sum256([]byte(name))
		return "h-" + hex.EncodeToString(sum[:])
	}
	return b.String()
}
