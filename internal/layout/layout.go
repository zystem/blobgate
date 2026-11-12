// Package layout says how a key becomes a path on a node. A key is a path of segments (`001/acme/s1_x/dist/app.txt`); every segment is escaped so that it is one safe file name on the weakest node:
//   - letters, digits and `- _ . + = @ , ( )` stay; every other byte is `%XX` (upper case hex), a space too (a server may trim it);
//   - a leading or trailing `.` is escaped, so no file is hidden or called `.` or `..`;
//   - `~` is escaped: it marks the generation suffix of a block directory (`<path>~<generation>/<n>`);
//   - a segment whose escape is longer than 200 bytes becomes its first 120 bytes (never cut inside a `%XX`), `~` and 24 hex digits of the SHA-256 of the whole segment.
//
// A key is refused, never rewritten, when it could be a trick: empty, over 1024 bytes, an empty, `.` or `..` segment, a control character, a leading or trailing `/`.
// Not handled: a node that folds the case of names (two keys that differ in case only would meet) and the reserved names of Windows (docs/rclone-probe.md, "Not run").
package layout

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

const (
	// MaxKeyLen is the longest key.
	MaxKeyLen  = 1024
	maxSegment = 200
	keptBytes  = 120
	hashHexLen = 24
	hexDigits  = "0123456789ABCDEF"
)

func safe(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("-_.+=@,()", c) >= 0
}

func escapeAll(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		edge := i == 0 || i == len(s)-1
		if safe(c) && !(edge && c == '.') {
			b.WriteByte(c)
		} else {
			b.WriteByte('%')
			b.WriteByte(hexDigits[c>>4])
			b.WriteByte(hexDigits[c&15])
		}
	}
	return b.String()
}

// EscapeSegment is the file name for one segment of a key.
func EscapeSegment(s string) string {
	r := escapeAll(s)
	if len(r) <= maxSegment {
		return r
	}
	cut := keptBytes
	for cut > 0 && cut < len(r) && (r[cut-1] == '%' || (cut >= 2 && r[cut-2] == '%')) {
		cut--
	}
	h := sha256.Sum256([]byte(s))
	return r[:cut] + "~" + hex.EncodeToString(h[:])[:hashHexLen]
}

// Valid returns "" if this may be a key, else why not.
func Valid(key string) string {
	if key == "" {
		return "the key is empty"
	}
	if len(key) > MaxKeyLen {
		return fmt.Sprintf("the key is over %d bytes", MaxKeyLen)
	}
	for i := 0; i < len(key); i++ {
		if key[i] < ' ' || key[i] == 0x7f {
			return "the key has a control character"
		}
		if key[i] == '\\' {
			return "the key has a backslash"
		}
	}
	if key[0] == '/' || key[len(key)-1] == '/' {
		return "the key starts or ends with /"
	}
	for _, p := range strings.Split(key, "/") {
		switch p {
		case "":
			return "the key has an empty segment"
		case ".", "..":
			return "the key has a . or .. segment"
		}
	}
	return ""
}

// Rel is the path on a node of a valid key (no generation).
func Rel(key string) string {
	parts := strings.Split(key, "/")
	for i, p := range parts {
		parts[i] = EscapeSegment(p)
	}
	return strings.Join(parts, "/")
}

// BlockDir is the directory of the blocks of one generation of a key: `<path>~<generation>`.
func BlockDir(key, generation string) string { return Rel(key) + "~" + generation }

// BlockPath is the path of block n (0-based) of a generation.
func BlockPath(key, generation string, n int) string {
	return fmt.Sprintf("%s/%d", BlockDir(key, generation), n)
}

// BlockKey names a block in the repair queue: the key with the shard, `~`, the generation, `/`, the number. It is read back from the right (the generation has no `~` or `/`, the number no `/`), so a key that
// holds both of those cannot be mistaken for it.
func BlockKey(full, generation string, n int) string {
	return fmt.Sprintf("%s~%s/%d", full, generation, n)
}

// DirKey names the blocks of a whole generation in the repair queue.
func DirKey(full, generation string) string { return full + "~" + generation }

// ParseKey reads what BlockKey or DirKey made. isBlock is false for a directory (a `purge` task).
func ParseKey(k string) (full, generation string, n int, isBlock bool, ok bool) {
	if i := strings.LastIndexByte(k, '/'); i >= 0 {
		if v, err := strconv.Atoi(k[i+1:]); err == nil && v >= 0 {
			if j := strings.LastIndexByte(k[:i], '~'); j > 0 && j < i-1 {
				return k[:j], k[j+1 : i], v, true, true
			}
		}
	}
	if j := strings.LastIndexByte(k, '~'); j > 0 && j < len(k)-1 && !strings.ContainsRune(k[j+1:], '/') {
		return k[:j], k[j+1:], 0, false, true
	}
	return "", "", 0, false, false
}
