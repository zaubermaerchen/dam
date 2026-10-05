# Release and Homebrew automation

Tag pushes matching the existing SemVer release format build seven archives, verify their contents, create `SHA256SUMS`, and publish a GitHub release. After that publication succeeds, a plain stable tag (`vX.Y.Z`) calls the [homebrew-tap shared workflow](https://github.com/zaubermaerchen/homebrew-tap/blob/main/docs/release-automation.md) to update `Formula/dam.rb`. Prerelease and build-metadata tags still publish releases but do not start a Homebrew update. Pull requests still run the release workflow's tests and builds without publishing.

The shared workflow reads the published release and checksums, tests the candidate formula on Linux and macOS, then opens a reviewable PR in `zaubermaerchen/homebrew-tap`. A maintainer merges that PR before the new formula is available through `brew update`.

Configure the following in the dam repository before publishing a stable release:

- Repository variable `HOMEBREW_TAP_APP_ID`: the GitHub App ID.
- Actions secret `HOMEBREW_TAP_APP_PRIVATE_KEY`: the App's PEM private key.

The App is installed only on `homebrew-tap`, with repository **Contents: read/write** and **Pull requests: read/write** permissions. The dam caller's `GITHUB_TOKEN` has `contents: read` for release metadata. The caller pins both the shared workflow and `automation-ref` to the same reviewed tap commit.

If the Homebrew update fails after the release is published, inspect the failed job and follow the [tap retry guidance](https://github.com/zaubermaerchen/homebrew-tap/blob/main/docs/release-automation.md). The release publisher refuses an existing release tag, so do not rerun that successful job to retry Formula updates. A run created before this caller was merged cannot gain the new job through a rerun.
