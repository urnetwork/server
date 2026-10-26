#!/usr/bin/env bash
# Install a new immutable, inactive C2 builder release; never replace an old one.
set -Eeuo pipefail
umask 077
[[ $# == 2 && $1 == --install-reviewed-c2-bundle && $2 =~ ^[0-9a-f]{64}$ ]] || exit 2
[[ $(id -u) == 0 ]] || exit 2
readonly ROOT=/home/by/urnetwork/temp/mono-root-c2-release-20261026.PUHdHLML
readonly CONTROL="$ROOT/trusted-control/server-qualified-v3"
readonly SOURCE="$ROOT/trusted-bundle-7c4dc74e5"
readonly REVISION=7c4dc74e5d53d7b42e22e059565d778b35bee83e
readonly RELEASE=/usr/local/libexec/urnetwork/competition-c2-a9e8e1a7-control-7c4dc74e5-go1273-v1
readonly VALIDATOR_SHA256="$2"
readonly TOOLCHAIN=/home/by/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.27.3.linux-amd64/bin
[[ ! -e "$RELEASE" && ! -L "$RELEASE" ]]
[[ $(realpath -e /usr/local/libexec/urnetwork) == /usr/local/libexec/urnetwork ]]
[[ $(sha256sum "$ROOT/trusted-control/competitionpatch" | cut -d' ' -f1) == "$VALIDATOR_SHA256" ]]
[[ $(sed -n 's/.*vcs.revision=//p' "$ROOT/trusted-control/validator-buildinfo.txt") == "$REVISION" ]]
[[ $(sed -n 's/.*vcs.modified=//p' "$ROOT/trusted-control/validator-buildinfo.txt") == false ]]
grep -qx exit=0 "$ROOT/trusted-control/validator-build.terminal"
[[ $(sudo -n -u by git -C "$CONTROL" rev-parse HEAD) == "$REVISION" ]]
[[ -z $(sudo -n -u by git -C "$CONTROL" status --porcelain=v1 --untracked-files=all) ]]
[[ -z $(find "$SOURCE" -mindepth 1 ! -type f ! -type d -print -quit) ]]
while IFS= read -r -d '' file; do
    relative="${file#"$SOURCE/"}"
    expected="$(sudo -n -u by git -C "$CONTROL" show "$REVISION:connect/sim-latency/evaluator/container/$relative" | sha256sum | cut -d' ' -f1)"
    [[ $(sha256sum "$file" | cut -d' ' -f1) == "$expected" ]]
done < <(find "$SOURCE" -type f -print0)
staging="$(mktemp -d /usr/local/libexec/urnetwork/.competition-c2-a9e8e1a7-control-7c4dc74e5.XXXXXXXX)"
install -d -m 0755 "$staging/container"
cp -a "$SOURCE/." "$staging/container/"
install -m 0555 "$ROOT/trusted-control/competitionpatch" "$staging/container/competitionpatch"
printf '%s\n' "$VALIDATOR_SHA256" > "$staging/container/competitionpatch.sha256"
chown -R 0:0 "$staging"
find "$staging" -type d -exec chmod 0755 {} +
find "$staging" -type f -exec chmod 0444 {} +
find "$staging" -type f -name '*.sh' -exec chmod 0555 {} +
chmod 0555 "$staging/container/competitionpatch"
mv -T "$staging" "$RELEASE"
builder_identity="$("$RELEASE/container/submission-builder-identity.sh")"
jq -nS --arg control_revision "$REVISION" --arg release "$RELEASE" \
    --arg validator_sha256 "$VALIDATOR_SHA256" --argjson builder "$builder_identity" \
    --arg installed_at "$(date -u +%FT%TZ)" \
    '{schema:1,kind:"sim-latency-c2-inactive-builder-release",installed_at:$installed_at,
      control_revision:$control_revision,release_directory:$release,
      validator_sha256:$validator_sha256,builder:$builder,
      measured_source_revision:"a9e8e1a710b24c749e6499a0606938252c83e136",
      evaluator_image_digest:"sha256:b1632a55b415f823871c60b220a194947d280894111c9fa765ad88100e206f24",
      qualified:false,activated:false,old_releases_modified:false}' \
    > "$ROOT/trusted-control/bundle-install-receipt.json"
install -m 0444 "$ROOT/trusted-control/bundle-install-receipt.json" "$RELEASE/release.json"
find "$RELEASE" -type f -print0 | sort -z | xargs -0 sha256sum > "$ROOT/trusted-control/installed-bundle.sha256"
sha256sum "$ROOT/trusted-control/bundle-install-receipt.json" "$ROOT/trusted-control/installed-bundle.sha256"
printf '%s\n' "$builder_identity"
printf 'installed_inactive_unqualified_release=%s\n' "$RELEASE"
