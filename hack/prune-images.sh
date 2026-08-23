#!/usr/bin/env bash
#
# Prunes old container image versions from GHCR.
#
# Every commit to main publishes an image, so the registry grows without bound. This keeps the
# releases forever, keeps the most recent builds, and deletes the rest.
#
# The reason this is not a one-liner is that the images are multi-architecture. A tagged image is
# an OCI index that only references the per-architecture manifests, and those show up in the
# package version list as untagged versions of their own. Deleting untagged versions on sight,
# which is the usual recipe, therefore breaks every tagged image it leaves behind: the tag still
# resolves, and the pull fails. So each version that is kept has its index read from the registry
# and the manifests it references are kept too.
#
# Dry run by default. Pass --delete to actually delete anything.

set -euo pipefail

OWNER="${OWNER:-}"
PACKAGE="${PACKAGE:-omni-infra-provider-bare-metal}"
# how many tagged builds to keep, on top of every release, newest first
KEEP="${KEEP:-10}"
DELETE=false

usage() {
  cat >&2 <<'USAGE'
usage: prune-images.sh [--delete] [--keep N] [--owner OWNER] [--package NAME]

Deletes old container image versions from ghcr.io, keeping every release, the most recent builds,
and the per-architecture manifests all of those reference.

Dry run unless --delete is given. Needs the gh CLI, authenticated with a token that carries
read:packages and delete:packages, or a GITHUB_TOKEN with packages: write inside Actions.
USAGE
  exit 2
}

while [ $# -gt 0 ]; do
  case "$1" in
    --delete) DELETE=true; shift ;;
    --keep) KEEP="${2:-}"; shift 2 ;;
    --owner) OWNER="${2:-}"; shift 2 ;;
    --package) PACKAGE="${2:-}"; shift 2 ;;
    -h|--help) usage ;;
    *) echo "unknown argument: $1" >&2; usage ;;
  esac
done

if [ -z "${OWNER}" ]; then
  echo "no owner: pass --owner or set OWNER" >&2
  exit 2
fi

for tool in gh jq curl; do
  command -v "${tool}" >/dev/null || { echo "${tool} is required but not installed" >&2; exit 1; }
done

# A release is a bare version. A build carries the `git describe` suffix, so v0.12.0 is kept
# forever and v0.12.0-7-g692cde5 ages out.
release_re='^v[0-9]+\.[0-9]+\.[0-9]+$'

echo "reading versions of ${OWNER}/${PACKAGE}"

# --paginate concatenates pages, so this streams one object per line and slurps them back into a
# single array rather than trying to parse the concatenation.
versions="$(gh api --paginate "/users/${OWNER}/packages/container/${PACKAGE}/versions" \
  --jq '.[] | {id, digest: .name, created: .created_at, tags: (.metadata.container.tags // [])}' \
  | jq -sc '.')"

total="$(jq 'length' <<<"${versions}")"

if [ "${total}" -eq 0 ]; then
  echo "no versions found, nothing to do"
  exit 0
fi

# Keep every release, then the newest KEEP tagged builds. Untagged versions are never kept on
# their own account: they are kept only by being referenced, which is resolved below.
keep_ids="$(jq -c --arg re "${release_re}" --argjson keep "${KEEP}" '
  [ (.[] | select((.tags | length) > 0)) ] as $tagged
  | ([ $tagged[] | select(any(.tags[]; test($re))) ]) as $releases
  | ([ $tagged[] | select(all(.tags[]; test($re) | not)) ]
      | sort_by(.created) | reverse | .[:$keep]) as $recent
  | [ ($releases + $recent)[].id ] | unique
' <<<"${versions}")"

if [ "$(jq 'length' <<<"${keep_ids}")" -eq 0 ]; then
  # Every version being unreferenced and untagged is not a state this should ever see, and it is
  # indistinguishable from the API having returned something unexpected. Deleting the whole
  # package is not a risk worth taking on that ambiguity.
  echo "refusing to continue: nothing would be kept, which is never right" >&2
  exit 1
fi

registry_token="$(curl -fsSL -u "x:${GITHUB_TOKEN:-}" \
  "https://ghcr.io/token?scope=repository:${OWNER}/${PACKAGE}:pull&service=ghcr.io" | jq -r '.token')"

if [ -z "${registry_token}" ] || [ "${registry_token}" = "null" ]; then
  echo "could not get a registry token for ${OWNER}/${PACKAGE}" >&2
  exit 1
fi

# Collect the digests to keep: the kept versions themselves, plus everything their indexes point
# at. A failure to read an index is fatal, because carrying on would delete children it lists.
keep_digests="$(mktemp)"
trap 'rm -f "${keep_digests}"' EXIT

while read -r id; do
  digest="$(jq -r --argjson id "${id}" '.[] | select(.id == $id) | .digest' <<<"${versions}")"
  echo "${digest}" >> "${keep_digests}"

  manifest="$(curl -fsSL -H "Authorization: Bearer ${registry_token}" \
    -H 'Accept: application/vnd.oci.image.index.v1+json' \
    -H 'Accept: application/vnd.docker.distribution.manifest.list.v2+json' \
    -H 'Accept: application/vnd.oci.image.manifest.v1+json' \
    -H 'Accept: application/vnd.docker.distribution.manifest.v2+json' \
    "https://ghcr.io/v2/${OWNER}/${PACKAGE}/manifests/${digest}")" || {
      echo "could not read the manifest of ${digest}, refusing to delete anything" >&2
      exit 1
    }

  # .manifests is the index case, and is absent for a single architecture image.
  jq -r '.manifests[]?.digest' <<<"${manifest}" >> "${keep_digests}"
done < <(jq -r '.[]' <<<"${keep_ids}")

sort -u -o "${keep_digests}" "${keep_digests}"

echo "keeping $(wc -l < "${keep_digests}") of ${total} versions, including referenced architectures"

deleted=0

while read -r id digest tags; do
  if grep -qxF "${digest}" "${keep_digests}"; then
    continue
  fi

  label="${tags:-<untagged>}"

  if [ "${DELETE}" = true ]; then
    gh api --silent --method DELETE \
      "/users/${OWNER}/packages/container/${PACKAGE}/versions/${id}"
    echo "deleted ${digest:0:19} ${label}"
  else
    echo "would delete ${digest:0:19} ${label}"
  fi

  deleted=$((deleted + 1))
done < <(jq -r '.[] | "\(.id) \(.digest) \(.tags | join(","))"' <<<"${versions}")

if [ "${deleted}" -eq 0 ]; then
  echo "nothing to prune"
elif [ "${DELETE}" != true ]; then
  echo "dry run: ${deleted} version(s) would be deleted, pass --delete to do it"
else
  echo "pruned ${deleted} version(s)"
fi
