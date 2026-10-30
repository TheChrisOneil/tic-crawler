#!/usr/bin/env bash
# Keep crawl state and catalog outputs on the `data` branch so hands-free runs build on each other
# and the dashboard can read them. Usage: data_branch.sh restore | publish "<message>" | dashboard "<message>"
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
    find "$WORK/out" -type f -size +50M -print -delete
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
  dashboard)
    # The repo is private, so Vercel serves the dashboard JSON from main:dashboard/data/.
    msg="${2:-update dashboard data}"
    SITE="${RUNNER_TEMP:-/tmp}/tic-site"
    rm -rf "$SITE"
    git clone -q --depth 1 -b "${SITE_BRANCH:-main}" "$REPO_URL" "$SITE"
    rm -rf "$SITE/dashboard/data"; mkdir -p "$SITE/dashboard/data"
    cp -r out/dashboard/. "$SITE/dashboard/data/"
    cd "$SITE"
    git config user.name "tic-crawler bot"
    git config user.email "actions@github.com"
    git add -A dashboard/data
    if git diff --cached --quiet; then echo "dashboard data unchanged"; exit 0; fi
    git commit -q -m "$msg"
    for i in 1 2 3; do
      if git push -q origin "HEAD:${SITE_BRANCH:-main}"; then echo "dashboard data pushed"; exit 0; fi
      git pull -q --rebase origin "${SITE_BRANCH:-main}" || true
      sleep $((i * 5))
    done
    exit 1
    ;;
  *) echo "usage: $0 restore|publish [message]|dashboard [message]" >&2; exit 2 ;;
esac
