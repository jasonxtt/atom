#!/usr/bin/env bash
# Assembles the four atom packages from parts that are already built (atomdns, WebUI), builds sing-box and
# atom-installer for both architectures, and writes the install.sh that goes with them. Nothing is published.
# atomdns is built on the Linux build host (release-build.yml); its artifacts are read from --atomdns-dir.
#
#   installer/scripts/build-package.sh --version 0.1.0 --atomdns-dir DIR \
#       [--webui FILE] [--atomdns-version STR] [--config DIR] [--sing-box-dir DIR] [--sing-box-version STR] \
#       [--repo owner/name] [--out DIR]
#
# DIR holds atomdns-linux-<variant>.tar.gz (each with an `atomdns` program in it) and atom-webui.tar.gz.
# Package <- atomdns variant:  amd64 <- amd64,  arm64 <- arm64-gnu,  amd64-musl <- amd64-musl,  arm64-musl <- arm64
# sing-box and atom-installer are static (CGO_ENABLED=0), one build per architecture, shared by glibc and musl.
#
# Output (default installer/dist): atom-<version>-linux-{amd64,arm64,amd64-musl,arm64-musl}.tar.gz, install.sh
# (this version and the four sha256 written into it), SHA256SUMS.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
INSTALLER="$(cd "$HERE/.." && pwd)"
ATOM_ROOT="$(cd "$INSTALLER/.." && pwd)"
VERSION="" ATOMDNS_DIR="" WEBUI="" ATOMDNS_VERSION="unknown" SINGBOX_VERSION=""
CONFIG="$ATOM_ROOT/atomdns/config" SINGBOX_DIR="$ATOM_ROOT/sing-box" REPO="jasonxtt/atom" OUT="$INSTALLER/dist"

while [ $# -gt 0 ]; do
  case "$1" in
    --version) VERSION="$2"; shift 2 ;;
    --atomdns-dir) ATOMDNS_DIR="$2"; shift 2 ;;
    --webui) WEBUI="$2"; shift 2 ;;
    --atomdns-version) ATOMDNS_VERSION="$2"; shift 2 ;;
    --config) CONFIG="$2"; shift 2 ;;
    --sing-box-dir) SINGBOX_DIR="$2"; shift 2 ;;
    --sing-box-version) SINGBOX_VERSION="$2"; shift 2 ;;
    --repo) REPO="$2"; shift 2 ;;
    --out) OUT="$2"; shift 2 ;;
    *) echo "unknown argument $1" >&2; exit 2 ;;
  esac
done
[[ "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo "--version X.Y.Z is required" >&2; exit 2; }
[ -d "$ATOMDNS_DIR" ] || { echo "--atomdns-dir DIR is required" >&2; exit 2; }
[ -n "$WEBUI" ] || WEBUI="$ATOMDNS_DIR/atom-webui.tar.gz"
[ -f "$WEBUI" ] || { echo "missing WebUI package: '$WEBUI'" >&2; exit 2; }
[ -f "$SINGBOX_DIR/release/LDFLAGS" ] || { echo "no sing-box source at $SINGBOX_DIR (git submodule update --init)" >&2; exit 2; }
[ -n "$SINGBOX_VERSION" ] || SINGBOX_VERSION="$(git -C "$SINGBOX_DIR" describe --tags 2>/dev/null | sed 's/^v//' || true)"
[ -n "$SINGBOX_VERSION" ] || SINGBOX_VERSION=unknown
[ -f "$CONFIG/config_custom.yaml" ] || { echo "no configuration at $CONFIG" >&2; exit 2; }

# package name -> GOARCH, atomdns variant
PACKAGES="amd64:amd64:amd64 arm64:arm64:arm64-gnu amd64-musl:amd64:amd64-musl arm64-musl:arm64:arm64"
for p in $PACKAGES; do
  v="${p##*:}"
  [ -f "$ATOMDNS_DIR/atomdns-linux-$v.tar.gz" ] || { echo "missing $ATOMDNS_DIR/atomdns-linux-$v.tar.gz" >&2; exit 2; }
done

sha256() { if command -v sha256sum >/dev/null; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi; }
commit() { git -C "$1" rev-parse --short HEAD 2>/dev/null || echo unknown; }

STAGE="$(mktemp -d)"
trap 'rm -rf "$STAGE"' EXIT
mkdir -p "$OUT"

# Check every atomdns before anything is built: right architecture (ELF e_machine: x86_64 0x3e, aarch64 0xb7),
# glibc packages need nothing newer than Ubuntu 22.04's glibc 2.35, musl packages must not reference glibc at all.
for p in $PACKAGES; do
  IFS=: read -r pkgname arch variant <<<"$p"
  mkdir -p "$STAGE/x-$variant"
  tar -xzf "$ATOMDNS_DIR/atomdns-linux-$variant.tar.gz" -C "$STAGE/x-$variant"
  bin="$STAGE/x-$variant/atomdns"
  [ -f "$bin" ] || { echo "atomdns-linux-$variant.tar.gz has no atomdns in it" >&2; exit 2; }
  want=3e; [ "$arch" = arm64 ] && want=b7
  got="$(od -An -tx1 -j18 -N2 "$bin" | tr -d ' \n')"
  [ "$got" = "${want}00" ] || { echo "atomdns-linux-$variant.tar.gz: ELF machine $got, expected ${want}00 for $arch" >&2; exit 2; }
  syms="$(strings -a "$bin" | grep -oE 'GLIBC_[0-9]+\.[0-9]+' | sed 's/GLIBC_//' | sort -uV || true)"
  case "$pkgname" in
    *-musl) [ -z "$syms" ] || { echo "atomdns-linux-$variant.tar.gz is for musl but references glibc" >&2; exit 2; } ;;
    *) need="$(printf '%s\n' "$syms" | tail -1)"
       if [ -n "$need" ] && [ "$(printf '%s\n2.35\n' "$need" | sort -V | tail -1)" != "2.35" ]; then
         echo "atomdns-linux-$variant.tar.gz needs glibc $need, newer than Ubuntu 22.04's 2.35; use the release-build.yml artifact" >&2; exit 2
       fi ;;
  esac
