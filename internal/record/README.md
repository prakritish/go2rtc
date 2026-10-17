# Record

Continuous or event-based recording of streams to disk in fragmented MP4 (fMP4) segments, playback of any
time range as a single MP4 file or HLS, and real-time playback through any go2rtc output (WebRTC, RTSP, MSE...).
Without transcoding.

- Each stream is recorded by a regular consumer, like any other client (WebRTC, MSE, RTSP...)
- Recording keeps the stream (producer) running all the time, like [preload](../streams/README.md)
- A new segment starts on the first keyframe after `segment` duration
- fMP4 is crash-safe: a segment is playable up to the last written keyframe even after a power loss
- A small `.idx` sidecar file stores the time and byte offset of each keyframe for fast seeking
- The filesystem is the only storage, the catalog is rebuilt on startup (no database)

## Configuration

```yaml
record:
  path: /var/lib/go2rtc/recordings  # default: recordings (relative to working dir)
  segment: 60s                      # default: 60s
  retention: 7d                     # delete segments older than this (supports d, h, m, s)
  max_size: 500GB                   # delete oldest segments when total size is bigger (K, M, G, T - powers of 1024)
  pre: 10s                          # events mode: keep the recording before the event (default 10s)
  post: 10s                         # events mode: keep the recording after the event (default 10s)
  stall_timeout: 30s                # re-attach to the stream if no data (default 30s)
  max_clip: 24h                     # max duration of video.mp4 and playlist.m3u8 (default 24h, 0 - unlimited)
  max_event: 1h                     # event in progress is ended automatically (default 1h, 0 - never)
  hls_segment: 4s                   # min HLS segment duration (default 4s)
  ffmpeg_jobs: 2                    # max simultaneous FFmpeg jobs for format=mp4 and exact (default 2)
  streams:
    camera1: video&audio            # all supported codecs (default)
    camera2: video                  # video only
    camera3: video=h264&audio=aac   # specific codecs
    camera4: video&audio&mode=events  # keep only the recording around events
```

### Events mode

With `mode=events` the camera is always connected, but the disk is written only during events:

- Without an event, segments are kept **in memory**: only the last keyframe intervals that cover `pre` time
- When an event starts, the memory pre-buffer is written to disk (it starts from the keyframe at or before
  `now - pre`) and the recording continues to disk
- When the event ends (`end + post`), the segment is finished on the next keyframe, the next segments are in memory
- Memory usage is about `2 x pre` of the stream bitrate per camera (ex. 4 Mbit/s and 10s = 10 MB)

