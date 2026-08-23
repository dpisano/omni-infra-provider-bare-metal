#!/usr/bin/env bash
#
# Re-applies the deletions this fork deliberately makes, which two things keep undoing:
#
#   * merging upstream, which reintroduces any of these files that upstream has touched, as a
#     modify/delete conflict that git cannot resolve on its own
#   * make rekres, which regenerates every workflow, since kres offers no way to turn that off
#
# The resolution is always the same, so it is scripted rather than done by hand every time. See the
# GitHub workflows section of AGENTS.md for why each of these is unwanted here.

set -euo pipefail

# Workflows that only work inside the Sidero Labs infrastructure, and the encrypted secrets they
# read. The hand-written docker-image.yaml and pull-request.yaml are deliberately not in this list.
PATHS=(
  .github/workflows/ci.yaml
  .github/workflows/e2e-cron.yaml
  .github/workflows/lock.yml
  .github/workflows/slack-notify.yaml
  .github/workflows/slack-notify-ci-failure.yaml
  .github/workflows/stale.yml
  .secrets.yaml
  .sops.yaml
)

cd "$(git rev-parse --show-toplevel)"

removed=0

for path in "${PATHS[@]}"; do
  before_tracked=false
  if git ls-files --error-unmatch -- "$path" >/dev/null 2>&1; then
    before_tracked=true
  fi

  # Drops the path whether it is tracked, staged, or left unmerged by a conflicted merge.
  # --ignore-unmatch keeps this quiet when the path is untracked or simply absent.
  git rm -qf --ignore-unmatch -- "$path" >/dev/null 2>&1 || true

  # An untracked copy, which is what make rekres leaves behind, survives the above.
  if [ -e "$path" ]; then
    rm -f -- "$path"
    echo "removed untracked $path"
    removed=$((removed + 1))
  elif [ "$before_tracked" = true ]; then
    echo "removed tracked $path"
    removed=$((removed + 1))
  fi
done

if [ "$removed" -eq 0 ]; then
  echo "fork policy already applied, nothing to remove"
fi
