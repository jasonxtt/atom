#!/usr/bin/env bash
# Assembles the atom package from parts that are already built, and the install.sh that goes with it.
# Nothing is published. The atomdns program is built on the Linux build host (it is not built here).
#
#   installer/scripts/build-package.sh --version 0.1.0 --atomdns FILE --sing-box FILE --webui FILE \
#       [--atomdns-version STR] [--config DIR] [--repo owner/name] [--out DIR]
#
# Output (default installer/dist): atom-<version>-linux-amd64.tar.gz, install.sh (this version and the
# package's sha256 written into it), SHA256SUMS.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
INSTALLER="$(cd "$HERE/.." && pwd)"
ATOM_ROOT="$(cd "$INSTALLER/.." && pwd)"
VERSION="" ATOMDNS="" SINGBOX="" WEBUI="" ATOMDNS_VERSION="unknown"
CONFIG="$ATOM_ROOT/atomdns/config" REPO="jasonxtt/atom" OUT="$INSTALLER/dist"

while [ $# -gt 0 ]; do
  case "$1" in
    --version) VERSION="$2"; shift 2 ;;
    --atomdns) ATOMDNS="$2"; shift 2 ;;
    --sing-box) SINGBOX="$2"; shift 2 ;;
    --webui) WEBUI="$2"; shift 2 ;;
    --atomdns-version) ATOMDNS_VERSION="$2"; shift 2 ;;
    --config) CONFIG="$2"; shift 2 ;;
    --repo) REPO="$2"; shift 2 ;;
    --out) OUT="$2"; shift 2 ;;
    *) echo "unknown argument $1" >&2; exit 2 ;;
  esac
done
[[ "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo "--version X.Y.Z is required" >&2; exit 2; }
for f in "$ATOMDNS" "$SINGBOX" "$WEBUI"; do [ -f "$f" ] || { echo "missing input file: '$f'" >&2; exit 2; }; done
[ -f "$CONFIG/config_custom.yaml" ] || { echo "no configuration at $CONFIG" >&2; exit 2; }

sha256() { if command -v sha256sum >/dev/null; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi; }
commit() { git -C "$1" rev-parse --short HEAD 2>/dev/null || echo unknown; }

STAGE="$(mktemp -d)"
trap 'rm -rf "$STAGE"' EXIT
NAME="atom-$VERSION"
PKG="$STAGE/$NAME"
mkdir -p "$PKG/bin" "$OUT"

(cd "$INSTALLER" && go vet ./... && go test ./... >/dev/null)
(cd "$INSTALLER" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "$PKG/bin/atom-installer" .)
cp "$ATOMDNS" "$PKG/bin/atomdns"
cp "$SINGBOX" "$PKG/bin/sing-box"
cp "$WEBUI" "$PKG/atom-webui.tar.gz"
chmod 0755 "$PKG"/bin/*
mkdir -p "$PKG/config"
(cd "$CONFIG" && tar -cf - --exclude README.md --exclude .DS_Store .) | (cd "$PKG/config" && tar -xf -)

cat > "$PKG/manifest.json" <<JSON
{
  "version": "$VERSION",
  "built_at": "$(date -u +%Y-%m-%dT%H:%M:%SZ)",
  "atomdns": {"version": "$ATOMDNS_VERSION", "commit": "$(commit "$ATOM_ROOT/atomdns")", "sha256": "$(sha256 "$PKG/bin/atomdns")"},
  "sing-box": {"commit": "$(commit "$ATOM_ROOT/sing-box")", "sha256": "$(sha256 "$PKG/bin/sing-box")"},
  "webui": {"sha256": "$(sha256 "$PKG/atom-webui.tar.gz")"},
  "installer": {"commit": "$(commit "$ATOM_ROOT")", "sha256": "$(sha256 "$PKG/bin/atom-installer")"}
}
JSON

ARCHIVE="$OUT/$NAME-linux-amd64.tar.gz"
rm -f "$ARCHIVE"
if tar --version 2>/dev/null | grep -q GNU; then
  tar -C "$STAGE" --owner=0 --group=0 --numeric-owner -czf "$ARCHIVE" "$NAME"
else
  COPYFILE_DISABLE=1 tar -C "$STAGE" --uid 0 --gid 0 --uname root --gname root -czf "$ARCHIVE" "$NAME"
fi
SUM="$(sha256 "$ARCHIVE")"
sed -e "s|@VERSION@|$VERSION|g" -e "s|@SHA256@|$SUM|g" -e "s|@REPO@|$REPO|g" "$HERE/install.sh.in" > "$OUT/install.sh"
chmod 0755 "$OUT/install.sh"
bash -n "$OUT/install.sh"
(cd "$OUT" && printf '%s  %s\n%s  %s\n' "$SUM" "$(basename "$ARCHIVE")" "$(sha256 install.sh)" install.sh > SHA256SUMS)
echo "built $ARCHIVE ($(du -h "$ARCHIVE" | cut -f1)), $OUT/install.sh"
