#!/usr/bin/env bash
# Every commit in BASE..HEAD (merge commits aside) must carry a Developer Certificate of Origin
# sign-off: a "Signed-off-by: Name <email>" line whose address is the commit author's. Add it with
# `git commit -s`; fix commits already made with `git rebase --signoff main` (see CONTRIBUTING.md).
# Commits by bots (author name ending in [bot], such as Renovate) are exempt.
#
#   scripts/check-dco.sh BASE HEAD
set -euo pipefail
base="${1:?usage: check-dco.sh BASE HEAD}"; head="${2:?usage: check-dco.sh BASE HEAD}"
bad=0
for c in $(git rev-list --no-merges "$base..$head"); do
  author="$(git log -1 --format='%an' "$c")"; email="$(git log -1 --format='%ae' "$c")"
  short="$(git log -1 --format='%h %s' "$c" | cut -c1-80)"
  case "$author" in *"[bot]") continue ;; esac
  signoffs="$(git log -1 --format='%B' "$c" | grep -iE '^Signed-off-by: .+ <[^>]+@[^>]+>[[:space:]]*$' || true)"
  if [ -z "$signoffs" ]; then
    echo "missing sign-off: $short" >&2; bad=1
  elif ! printf '%s\n' "$signoffs" | grep -qiF "<$email>"; then
    echo "sign-off is not by the author <$email>: $short" >&2; bad=1
  fi
done
if [ "$bad" = 1 ]; then
  echo >&2
  echo "Every commit needs 'Signed-off-by: Your Name <you@example.com>' (the Developer Certificate of Origin, see DCO)." >&2
  echo "For the last commit: git commit --amend -s --no-edit     For several: git rebase --signoff $base" >&2
  exit 1
fi
echo "all commits are signed off"
