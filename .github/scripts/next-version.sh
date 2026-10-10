#!/usr/bin/env bash
# Prints the next release version (without "v") from conventional commit
# titles since the last vX.Y.Z tag, or nothing if there's nothing new:
#   feat!: / fix!: or "BREAKING CHANGE" → major (minor while still 0.x)
#   feat:                               → minor
#   anything else                       → patch
# With no tags yet it prints $INITIAL (default 0.1.0).
set -euo pipefail

last=$(git describe --tags --abbrev=0 --match 'v[0-9]*.[0-9]*.[0-9]*' 2>/dev/null || true)
if [[ -z $last ]]; then
  echo "${INITIAL:-0.1.0}"
  exit
fi

titles=$(git log --format=%s "$last..HEAD")
[[ -z $titles ]] && exit 0
bodies=$(git log --format=%b "$last..HEAD")

IFS=. read -r major minor patch <<<"${last#v}"
if grep -qE '^[a-z]+(\([^)]*\))?!:' <<<"$titles" || grep -q 'BREAKING CHANGE' <<<"$bodies"; then
  if ((major == 0)); then echo "0.$((minor + 1)).0"; else echo "$((major + 1)).0.0"; fi
elif grep -qE '^feat(\([^)]*\))?:' <<<"$titles"; then
  echo "$major.$((minor + 1)).0"
else
  echo "$major.$minor.$((patch + 1))"
fi
