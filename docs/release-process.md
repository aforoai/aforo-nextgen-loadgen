# Release process

This document is for the maintainer who is shipping a release. The
release pipeline is automated end-to-end — push a tag, watch the
Actions tab, the binaries and Homebrew formula update on their own.

## Versioning

`aforo-loadgen` follows Semantic Versioning 2.0:

| Bump      | When                                                                             |
| --------- | -------------------------------------------------------------------------------- |
| **MAJOR** | An incompatible scenario YAML schema change, a removed CLI flag, a renamed binary. |
| **MINOR** | New subcommand, new scenario, new flag with a backward-compatible default.         |
| **PATCH** | Bug fix, doc change, dependency bump that does not change the public surface.      |

Until `v1.0.0` the loader is in pre-stability. The `Unreleased` section
in `CHANGELOG.md` records intent — turn that into a release section
under the new version number when you are ready to tag.

## Cutting a release

### 1. Land all changes on `main`

A release is always cut from `main`. If you need to release a hotfix
for a previous version, branch from the tag, fix, then cherry-pick to
main when convenient — but that branch never gets a tag of its own.
Single-trunk releases only.

### 2. Update CHANGELOG.md

Every release MUST have a section in `CHANGELOG.md`. The section heading
is the version number in `[N.N.N]` square brackets, followed by an
ISO-8601 date. Under it, group changes into the standard Keep-a-Changelog
buckets: `Added`, `Changed`, `Deprecated`, `Removed`, `Fixed`,
`Security`. Drop empty buckets — only print headers you have content for.

When the section is in place, move whatever was under `Unreleased` into
the new section, and add a fresh empty `Unreleased` heading at the top.

Update the link references at the bottom of the file too — both the
new tag's diff link and the comparison link from the new tag to HEAD.

Commit the changelog change on `main` before tagging. The tag then
points at the commit that includes its own changelog entry.

### 3. Tag and push

```sh
TAG=v0.1.1
git tag -a "$TAG" -m "Release $TAG

<paste the new CHANGELOG section body here>"
git push origin "$TAG"
```

The tag's annotation message becomes part of the GitHub Release body.
Keep it informative — it is the closest thing operators read in the
wild.

### 4. Watch the workflow

`https://github.com/aforoai/aforo-nextgen-loadgen/actions/workflows/release.yml`

The pipeline runs three jobs:

1. **build + test** — full test suite with race detector. Fails block.
2. **golangci-lint** — same lint config as `ci.yml`. Fails block.
3. **goreleaser** — cross-compiles, archives, checksums, publishes the
   GitHub Release, regenerates the Homebrew formula in
   `aforoai/aforo-nextgen-homebrew-tap`.

Total runtime is typically 4–6 minutes.

### 5. Verify the release artifacts

Once the workflow goes green:

- Open the GitHub Release page. You should see four `.tar.gz` archives
  (darwin amd64, darwin arm64, linux amd64, linux arm64) and one
  `checksums.txt`.
- Download a checksum'd archive and confirm its SHA-256 matches the
  line in `checksums.txt`.
- Run `brew update && brew upgrade aforoai/tap/loadgen`. The version
  printed by `aforo-loadgen version` should be the new tag, with the
  build's commit SHA and ISO-8601 build date.

If any of those three checks fail, the release is broken — file a
follow-up immediately. Do **not** silently re-tag the same version
(SemVer forbids reusing a tag, and Homebrew already cached the
formula at the broken SHA).

### 6. Recovery: yanking a broken release

If the release is broken (binary segfaults, formula is malformed):

1. Mark the GitHub Release as "pre-release" on the Release page.
2. Tag a new patch release with the fix (`v0.1.2`).
3. Publish the new release through the normal flow.

Do **not** delete the tag or the Release page — Homebrew users who
already installed the broken version need a discoverable record of what
shipped when. The replacement tag is the source of truth going forward.

## Local dry-runs

Before tagging, validate the release config locally:

```sh
make release-check     # validates .goreleaser.yaml without building
make release           # snapshot build (no tag, no publish, no formula push)
```

The snapshot build emits `dist/aforo-loadgen_Darwin_x86_64.tar.gz` and
the other three platforms into `./dist/`. Inspect any of them; they
should look identical in shape to a real release archive.

## What changes in CI on each release

The `loadgen-smoke` workflows in the four service repos use
`go install ...@latest`. That picks up the new tag automatically on the
next PR run — no service-side change is required.

If a release introduces a new mandatory CLI flag and you do not want the
service repos' smoke jobs to break the moment they pick up the new
binary, ship a backwards-compatible default or pin the four service
workflows to `@vX.Y.Z` until they migrate.
