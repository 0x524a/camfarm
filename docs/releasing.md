# Releasing

For maintainers. A release is a `vX.Y.Z` tag on `main`. Pushing the tag starts the `release` workflow, which builds the archives and the container image and creates a **draft** GitHub release. Nothing is public until the draft is published by hand.

## What a release contains

- `camfarm_<version>_<os>_<arch>.tar.gz` for linux and darwin on amd64 and arm64, and `.zip` for windows on amd64. Each holds the `camfarm` binary, `LICENSE` and `README.md`. The build is pure Go (`CGO_ENABLED=0`), so there is nothing to install beside the binary.
- `SHA256SUMS` covering every archive.
- A multi-arch (amd64, arm64) image at `ghcr.io/0x524a/camfarm`, tagged `X.Y.Z`, plus `X.Y` and `latest` for a tag with no prerelease suffix.
- A build provenance attestation for each archive, for `SHA256SUMS` and for the image.

`camfarm --version` prints the version, commit and build date stamped into the binary. An unstamped build (`go build` in a checkout) prints `dev`; `go install ...@vX.Y.Z` prints the module version.

## Cutting a release

1. Merge everything for the release to `main` and wait for CI to pass on that commit.
2. Check the checks without tagging: `scripts/release.sh --dry-run v0.1.0`.
3. Tag and push: `scripts/release.sh v0.1.0`. The script refuses unless you are on a clean `main` equal to `origin/main`, the tag is new, CI succeeded on the commit, and `goreleaser check` passes. It asks before pushing.
4. Watch the `release` workflow. Its first job refuses a tag that is not reachable from `main` or whose commit has no green `ci` run, so a hand-pushed tag cannot ship unchecked code.
5. Open the draft release, write the notes, and publish it.
6. Update the version in the README's Status line.

Use a suffix such as `v0.2.0-rc.1` for a prerelease: the release is marked as a prerelease and the image is not tagged `latest`.

## Trying the packaging locally

`scripts/package.sh` runs GoReleaser in snapshot mode (nothing is published), verifies `SHA256SUMS`, then unpacks the archive for your OS and architecture and runs `--version`. It needs [GoReleaser](https://goreleaser.com/install/) v2. CI runs the same script on every pull request, so a broken `.goreleaser.yaml` is found before the tag.

## Verifying a download

```sh
sha256sum --check --ignore-missing SHA256SUMS
gh attestation verify camfarm_0.1.0_linux_amd64.tar.gz --repo 0x524a/camfarm
gh attestation verify oci://ghcr.io/0x524a/camfarm:0.1.0 --repo 0x524a/camfarm
```

## If a release goes wrong

Delete the draft, delete the tag locally and on origin (`git push origin :refs/tags/vX.Y.Z`), fix forward, and tag a new version. Do not move a tag that has been published. Package versions on ghcr can be deleted from the repository's Packages page.
