package record

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRepairSegment(t *testing.T) {
	segments := recordAV(t)
	seg := segments[1]

	expect, err := ReadIndex(IndexPath(seg.Path))
	require.NoError(t, err)

	// crash: no index and incomplete fragment at the end
	require.NoError(t, os.Remove(IndexPath(seg.Path)))
	f, err := os.OpenFile(seg.Path, os.O_WRONLY|os.O_APPEND, 0)
	require.NoError(t, err)
	_, err = f.Write([]byte{0, 0, 1, 0, 'm', 'o', 'o', 'f', 1, 2, 3}) // partial moof
	require.NoError(t, err)
	require.NoError(t, f.Close())

	duration, err := RepairSegment(seg.Path)
	require.NoError(t, err)
	require.Equal(t, 2*time.Second, duration)

	fi, err := os.Stat(seg.Path)
	require.NoError(t, err)
	require.Equal(t, seg.Size, fi.Size(), "incomplete data should be truncated")

	entries, err := ReadIndex(IndexPath(seg.Path))
	require.NoError(t, err)
	require.Equal(t, expect, entries, "rebuilt index should be the same")

	// the repaired segment works in a clip
	clip, err := NewClip([]Segment{seg}, seg.Start, seg.End())
	require.NoError(t, err)
	checkTimeline(t, parseClip(t, readAll(t, clip), clip.init))

	// not an MP4 file: error, file is not changed
	bad := t.TempDir() + "/bad.mp4"
	require.NoError(t, os.WriteFile(bad, []byte("not a video file at all"), 0644))
	_, err = RepairSegment(bad)
	require.Error(t, err)
	b, _ := os.ReadFile(bad)
	require.Equal(t, "not a video file at all", string(b))
}
