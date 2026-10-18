#!/usr/bin/env bash
# Ask omp to resolve a PR rebase; only the wrapper may push.
set -euo pipefail

if [[ $# -ne 2 || ! $1 =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ || ! $2 =~ ^[1-9][0-9]*$ || ! ${ATTENTIOND_CHECKOUT:-} = /* ]]; then
  echo 'usage: ATTENTIOND_CHECKOUT=/absolute/clone tools/agent-rebase.sh owner/repo PR' >&2
  exit 2
fi
repo=$1
pr=$2
checkout=$ATTENTIOND_CHECKOUT
policy=$(dirname "$0")/agent-rebase.yml
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
wt="${checkout}-wt/agent-rebase-${pr}-$(date +%Y%m%d%H%M%S)-$$"
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

prompt="You are in a disposable detached worktree of $repo PR #$pr at $lease. Rebase onto the base ref $base at $base_sha. Do not push or remove the worktree. The wrapper alone pushes.
Run one git command per bash call. Never join commands with &&, ||, ; or pipes. No builds or tests.
If the base is already an ancestor of HEAD, end with RESULT: up-to-date.
Otherwise rebase onto $base_sha. Resolve only conflicts whose correct result is clear from both sides. Stage resolved files and continue. If anything is ambiguous, abort the rebase and end with RESULT: needs-human <conflicting files and why>.
End with exactly one line: RESULT: rebased <sha> | RESULT: up-to-date | RESULT: needs-human <reason>."

if output=$(GIT_EDITOR=true GIT_TERMINAL_PROMPT=0 omp -p --cwd "$wt" \
  --model "${ATTENTIOND_MODEL:-anthropic/claude-sonnet-5-5}" \
  --thinking "${ATTENTIOND_THINKING:-medium}" \
  --approval-mode write --config "$policy" \
  --tools read,grep,glob,edit,bash --max-time 15m --no-title "$prompt" 2>&1); then
  printf '%s\n' "$output"
else
  status=$?
  printf '%s\n' "$output"
  echo "not pushing: omp exited $status" >&2
  exit "$status"
fi
agent_result=''
while IFS= read -r line; do
  case "$line" in 'RESULT: '*) agent_result=${line#RESULT: } ;; esac
done <<< "$output"

case "$agent_result" in
  rebased\ *)
    if rebasing ||
       [[ -n $(git -C "$wt" status --porcelain) ]] ||
       ! git -C "$wt" merge-base --is-ancestor "$base_sha" HEAD; then
      result='needs-human unfinished rebase or dirty worktree'
    else
      git -C "$checkout" fetch origin "+refs/heads/$base:refs/remotes/origin/$base"
      if git -C "$wt" merge-base --is-ancestor "$(git -C "$checkout" rev-parse "refs/remotes/origin/$base")" HEAD; then
        git -C "$wt" push --force-with-lease="refs/heads/$head:$lease" origin "HEAD:refs/heads/$head"
        result="rebased $(git -C "$wt" rev-parse HEAD)"
      else
        result='needs-human base moved during rebase'
      fi
    fi
    ;;
  up-to-date)
    if rebasing ||
       [[ -n $(git -C "$wt" status --porcelain) ]] ||
       ! git -C "$wt" merge-base --is-ancestor "$base_sha" HEAD; then
      result='needs-human unfinished rebase or dirty worktree'
    else
      result='up-to-date'
    fi
    ;;
  needs-human\ *) result=$agent_result ;;
  *) result='needs-human agent did not return a valid result' ;;
esac
