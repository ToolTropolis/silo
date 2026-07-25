package cache

import (
	"encoding/binary"
	"fmt"
	"time"
)

// Cache entries carry a fixed-size binary header so eviction can reason about
// age without a second bucket to keep in sync.
//
// Binary rather than a JSON envelope because Get is on the read path: decoding
// is a reslice plus two integer reads, with no allocation beyond the copy that
// already happens.
const (
	entryMagic    uint32 = 0x53494c4f // "SILO"
	entryVersion  uint32 = 1
	entryHeaderSz        = 16 // magic(4) + version(4) + writtenAt(8)
)

// formatVersionKey records the entry layout a file was written with, so a
// future change is a deliberate one-time migration rather than a guess about
// what the bytes mean.
var formatVersionKey = []byte("format-version")

// encodeEntry prefixes content with its header.
func encodeEntry(content []byte, writtenAt time.Time) []byte {
	buf := make([]byte, entryHeaderSz+len(content))
	binary.BigEndian.PutUint32(buf[0:4], entryMagic)
	binary.BigEndian.PutUint32(buf[4:8], entryVersion)
	binary.BigEndian.PutUint64(buf[8:16], uint64(writtenAt.UnixNano()))
	copy(buf[entryHeaderSz:], content)
	return buf
}

// decodeEntry splits a stored value into its write time and content.
//
// A value that doesn't carry the header is a corrupt or truncated record rather
// than an older format: the generation stamp wipes every pre-existing content
// bucket, so by the time this runs every entry was written by this code.
func decodeEntry(raw []byte) (content []byte, writtenAt time.Time, err error) {
	if len(raw) < entryHeaderSz {
		return nil, time.Time{}, fmt.Errorf("%w: %d bytes is shorter than the header", ErrCorruptEntry, len(raw))
	}
	if got := binary.BigEndian.Uint32(raw[0:4]); got != entryMagic {
		return nil, time.Time{}, fmt.Errorf("%w: bad magic %#x", ErrCorruptEntry, got)
	}
	if got := binary.BigEndian.Uint32(raw[4:8]); got != entryVersion {
		return nil, time.Time{}, fmt.Errorf("%w: unknown entry version %d", ErrCorruptEntry, got)
	}
	writtenAt = time.Unix(0, int64(binary.BigEndian.Uint64(raw[8:16])))
	return raw[entryHeaderSz:], writtenAt, nil
}
