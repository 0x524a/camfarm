#!/usr/bin/env bash
# Cuts a release: checks that the commit is fit to ship, then creates and pushes
# an annotated vX.Y.Z tag. Pushing the tag starts .github/workflows/release.yml,
# which builds the archives and the container image and creates a DRAFT GitHub
# release; nothing is public until you publish that draft.
#
# Usage: scripts/release.sh [--dry-run] vX.Y.Z[-prerelease]
#   --dry-run  run every check and stop before creating the tag
# Needs: git, gh (authenticated), goreleaser v2.
set -euo pipefail

dry_run=0
if [ "${1:-}" = "--dry-run" ]; then
	dry_run=1
	shift
fi
version=${1:-}
semver='^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?$'
if [[ ! $version =~ $semver ]]; then
	echo "usage: scripts/release.sh [--dry-run] vX.Y.Z[-prerelease]" >&2
	exit 2
fi

die() { echo "release: $*" >&2; exit 1; }

cd "$(dirname "$0")/.."
for tool in git gh goreleaser; do
	command -v "$tool" >/dev/null || die "$tool is not installed"
done

[ "$(git rev-parse --abbrev-ref HEAD)" = main ] || die "not on main"
[ -z "$(git status --porcelain)" ] || die "working tree is not clean"

git fetch --quiet --tags origin
[ "$(git rev-parse HEAD)" = "$(git rev-parse origin/main)" ] || die "local main differs from origin/main (pull or push first)"
git rev-parse --quiet --verify "refs/tags/$version" >/dev/null && die "tag $version already exists"

sha=$(git rev-parse HEAD)
conclusion=$(gh api "repos/{owner}/{repo}/actions/workflows/ci.yml/runs?head_sha=$sha&event=push&per_page=1" --jq '.workflow_runs[0].conclusion')
[ "$conclusion" = success ] || die "ci on ${sha:0:12} is '${conclusion:-missing}', not 'success'"

goreleaser check

if [ "$dry_run" = 1 ]; then
	echo "release: dry run ok; would tag ${sha:0:12} as $version"
	exit 0
fi

echo "release: tagging $(git log -1 --format='%h %s' "$sha") as $version"
read -r -p "push $version to origin? [y/N] " answer
[ "$answer" = y ] || die "aborted"

git tag -a "$version" -m "CamFarm $version" "$sha"
git push origin "refs/tags/$version"
echo "release: pushed $version; watch the 'release' workflow, then publish the draft release"
