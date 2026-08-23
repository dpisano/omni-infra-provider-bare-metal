This fork's release of upstream __VERSION__, cut automatically by `.github/workflows/upstream-release.yaml`.

It is this fork's `main` at the point it contained upstream's __VERSION__ release commit, so it carries everything in [upstream's __VERSION__ release](https://github.com/__UPSTREAM_REPO__/releases/tag/__VERSION__) plus what this fork adds on top: support for machines with no BMC, whose power a human controls, and Raspberry Pi network boot. See the readme for those.

The version number is upstream's, and refers to the upstream release this one corresponds to. It is not an independent version of this fork, and the tag points at a different commit than upstream's tag of the same name.

```text
__IMAGE__
```

Read upstream's release notes for the changes themselves, and their upgrade notes before upgrading.