done

(cd "$INSTALLER" && go vet ./... && go test ./... >/dev/null)
for arch in amd64 arm64; do
  mkdir -p "$STAGE/build/$arch"
  (cd "$INSTALLER" && CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath -ldflags "-s -w -X main.version=$VERSION" \
    -o "$STAGE/build/$arch/atom-installer" .)
  (cd "$SINGBOX_DIR" && CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath \
    -tags "$(cat release/DEFAULT_BUILD_TAGS_OTHERS)" \
    -ldflags "-s -w -X github.com/sagernet/sing-box/constant.Version=$SINGBOX_VERSION $(cat release/LDFLAGS)" \
    -o "$STAGE/build/$arch/sing-box" ./cmd/sing-box)
done

if tar --version 2>/dev/null | grep -q GNU; then
  mktar() { tar -C "$1" --owner=0 --group=0 --numeric-owner -czf "$2" "$3"; }
else
  mktar() { COPYFILE_DISABLE=1 tar -C "$1" --no-xattrs --no-mac-metadata --uid 0 --gid 0 --uname root --gname root -czf "$2" "$3"; }
fi

SHAS=""
for p in $PACKAGES; do
  IFS=: read -r pkgname arch variant <<<"$p"
  libc=glibc; case "$pkgname" in *-musl) libc=musl ;; esac
  NAME="atom-$VERSION"
  PKG="$STAGE/$pkgname/$NAME"
  mkdir -p "$PKG/bin" "$PKG/config"
  cp "$STAGE/x-$variant/atomdns" "$PKG/bin/atomdns"
  cp "$STAGE/build/$arch/sing-box" "$STAGE/build/$arch/atom-installer" "$PKG/bin/"
  cp "$WEBUI" "$PKG/atom-webui.tar.gz"
  chmod 0755 "$PKG"/bin/*
  (cd "$CONFIG" && tar -cf - --exclude README.md --exclude .DS_Store .) | (cd "$PKG/config" && tar -xf -)
  cat > "$PKG/manifest.json" <<JSON
{
  "version": "$VERSION",
  "arch": "$arch",
  "libc": "$libc",
  "built_at": "$(date -u +%Y-%m-%dT%H:%M:%SZ)",
  "atomdns": {"version": "$ATOMDNS_VERSION", "commit": "$(commit "$ATOM_ROOT/atomdns")", "sha256": "$(sha256 "$PKG/bin/atomdns")"},
  "sing-box": {"commit": "$(commit "$SINGBOX_DIR")", "sha256": "$(sha256 "$PKG/bin/sing-box")"},
  "webui": {"sha256": "$(sha256 "$PKG/atom-webui.tar.gz")"},
  "installer": {"commit": "$(commit "$ATOM_ROOT")", "sha256": "$(sha256 "$PKG/bin/atom-installer")"}
}
JSON
  ARCHIVE="$OUT/$NAME-linux-$pkgname.tar.gz"
  rm -f "$ARCHIVE"
  mktar "$STAGE/$pkgname" "$ARCHIVE" "$NAME"
  SHAS="$SHAS $pkgname=$(sha256 "$ARCHIVE")"
  echo "built $ARCHIVE ($(du -h "$ARCHIVE" | cut -f1))"
done

sha_of() { for e in $SHAS; do [ "${e%%=*}" = "$1" ] && echo "${e#*=}"; done; }
sed -e "s|@VERSION@|$VERSION|g" -e "s|@REPO@|$REPO|g" \
    -e "s|@SHA256_AMD64@|$(sha_of amd64)|g" -e "s|@SHA256_ARM64@|$(sha_of arm64)|g" \
    -e "s|@SHA256_AMD64_MUSL@|$(sha_of amd64-musl)|g" -e "s|@SHA256_ARM64_MUSL@|$(sha_of arm64-musl)|g" \
    "$HERE/install.sh.in" > "$OUT/install.sh"
chmod 0755 "$OUT/install.sh"
bash -n "$OUT/install.sh"
(cd "$OUT" && : > SHA256SUMS
 for p in $PACKAGES; do f="atom-$VERSION-linux-${p%%:*}.tar.gz"; printf '%s  %s\n' "$(sha256 "$f")" "$f" >> SHA256SUMS; done
 printf '%s  %s\n' "$(sha256 install.sh)" install.sh >> SHA256SUMS)
echo "wrote $OUT/install.sh, $OUT/SHA256SUMS"
