package record

import (
	"encoding/binary"
	"errors"
	"io"
	"os"
	"time"
)

// Index file (.idx) is a small append-only sidecar for each segment:
//
//	header: 8 bytes magic "G2RIDX01"
//	entry:  16 bytes = type (1 byte) + reserved (3 bytes) + time ms (uint32) + offset (uint64)
//
// Keyframe entries point to the moof atom of each keyframe. The end entry is written
// when the segment is closed properly and holds the total duration and file size.
// The start entry (before the end entry) holds the correction of the segment start time
// (file name) in milliseconds in the offset field (signed).
// If the process crashes, the end entry will be missing and the last keyframe
// is used as an approximate end.

const (
	indexMagic     = "G2RIDX01"
	indexEntrySize = 16

	EntryKeyframe byte = 1
	EntryEnd      byte = 2
	EntryStart    byte = 3
)

type IndexEntry struct {
	Type   byte
	Time   time.Duration // offset from segment start
	Offset int64         // byte offset in the mp4 file
}

func appendIndexEntry(b []byte, e IndexEntry) []byte {
	var buf [indexEntrySize]byte
	buf[0] = e.Type
	binary.BigEndian.PutUint32(buf[4:], uint32(e.Time/time.Millisecond))
	binary.BigEndian.PutUint64(buf[8:], uint64(e.Offset))
	return append(b, buf[:]...)
}

// ReadIndex reads all entries from an index file. A truncated last entry is ignored.
func ReadIndex(path string) ([]IndexEntry, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return DecodeIndex(b)
}

func DecodeIndex(b []byte) ([]IndexEntry, error) {
	if len(b) < len(indexMagic) || string(b[:len(indexMagic)]) != indexMagic {
		return nil, errors.New("record: wrong index header")
	}
	b = b[len(indexMagic):]

	entries := make([]IndexEntry, 0, len(b)/indexEntrySize)
	for len(b) >= indexEntrySize {
		entries = append(entries, IndexEntry{
			Type:   b[0],
			Time:   time.Duration(binary.BigEndian.Uint32(b[4:])) * time.Millisecond,
			Offset: int64(binary.BigEndian.Uint64(b[8:])),
		})
		b = b[indexEntrySize:]
	}
	return entries, nil
}

// IndexStart returns the start correction of the segment (zero if none)
func IndexStart(entries []IndexEntry) time.Duration {
	for _, e := range entries {
		if e.Type == EntryStart {
			return time.Duration(e.Offset) * time.Millisecond
		}
	}
	return 0
}

// IndexDuration returns segment duration from index entries
// and true if the segment was closed properly.
func IndexDuration(entries []IndexEntry) (time.Duration, bool) {
	if n := len(entries); n > 0 {
		last := entries[n-1]
		return last.Time, last.Type == EntryEnd
	}
	return 0, false
}

type indexWriter struct {
	f   *os.File
	buf []byte
}

func createIndex(path string) (*indexWriter, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	if _, err = io.WriteString(f, indexMagic); err != nil {
		_ = f.Close()
		return nil, err
	}
	return &indexWriter{f: f}, nil
}

func (w *indexWriter) Write(e IndexEntry) error {
	w.buf = appendIndexEntry(w.buf[:0], e)
	_, err := w.f.Write(w.buf)
	return err
}

func (w *indexWriter) Close() error {
	err := w.f.Sync()
	if err2 := w.f.Close(); err == nil {
		err = err2
	}
	return err
}
