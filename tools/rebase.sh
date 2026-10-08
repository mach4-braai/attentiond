#!/usr/bin/env bash
# Rebase a PR in a detached worktree; push only with the original head lease.
set -euo pipefail

if [[ $# -ne 2 || ! $1 =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ || ! $2 =~ ^[1-9][0-9]*$ || ! ${ATTENTIOND_CHECKOUT:-} = /* ]]; then
  echo 'usage: ATTENTIOND_CHECKOUT=/absolute/clone tools/rebase.sh owner/repo PR' >&2
  exit 2
fi
repo=$1
pr=$2
checkout=$ATTENTIOND_CHECKOUT
refs=$(gh pr view "$pr" -R "$repo" --json headRefName,baseRefName --jq '.headRefName + " " + .baseRefName')
read -r head base <<< "$refs"
if [[ -z ${head:-} || -z ${base:-} || $refs != "$head $base" ]] ||
   ! git check-ref-format "refs/heads/$head" ||
   ! git check-ref-format "refs/heads/$base"; then
  echo 'invalid PR head or base ref' >&2
  exit 1
fi

git -C "$checkout" fetch origin "+refs/heads/$head:refs/remotes/origin/$head" "+refs/heads/$base:refs/remotes/origin/$base"
lease=$(git -C "$checkout" rev-parse "refs/remotes/origin/$head")
base_sha=$(git -C "$checkout" rev-parse "refs/remotes/origin/$base")
wt="${checkout}-wt/rebase-${pr}-$(date +%Y%m%d%H%M%S)-$$"
mkdir -p "${checkout}-wt"
git -C "$checkout" worktree add --detach "$wt" "$lease"
result=''
cleanup() {
  status=$?
  trap - EXIT INT TERM
  echo '--- worktree state at exit'
  git -C "$wt" status || true
  git -C "$wt" diff --stat || true
  git -C "$wt" log --oneline "$base_sha..HEAD" || true
  git -C "$checkout" worktree remove --force "$wt" || status=1
  if [[ -n $result && $status -eq 0 ]]; then
    echo "RESULT: $result"
  fi
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

rebasing() {
  [[ -e $(git -C "$wt" rev-parse --path-format=absolute --git-path rebase-merge) ||
     -e $(git -C "$wt" rev-parse --path-format=absolute --git-path rebase-apply) ]]
}

if git -C "$wt" merge-base --is-ancestor "$base_sha" HEAD; then
  result='up-to-date'
elif git -C "$wt" rebase "$base_sha"; then
  if ! rebasing &&
     [[ -z $(git -C "$wt" status --porcelain) ]] &&
     git -C "$wt" merge-base --is-ancestor "$base_sha" HEAD; then
    git -C "$wt" push --force-with-lease="refs/heads/$head:$lease" origin "HEAD:refs/heads/$head"
    result="rebased $(git -C "$wt" rev-parse HEAD)"
  else
    echo 'not pushing: unfinished or invalid rebase' >&2
    exit 1
  fi
else
  files=$(git -C "$wt" diff --name-only --diff-filter=U | paste -sd, -)
  git -C "$wt" rebase --abort
  result="needs-conflicts ${files:-unknown}"
fi
