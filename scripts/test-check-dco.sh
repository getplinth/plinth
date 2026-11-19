#!/usr/bin/env bash
# Tests for check-dco.sh on a throwaway repository.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
cd "$tmp"
git init -q -b main; git config user.name "Ada Dev"; git config user.email ada@example.test; git config commit.gpgsign false
git commit -q --allow-empty -m "base"
base="$(git rev-parse HEAD)"
fails=0
expect() { # description wanted-exit
  local got=0; "$here/check-dco.sh" "$base" HEAD >/dev/null 2>&1 || got=$?
  if [ "$got" -ne "$2" ]; then echo "FAIL: $1 (exit $got, wanted $2)"; fails=1; else echo "ok:   $1"; fi
}
git commit -q --allow-empty -s -m "signed commit";                       expect "a signed-off commit passes" 0
git commit -q --allow-empty -m "unsigned commit";                        expect "an unsigned commit fails" 1
git reset -q --hard HEAD~1
git commit -q --allow-empty -m "wrong person

Signed-off-by: Someone Else <else@example.test>";                        expect "a sign-off by someone else fails" 1
git reset -q --hard HEAD~1
git commit -q --allow-empty -m "case differs

Signed-off-by: Ada Dev <ADA@Example.test>";                              expect "the author's address matches case-insensitively" 0
git reset -q --hard HEAD~1
git -c user.name="renovate[bot]" -c user.email=bot@example.test commit -q --allow-empty -m "bump a dependency"; expect "a bot commit is exempt" 0
git reset -q --hard HEAD~1
git commit -q --allow-empty -m "text mentioning Signed-off-by: Ada Dev <ada@example.test> inline only"; expect "a sign-off that is not on its own line fails" 1
git reset -q --hard HEAD~1
git checkout -q -b side; git commit -q --allow-empty -s -m "on a branch"
git checkout -q main; git commit -q --allow-empty -s -m "on main"
git merge -q --no-ff side -m "merge without a sign-off";                 expect "a merge commit needs none" 0
exit "$fails"
