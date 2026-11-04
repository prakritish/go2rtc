package creds

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestString(t *testing.T) {
	AddSecret("admin")
	AddSecret("pa$$word")

	s := SecretString("rtsp://admin:pa$$word@192.168.1.123/stream1")
	require.Equal(t, "rtsp://***@192.168.1.123/stream1", s)

	s = SecretString("rtsp://192.168.1.123/stream1?user=admin&pass=pa$$word")
	require.Equal(t, "rtsp://192.168.1.123/stream1?user=***&pass=***", s)
}

// moved from pkg/core (StripUserinfo became SecretString)
func TestStringUserinfo(t *testing.T) {
	s := `streams:
  test:
    - ffmpeg:rtsp://username:password@10.1.2.3:554/stream1
    - ffmpeg:rtsp://10.1.2.3:554/stream1@#video=copy
`
	require.Equal(t, `streams:
  test:
    - ffmpeg:rtsp://***@10.1.2.3:554/stream1
    - ffmpeg:rtsp://10.1.2.3:554/stream1@#video=copy
`, SecretString(s))
}
