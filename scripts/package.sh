#!/usr/bin/env bash
# Builds every release archive locally with GoReleaser (snapshot mode: nothing is
# published) and checks the result the way a user would: the checksum file
# verifies, and the archive for this machine unpacks and runs.
#
# Usage: scripts/package.sh
# Needs: goreleaser v2, sha256sum (or shasum), tar, unzip.
set -euo pipefail

cd "$(dirname "$0")/.."

command -v goreleaser >/dev/null || { echo "package: goreleaser is not installed (https://goreleaser.com/install/)" >&2; exit 1; }

goreleaser check
goreleaser release --snapshot --clean --skip=publish

cd dist
if command -v sha256sum >/dev/null; then
	sha256sum --check --strict SHA256SUMS
else
	shasum -a 256 --check SHA256SUMS
fi

os=$(go env GOOS)
arch=$(go env GOARCH)
archive=$(ls camfarm_*_"${os}"_"${arch}".tar.gz camfarm_*_"${os}"_"${arch}".zip 2>/dev/null | head -n 1 || true)
if [ -z "$archive" ]; then
	echo "package: no archive for ${os}/${arch}; checksums verified, nothing to run" >&2
	exit 0
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
case "$archive" in
	*.zip) unzip -q "$archive" -d "$tmp" ;;
	*) tar -xzf "$archive" -C "$tmp" ;;
esac
for f in LICENSE README.md; do
	[ -f "$tmp/$f" ] || { echo "package: $archive is missing $f" >&2; exit 1; }
done
bin="$tmp/camfarm"
[ -x "$bin" ] || bin="$tmp/camfarm.exe"
echo "package: $archive -> $("$bin" --version)"
