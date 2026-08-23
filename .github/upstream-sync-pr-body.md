Merges `siderolabs/omni-infra-provider-bare-metal` into this fork. Opened automatically once a week by `.github/workflows/upstream-sync.yaml`.

`hack/fork-policy.sh` has already reapplied the deletions this fork makes on purpose, so the workflows and secrets upstream keeps editing are gone again and are not part of this diff.

Read it before merging. This fork diverges in ways an upstream change can invalidate without conflicting textually, most notably the arm64 agent mode extension list in `internal/provider/imagefactory/client.go`, which deliberately holds only what a single board computer needs: an extension added upstream will not reach it.

Note that GitHub does not run workflows on a pull request opened with `GITHUB_TOKEN`, so the usual checks are absent here. Close and reopen this pull request to run them.
