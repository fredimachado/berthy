#!/bin/bash
# 14-clean-checkout proves what a release records when the manifest directory is a git checkout. A deploy from a clean
# checkout records HEAD without --commit. An edited overlay is a local change: the deploy warns with its name, and history
# marks the release modified next to its commit. A --commit that is not HEAD is kept, with a warning. A protected
# environment refuses a deploy from local changes before anything is sent, and accepts it once the files are committed. A
# manifest directory outside a git work tree deploys as it always did: no commit, no warning, no refusal.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"
. "$RUN/digests.env"

overlay="$MAN/berthy.$ENVN.yml"

# newest_commit: the commit of the newest history row, as the text output prints it.
newest_commit() { adm history web 2>&1 | sed -n 's/^commit: //p' | head -n 1; }

# The checkout: the base manifest and an overlay, both committed.
git -C "$MAN" init -q
git -C "$MAN" config user.name e2e
git -C "$MAN" config user.email e2e@example.com
printf 'apps:\n  web:\n    env:\n      plain:\n        LOCAL_EDIT: "0"\n' > "$overlay"
git -C "$MAN" add -A
git -C "$MAN" commit -q -m "manifest"
head1=$(git -C "$MAN" rev-parse HEAD)

out=$(adm deploy web --version v1 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy from a clean checkout" 0 "$code" "$out"
expect_not_contains "a clean checkout prints no warning" "$out" "warning:"
hist=$(adm history web --json 2>&1)
expect_contains "the release records HEAD without --commit" "$hist" "\"commit\": \"$head1\""
expect_not_contains "a clean release is not marked modified" "$hist" "modified"

# An edited overlay, not committed, is a local change.
sed -i 's/LOCAL_EDIT: "0"/LOCAL_EDIT: "1"/' "$overlay"
out=$(adm deploy web --version v2 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy from an edited overlay" 0 "$code" "$out"
expect_contains "the warning names the overlay" "$out" "warning: the manifest has local changes: berthy.$ENVN.yml"
expect_contains "history marks the release modified next to its commit" "$(adm history web 2>&1)" "commit: $head1 (modified)"

# A --commit that is not HEAD is kept, with a warning.
out=$(adm deploy web --version v1 --manifest-dir "$MAN" --commit deadbeef 2>&1); code=$?
expect_code "deploy with a commit that is not HEAD" 0 "$code" "$out"
expect_contains "a commit that is not HEAD gets a warning" "$out" "warning: --commit deadbeef is not HEAD ($head1)"
expect_contains "history keeps the given commit" "$(adm history web 2>&1)" "commit: deadbeef (modified)"

# Protected: the base manifest says so and is committed; the overlay is still edited. The deploy by digest is refused
# before anything is sent, so history does not change.
{ base_manifest; echo "protected: true"; } > "$MAN/berthy.yml"
git -C "$MAN" add berthy.yml
git -C "$MAN" commit -q -m "protect"
before=$(adm history web --json 2>&1)
out=$(adm deploy web --version "$WEB_V1_DIGEST" --manifest-dir "$MAN" 2>&1); code=$?
expect_code "a protected environment refuses local changes" 1 "$code" "$out"
expect_contains "the refusal names the overlay" "$out" "the manifest has local changes: commit them first: berthy.$ENVN.yml"
expect_eq "the refused deploy leaves no record" "$before" "$(adm history web --json 2>&1)"

# Committed, the same overlay deploys to the protected environment, and the newest release records the new HEAD.
git -C "$MAN" add "berthy.$ENVN.yml"
git -C "$MAN" commit -q -m "overlay"
head3=$(git -C "$MAN" rev-parse HEAD)
out=$(adm deploy web --version "$WEB_V2_DIGEST" --manifest-dir "$MAN" 2>&1); code=$?
expect_code "a clean checkout deploys to the protected environment" 0 "$code" "$out"
expect_eq "the newest release records HEAD and is not modified" "$head3" "$(newest_commit)"
spec=$(service_spec "$PROJECT-web-main")
expect_contains "the service runs the pinned digest" "$spec" "$WEB_V2_DIGEST"

# Outside a git work tree: a copy without .git, with an edited overlay, deploys with no commit and no check.
COPY="$E2E_ROOT/manifest-copy"
rm -rf "$COPY"
mkdir -p "$COPY"
cp -a "$MAN/." "$COPY/"
rm -rf "$COPY/.git"
sed -i 's/LOCAL_EDIT: "1"/LOCAL_EDIT: "2"/' "$COPY/berthy.$ENVN.yml"
out=$(adm deploy web --version "$WEB_V1_DIGEST" --manifest-dir "$COPY" 2>&1); code=$?
expect_code "a manifest outside a git work tree deploys" 0 "$code" "$out"
expect_not_contains "outside a git work tree nothing is checked" "$out" "warning:"
expect_eq "the release records no commit" "" "$(newest_commit)"

finish
