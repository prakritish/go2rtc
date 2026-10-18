# go2rtc with recording: production image

A small image for running go2rtc as an NVR with the [record](../../internal/record/README.md) module.

| File            | Description                                                         |
|-----------------|---------------------------------------------------------------------|
| `Dockerfile`    | Alpine + go2rtc + FFmpeg, non-root (uid 1000), health check         |
| `compose.yaml`  | Docker Compose service with volumes, secrets from `.env`, limits    |
| `go2rtc.yaml`   | Sample production config: auth, continuous and events recording     |
| `.env.example`  | Secrets template                                                    |

FFmpeg is included: `video.mp4?format=mp4` and `&exact` use it (remux, no transcoding), and so do `ffmpeg:`
sources. The image is ~60 MB compressed. Compared to [docker/Dockerfile](../Dockerfile) it has no Python, ffplay,
fonts or Intel VAAPI drivers: add `libva-intel-driver intel-media-driver` if you transcode with hardware.

Prebuilt image for linux/amd64 and linux/arm64: `prakritish/go2rtc` (Docker Hub). Releases (binaries for Linux and
macOS, signed macOS binaries, the image) are built with [scripts/release.sh](../../scripts/release.sh).

## Quick start

```bash
cd docker/record
cp .env.example .env                          # fill in the passwords
mkdir -p config && cp go2rtc.yaml config/     # edit the cameras
sudo mkdir -p /srv/recordings
sudo chown -R 1000:1000 config /srv/recordings
docker compose up -d --build
docker compose logs -f
```

WebUI and API: `http://HOST:1984` (user and password from `.env`).

Without Compose:

```bash
docker build -f docker/record/Dockerfile -t go2rtc-record .   # from the repository root
docker run -d --name go2rtc --network host --restart unless-stopped \
  -v ./config:/config -v /srv/recordings:/recordings \
  -e GO2RTC_API_PASS=... -e GO2RTC_RTSP_PASS=... \
  go2rtc-record
```

## Production notes

- **Storage**: put `/recordings` on a dedicated disk. Set `max_size` to ~90% of it, so the disk never
  fills up. Disk usage is about `bitrate x time`: 4 Mbit/s = ~43 GB per camera per day.
- **Shutdown**: `docker stop` sends SIGTERM, go2rtc closes the open segments (`stop_grace_period: 30s`).
  After a crash or power loss the segments are repaired on the next start.
- **Secrets**: `${NAME}` in `go2rtc.yaml` is replaced with the environment variable. An unset variable without
  a default stays as is (`${NAME}`), Compose fails early if a required secret is missing.
- **Config is writable**: `PUT /api/record` and the WebUI editor save to `/config/go2rtc.yaml`.
- **Security**: the WebUI can edit the config and run `exec:` sources, keep port 1984 private (VPN or reverse
  proxy with TLS). RTSP (8554) has its own user and password.
- **Health check**: `GET http://127.0.0.1:1984/api` from inside the container (loopback skips auth). If you change
  `api.listen` or enable `api.local_auth`, override the health check.
- **Network**: `network_mode: host` is the simplest for WebRTC. With a bridge network map 8555/tcp and 8555/udp and
  set `GO2RTC_PUBLIC_IP` to the address browsers use.
- **A/V sync**: use RTSP from the cameras when possible, its RTCP sender reports give exact audio/video sync.
- **Time**: segment names and the API use UTC; `TZ` only changes the log time.
