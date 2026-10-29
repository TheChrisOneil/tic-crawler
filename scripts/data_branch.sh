#!/usr/bin/env bash
# Keep crawl state and catalog outputs on the `data` branch so hands-free runs build on each other
# and the dashboard can read them. Usage: data_branch.sh restore | publish "<message>"
set -euo pipefail
BRANCH="${DATA_BRANCH:-data}"
WORK="${RUNNER_TEMP:-/tmp}/tic-data"
REPO_URL="https://x-access-token:${GITHUB_TOKEN}@github.com/${GITHUB_REPOSITORY}"

case "${1:-}" in
  restore)
    rm -rf "$WORK"
    if git clone -q --depth 1 -b "$BRANCH" "$REPO_URL" "$WORK" 2>/dev/null; then
      mkdir -p state out
      [ -d "$WORK/state" ] && cp -r "$WORK/state/." state/
      [ -d "$WORK/out" ] && cp -r "$WORK/out/." out/
      echo "restored state and outputs from $BRANCH"
    else
      echo "no $BRANCH branch yet; starting fresh"
    fi
    ;;
  publish)
    msg="${2:-update data}"
    if [ ! -d "$WORK/.git" ]; then
      rm -rf "$WORK"; mkdir -p "$WORK"; git -C "$WORK" init -q -b "$BRANCH"
      git -C "$WORK" remote add origin "$REPO_URL"
    fi
    mkdir -p "$WORK/state" "$WORK/out"
    cp -r state/. "$WORK/state/" 2>/dev/null || true
    cp -r out/. "$WORK/out/"
    # Raw MRFs never go here; keep the branch small by dropping very large stored TOCs.
    rm -rf "$WORK/out/raw"
    find "$WORK/out" -name '*.json.gz' -size +50M -print -delete
    cat > "$WORK/README.md" <<'MD'
# tic-crawler data branch

Written by GitHub Actions. `state/tic.sqlite` is the crawl state; `out/` holds TOCs, manifests,
run logs, the access audit and `out/dashboard/` (read by the dashboard). Do not edit by hand.
MD
    cd "$WORK"
    git config user.name "tic-crawler bot"
    git config user.email "actions@github.com"
    git add -A
    if git diff --cached --quiet; then echo "nothing to publish"; exit 0; fi
    git commit -q -m "$msg"
    for i in 1 2 3; do
      if git push -q origin "HEAD:$BRANCH"; then echo "published to $BRANCH"; exit 0; fi
      git pull -q --rebase -X theirs origin "$BRANCH" || true
      sleep $((i * 5))
    done
    exit 1
    ;;
  *) echo "usage: $0 restore|publish [message]" >&2; exit 2 ;;
esac
