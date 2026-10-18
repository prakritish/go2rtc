#!/usr/bin/env bash
# Release go2rtc (with the record module):
#   1. binaries for linux and macOS, amd64 and arm64 -> dist/<version>/
#   2. macOS binaries signed with the Developer ID Application certificate on the MDM server
#      (the private key never leaves the server: the binaries go there through S3 and SSM)
#   3. multi-arch Docker image (linux/amd64 + linux/arm64) pushed to Docker Hub
#   4. git tag v<version> pushed to origin
#
# usage: scripts/release.sh <version>        ex. scripts/release.sh 1.9.14-rec.1
#
# Requirements: go, docker with buildx and arm64 emulation (apt install qemu-user-binfmt),
# docker login (Docker Hub), aws CLI with SSM and S3 access to the MDM account, a clean git tree.
#
# Optional environment:
#   IMAGE=prakritish/go2rtc      Docker image name
#   LATEST=1                     also tag the image as :latest (default 1, set 0 for test releases)
#   SKIP_SIGN=1                  don't sign macOS binaries (not for distribution)
#   NO_PUSH=1                    build everything, push nothing (no image, no git tag)
#   SIGN_INSTANCE, SIGN_BUCKET, AWS_REGION, BUNDLE_ID - see below
set -euo pipefail

VERSION="${1:-}"
VERSION="${VERSION#v}"

IMAGE="${IMAGE:-prakritish/go2rtc}"
LATEST="${LATEST:-1}"
PLATFORMS="linux/amd64,linux/arm64"
SIGN_INSTANCE="${SIGN_INSTANCE:-i-057a6e6e158217c5d}"     # MDM server (fleet-mdm-ec2)
SIGN_BUCKET="${SIGN_BUCKET:-fleet-mdm-backups-pmcddrb4}"  # the server can read and write this bucket
AWS_REGION="${AWS_REGION:-us-west-2}"
BUNDLE_ID="${BUNDLE_ID:-ai.hakimo.go2rtc}"
TEAM_ID="Q2QDG3YXPS"

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DIST="$ROOT/dist/$VERSION"
TAG="v$VERSION"

CLEANUP_S3=""
cleanup() { [[ -z "$CLEANUP_S3" ]] || aws s3 rm --quiet --recursive "$CLEANUP_S3" >/dev/null 2>&1 || true; }
trap cleanup EXIT

log() { printf '\n\033[1m==> %s\033[0m\n' "$*"; }
die() { printf '\033[31mERROR: %s\033[0m\n' "$*" >&2; exit 1; }

# ---------- checks ----------

