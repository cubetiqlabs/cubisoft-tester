#!/usr/bin/env bash
# Cut a release: bump the version, commit, tag and push. The tag is what the
# GitHub workflow builds from, so pushing it is the whole release.
#
#   scripts/release.sh patch|minor|major   # bump from version.txt
#   scripts/release.sh 1.4.0               # set an exact version
#   scripts/release.sh 1.4.0 --force       # move an existing tag (local + remote)
set -euo pipefail
cd "$(dirname "$0")/.."

FORCE=false
BUMP=""
for arg in "$@"; do
  case "$arg" in
    --force|-f) FORCE=true ;;
    -h|--help) sed -n '2,8p' "$0" | sed 's/^# \?//'; exit 0 ;;
    *) BUMP="$arg" ;;
  esac
done
[ -n "$BUMP" ] || { echo "usage: $0 patch|minor|major|X.Y.Z [--force]" >&2; exit 1; }

CURRENT=$(tr -d '[:space:]' < version.txt)
IFS=. read -r MAJ MIN PAT <<< "$CURRENT"
case "$BUMP" in
  major) VERSION="$((MAJ + 1)).0.0" ;;
  minor) VERSION="${MAJ}.$((MIN + 1)).0" ;;
  patch) VERSION="${MAJ}.${MIN}.$((PAT + 1))" ;;
  *)     VERSION="${BUMP#v}" ;;
esac
[[ "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo "not a semver version: $VERSION" >&2; exit 1; }
TAG="v$VERSION"

# A dirty tree means the tag would not describe what gets built.
if [ -n "$(git status --porcelain --untracked-files=no)" ]; then
  echo "working tree has uncommitted changes; commit or stash them first" >&2
  exit 1
fi

if git rev-parse -q --verify "refs/tags/$TAG" >/dev/null && [ "$FORCE" = false ]; then
  echo "tag $TAG already exists; pass --force to move it" >&2
  exit 1
fi

echo "$VERSION" > version.txt
# Keep the packaged metadata in step so the installed app reports the same version.
sed -i.bak -E "s/^(  version: )\".*\"/\1\"$VERSION\"/" build/config.yml && rm build/config.yml.bak
for plist in build/darwin/Info.plist build/darwin/Info.dev.plist; do
  [ -f "$plist" ] || continue
  sed -i.bak -E "/<key>CFBundle(Short)?Version(String)?<\/key>/{n;s|<string>.*</string>|<string>$VERSION</string>|;}" "$plist"
  rm "$plist.bak"
done

git add version.txt build/config.yml build/darwin/Info.plist build/darwin/Info.dev.plist 2>/dev/null || true
if ! git diff --cached --quiet; then
  git commit -m "release: $TAG"
fi

BRANCH=$(git rev-parse --abbrev-ref HEAD)
git push origin "$BRANCH"

if [ "$FORCE" = true ]; then
  git tag -f -a "$TAG" -m "$TAG"
  git push --force origin "refs/tags/$TAG"
else
  git tag -a "$TAG" -m "$TAG"
  git push origin "refs/tags/$TAG"
fi

echo "pushed $TAG — the release workflow builds and publishes from here:"
echo "  https://github.com/cubetiqlabs/cubisoft-tester/actions"