Events are created by the [events API](#events), for example from a motion detector or an alarm.
A back-dated event (`start` in the past) gets only the data that is in the pre-buffer.

## File layout

```
{path}/{stream}/{YYYY-MM-DD}/{YYYY-MM-DD}T{hh-mm-ss.mmm}Z.mp4   - segment (UTC start time)
{path}/{stream}/{YYYY-MM-DD}/{YYYY-MM-DD}T{hh-mm-ss.mmm}Z.idx   - keyframe index
```

The stream name is URL-escaped to be a safe directory name.

## API

| Method | Path                                           | Description                                      |
|--------|------------------------------------------------|--------------------------------------------------|
| GET    | `/api/record`                                  | Status of recorders and storage                  |
| PUT    | `/api/record?src=camera1&video&audio`          | Start recording (saved to the config)            |
| DELETE | `/api/record?src=camera1`                      | Stop recording (removed from the config)         |
| GET    | `/api/record/video.mp4?src=camera1&start=&duration=` | **One MP4 for the time range** (start + duration) |
| GET    | `/api/record/video.mp4?src=camera1&start=&end=`      | **One MP4 for the time range** (start .. end)     |
| GET    | `/api/record/playlist.m3u8?src=camera1&start=&duration=` | HLS VOD playlist for the time range (Safari, hls.js) |
| GET    | `/api/record/timeline?src=camera1&start=&end=` | Recorded ranges, gaps and events                 |
| GET    | `/api/record/events?src=camera1&start=&end=`   | List events                                      |
| POST   | `/api/record/events?src=camera1&label=motion`  | Create event (see [events](#events))             |
| PUT    | `/api/record/events?src=camera1&id=...`        | End event in progress                            |
| DELETE | `/api/record/events?src=camera1&id=...`        | Delete event                                     |
| GET    | `/api/record/segments?src=camera1&start=&end=` | List segments overlapping the time range         |
| GET    | `/api/record/segment.mp4?src=camera1&id=...`   | Download one segment                             |

- `start`, `end` - RFC3339 time (`2026-10-17T14:00:00Z`, `2026-10-17T19:30:00+05:30`), unix seconds (`1792245600.5`),
  `now` or seconds relative to now (`-60`)
- `event` - event ID instead of `start` and `end`: the event with `pre` and `post` time (`video.mp4`, `playlist.m3u8`, `record:`)
- `duration` - seconds (`300`, `12.5`) or duration (`5m`, `1h30m`)
- `filename` - optional, download as an attachment with this name (`video.mp4` and `segment.mp4`)

### Playback: video.mp4

```
GET /api/record/video.mp4?src=camera1&start=2026-10-17T14:00:00Z&duration=300
GET /api/record/video.mp4?src=camera1&start=2026-10-17T14:00:00Z&end=2026-10-17T14:05:00Z
```

Returns one fragmented MP4 file made from the recorded segments on the fly:

- No transcoding: the response is built from file ranges with patched fragment headers (sequence numbers
  and decode times), it uses almost no CPU and memory
- The size is known in advance: `Content-Length` and HTTP `Range` requests are supported (seeking in players,
  resumable downloads)
- Starts from the **keyframe at or before** `start` (the real start is in the `X-Record-Start` header)
- Ends exactly before the first frame at or after `end`
- Gaps (camera offline) are skipped, the video continues with the next recorded frame
- Works for the segment being recorded right now (up to the last written keyframe interval)
- Max duration is `max_clip` (24 hours by default)

Optional params (require FFmpeg, still without transcoding):

- `format=mp4` - regular MP4 with `moov` at the start (`+faststart`), for tools that don't like fragmented MP4
- `exact` - start exactly from the requested time. FFmpeg keeps the frames from the keyframe and adds
  an MP4 edit list, so players hide the frames before `start`. Implies `format=mp4`.

With these params the file is prepared in `{path}/.tmp` before sending (max `ffmpeg_jobs` at a time). The result is
cached for 5 minutes, so HTTP Range requests and repeated downloads don't run FFmpeg again, and simultaneous
identical requests wait for one FFmpeg job.
FFmpeg binary is taken from the `ffmpeg: bin:` config option (default `ffmpeg`).

Response headers:

| Header               | Description                                                            |
|----------------------|------------------------------------------------------------------------|
| `X-Record-Start`     | Real start time of the video (keyframe)                                |
| `X-Record-End`       | Real end time of the video                                             |
| `X-Record-Duration`  | Media duration in seconds (without gaps)                               |
| `X-Record-Gaps`      | Number of gaps inside the video                                        |
| `X-Record-Truncated` | The video ends earlier than requested because codec parameters changed |

Errors: `400` - wrong params, `404` - no recording for the time range.

### Playback: playlist.m3u8

```
GET /api/record/playlist.m3u8?src=camera1&start=2026-10-17T14:00:00Z&duration=300
```

HLS VOD playlist (version 7, fMP4) for Safari and for seeking in [hls.js](https://github.com/video-dev/hls.js):

- Media segments are byte ranges (`EXT-X-BYTERANGE`) of the `video.mp4` clip for the same time range,
  so there is no extra storage or processing. The clip is cached for the following segment requests.
- Segments start on keyframes and are at least `hls_segment` (4 seconds) long
- `EXT-X-PROGRAM-DATE-TIME` for every segment gives the real time, also after gaps
- If the range includes the segment being recorded and `end` is in the future, the playlist is **live**
  (`EXT-X-PLAYLIST-TYPE:EVENT` without `EXT-X-ENDLIST`): the player reloads it and gets new segments,
  the listed segments never change (the last unfinished segment is listed after the next keyframe)

### Timeline

```json
{
  "ranges": [
    {"start": "2026-10-17T14:00:00.123Z", "end": "2026-10-17T15:30:00.456Z", "duration": 5400.333}
  ],
  "gaps": []
}
```

Segments closer than 1 second are merged into one range.

### Segments

```json
[
  {
    "id": "2026-10-17/2026-10-17T14-03-00.123Z.mp4",
    "start": "2026-10-17T14:03:00.123Z",
    "end": "2026-10-17T14:04:00.123Z",
    "duration": 60,
    "size": 31457280,
    "codecs": "avc1.640029,mp4a.40.2"
  }
]
```

The segment being recorded right now has `"recording": true`.

### Events

```
POST   /api/record/events?src=camera1&label=motion                       - event in progress, starts now
POST   /api/record/events?src=camera1&label=motion&duration=30           - event of 30 seconds from now
POST   /api/record/events?src=camera1&start=-15&duration=30              - started 15 seconds ago
PUT    /api/record/events?src=camera1&id=K3vZ8pQ2aB                      - end event in progress now
PUT    /api/record/events?src=camera1&id=K3vZ8pQ2aB&end=2026-10-17T14:05:00Z
GET    /api/record/events?src=camera1&start=-3600                        - events of the last hour
DELETE /api/record/events?src=camera1&id=K3vZ8pQ2aB
```

```json
{"id": "K3vZ8pQ2aB", "start": "2026-10-17T14:00:00Z", "end": "2026-10-17T14:00:30Z", "label": "motion"}
```

- Events are stored in `{path}/{stream}/events.json`
- An event in progress is ended automatically after `max_event` (1 hour)
- Ended events are removed when there is no recording for them anymore (retention)
- Watch the event: `video.mp4?src=camera1&event=K3vZ8pQ2aB` or `playlist.m3u8?src=camera1&event=K3vZ8pQ2aB`

## Playback source: record:

The recording can be played in real time as a regular go2rtc source, so it works with every go2rtc output:
WebRTC, MSE, RTSP, `stream.mp4`, HLS, etc.

```yaml
streams:
  camera1_replay: record:camera1?start=2026-10-17T14:00:00Z&duration=300   # time range
  camera1_delay: record:camera1?start=-60                                    # 60 seconds behind live
```

```
http://192.168.1.123:1984/stream.html?src=record:camera1%3Fstart%3D-60
http://192.168.1.123:1984/api/stream.mp4?src=record:camera1%3Fevent%3DK3vZ8pQ2aB
rtsp://192.168.1.123:8554/camera1_delay
```

- `start` is required, `end`, `duration` or `event` limit the playback
- **Without** `end` or `duration` the playback follows the recording (it never ends), segments recorded after
  the start of the playback are played too
- Gaps are skipped
- After the end of the range the source stays connected without data (go2rtc doesn't restart it)
- Relative `start` (`-60`) is calculated when the source is started (the first consumer connects)
- Supported codecs: H264, H265, AAC, Opus and PCM audio. PCMA/PCMU/PCM are stored as FLAC and played as
  PCMA, PCMU or L16 (8000 Hz) or L16 (other rates), the conversion is lossless. MP3 is not played

## Timestamps and A/V sync

- Each sample's decode time is its camera timestamp relative to the segment start, so frame timing is exact
- The first segment after a (re)connect starts at the arrival time of its first keyframe, it's corrected on close
  to the best estimate (the least delayed frame), because the first frames are usually late (startup latency).
  The correction is saved to the index (`.idx`), the file name doesn't change
- Next segments continue the previous one exactly (start = previous start + duration), no overlaps or gaps.
  If the own estimate of a segment differs more than 500 ms (clock drift), it's corrected
- When segments roll over, audio keeps its sync with video from the previous segment. Audio usually arrives
  later than video (encoder buffers), so the next segment may start its video with a small lead (up to 2 s)
  to keep the late audio samples in place. The index times are relative to the first keyframe.
- After a (re)connect, audio is aligned with video by arrival time (RTP timestamps of different tracks are unrelated)

## Reliability

- If the camera goes offline, the current segment is closed and a new one starts after reconnect
- A timestamp jump (camera reconnect) of more than 5 seconds closes the segment
- If no data is received for `stall_timeout` (30 seconds), the recorder re-attaches to the stream
- If the camera changes resolution or encoder settings (new SPS/PPS/VPS in the stream or in the SDP after
  reconnect), a new segment starts with the new init (moov)
- After a crash (power loss, kill), the index of unfinished segments is rebuilt from the MP4 on startup and the
  incomplete data at the end of the file is truncated
- If the stream can't be started, the recorder retries every 5 seconds to 1 minute
- On a disk error the segment is closed and recording retries after 5 seconds
- On exit (SIGINT/SIGTERM) all segments are finished properly

## Limitations

- `video.mp4` starts from a keyframe, not from the exact frame (use `exact` for that).
- One MP4 file can't change codec parameters: if they differ between segments in the range (camera changed
  resolution), `video.mp4` and `playlist.m3u8` end at the change (`X-Record-Truncated`). The `record:` source
  plays through the change.
- `format=mp4` and `exact` prepare the whole file before sending, so the response starts after FFmpeg finishes.
- After a (re)connect, audio is aligned with video by arrival time (RTCP sender reports are not used).
- If a clip starts in a segment with an audio lead or late audio samples, its video starts with a small offset
  (`start_time` > 0, usually below 0.6 s) and audio starts at 0.
- Stereo PCM is recorded as FLAC with a double sample rate (like `stream.mp4`), the `record:` source plays it as mono.