[[ -n "$VERSION" ]] || die "usage: $0 <version>   (ex. 1.9.14-rec.1)"
# semver: Go puts the git tag into the binary only if it's a valid semantic version
[[ "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] || die "version must be semver: 1.2.3 or 1.2.3-rec.1"

cd "$ROOT"

for cmd in go git docker; do command -v $cmd >/dev/null || die "$cmd is not installed"; done
docker buildx version >/dev/null 2>&1 || die "docker buildx is not installed"
[[ "${SKIP_SIGN:-0}" == 1 ]] || command -v aws >/dev/null || die "aws CLI is not installed (or set SKIP_SIGN=1)"

[[ -z "$(git status --porcelain)" ]] || die "git tree is not clean, commit or stash the changes"

if [[ -e /proc/sys/fs/binfmt_misc/qemu-aarch64 ]] || [[ "$(uname -m)" =~ ^(aarch64|arm64)$ ]]; then :; else
  die "no arm64 emulation for the Docker image: sudo apt install qemu-user-binfmt"
fi

if [[ "${NO_PUSH:-0}" != 1 ]]; then
  grep -q 'index.docker.io' "${DOCKER_CONFIG:-$HOME/.docker}/config.json" 2>/dev/null || die "not logged in to Docker Hub: docker login"
fi

# git tag: Go stamps it into the binary (no "+dev" suffix in the version)
if git rev-parse -q --verify "refs/tags/$TAG" >/dev/null; then
  [[ "$(git rev-parse "$TAG^{commit}")" == "$(git rev-parse HEAD)" ]] || die "tag $TAG exists and points to another commit"
else
  log "Tag $TAG"
  git tag -a "$TAG" -m "go2rtc $VERSION"
fi

# ---------- 1. binaries ----------

log "Build binaries -> dist/$VERSION"
rm -rf "$DIST"
mkdir -p "$DIST"

build() { # os arch name
  echo "  $3"
  CGO_ENABLED=0 GOOS=$1 GOARCH=$2 go build -ldflags "-s -w -X main.version=$VERSION" -trimpath -o "$DIST/$3" .
}
build linux  amd64 go2rtc_linux_amd64
build linux  arm64 go2rtc_linux_arm64
build darwin amd64 go2rtc_mac_amd64
build darwin arm64 go2rtc_mac_arm64

# the version must be clean (tagged commit, clean tree)
if [[ "$(uname -s)/$(uname -m)" == "Linux/x86_64" ]]; then
  got="$("$DIST/go2rtc_linux_amd64" -v)"
  echo "  $got"
  [[ "$got" == "go2rtc version $VERSION "* ]] || die "unexpected version: $got"
fi

# ---------- 2. sign macOS binaries ----------

sign_macos() {
  local s3="s3://$SIGN_BUCKET/go2rtc-release/$VERSION-$(date +%s)"
  CLEANUP_S3="$s3" # removed on exit, also on errors

  log "Sign macOS binaries on $SIGN_INSTANCE (Developer ID Application, team $TEAM_ID)"
  for f in go2rtc_mac_amd64 go2rtc_mac_arm64; do
    aws s3 cp --quiet "$DIST/$f" "$s3/in/$f"
  done

  # runs as root on the server; the key is copied only to a private folder in memory for the signing
  local script
  script=$(cat <<EOF
set -eu
D=/opt/fleet-mdm/nanomdm/certs/signing
RC=/opt/fleet-mdm/bin/rcodesign
INTER=/opt/fleet-mdm/controlplane/app/apple_developer_id_g2_ca.pem
openssl x509 -in \$D/app.pem -noout -checkend 0 >/dev/null || { echo "certificate expired"; exit 1; }
BASE=/dev/shm; [ -w \$BASE ] || BASE=/tmp
T=\$(mktemp -d \$BASE/g2rsign.XXXXXX); trap 'rm -rf "\$T"' EXIT; chmod 700 \$T
umask 077
cat \$D/app.key \$D/app.pem \$INTER > \$T/id.pem
for f in go2rtc_mac_amd64 go2rtc_mac_arm64; do
  aws s3 cp --quiet $s3/in/\$f \$T/\$f
  \$RC sign --pem-file \$T/id.pem --code-signature-flags runtime --binary-identifier $BUNDLE_ID \$T/\$f \$T/\$f.signed >\$T/log 2>&1 || { cat \$T/log; exit 1; }
  \$RC verify \$T/\$f.signed >\$T/log 2>&1 || { cat \$T/log; exit 1; }
  aws s3 cp --quiet \$T/\$f.signed $s3/out/\$f
  echo "signed \$f"
done
EOF
)
  local b64 cid status
  b64=$(printf '%s' "$script" | base64 -w0)
  cid=$(aws ssm send-command --region "$AWS_REGION" --instance-ids "$SIGN_INSTANCE" \
    --document-name AWS-RunShellScript --comment "go2rtc $VERSION macOS signing" \
    --parameters "commands=[\"echo $b64 | base64 -d | bash\"]" \
    --query Command.CommandId --output text)

  for _ in $(seq 1 60); do
    sleep 3
    status=$(aws ssm get-command-invocation --region "$AWS_REGION" --command-id "$cid" \
      --instance-id "$SIGN_INSTANCE" --query Status --output text 2>/dev/null || echo Pending)
    case "$status" in Pending|InProgress|Delayed) continue ;; *) break ;; esac
  done
  aws ssm get-command-invocation --region "$AWS_REGION" --command-id "$cid" --instance-id "$SIGN_INSTANCE" \
    --query '[StandardOutputContent,StandardErrorContent]' --output text | sed '/^\s*$/d; s/^/  /'
  [[ "$status" == Success ]] || die "signing failed on the server (status: $status)"

  for f in go2rtc_mac_amd64 go2rtc_mac_arm64; do
    aws s3 cp --quiet "$s3/out/$f" "$DIST/$f.signed"
    # the signature embeds the certificate with the team ID
    grep -aq "$TEAM_ID" "$DIST/$f.signed" || die "$f: no Developer ID signature in the result"
    mv "$DIST/$f.signed" "$DIST/$f"
    chmod 755 "$DIST/$f"
  done
}

if [[ "${SKIP_SIGN:-0}" == 1 ]]; then
  log "SKIP_SIGN=1: macOS binaries are NOT signed"
else
  sign_macos
fi

log "Checksums"
(cd "$DIST" && sha256sum go2rtc_* > SHA256SUMS && sed 's/^/  /' SHA256SUMS)

# ---------- 3. Docker image ----------

tags=(-t "$IMAGE:$VERSION")
[[ "$LATEST" == 1 ]] && tags+=(-t "$IMAGE:latest")

if [[ "${NO_PUSH:-0}" == 1 ]]; then
  log "NO_PUSH=1: build the Docker image for $PLATFORMS without pushing"
  docker buildx build --platform "$PLATFORMS" --build-arg VERSION="$VERSION" \
    -f docker/record/Dockerfile "${tags[@]}" .
  log "Done (nothing pushed): dist/$VERSION"
  exit 0
fi

log "Build and push $IMAGE:$VERSION for $PLATFORMS"
docker buildx build --platform "$PLATFORMS" --build-arg VERSION="$VERSION" \
  -f docker/record/Dockerfile "${tags[@]}" --push .

docker buildx imagetools inspect "$IMAGE:$VERSION" | grep -E '^(Name|Digest)|Platform' | sed 's/^/  /'

# ---------- 4. git tag ----------

log "Push tag $TAG"
git push origin "$TAG"

log "Done: $IMAGE:$VERSION, binaries in dist/$VERSION"
