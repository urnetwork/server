# Source-owned qualification-index predicates. The caller owns run_owned and
# its finite command budgets; these functions never stage changed file bytes.
qualification_authenticate_present() {
  local name=$1 index=$2 destination=$3
  shift 3
  # read-tree deliberately has no stat cache. Refresh rehashes present files,
  # rejects actual content/mode differences, and leaves missing paths unstaged.
  run_owned "$io_seconds" "$name-refresh-present" env GIT_INDEX_FILE="$index" git "$@" -C "$destination" update-index --really-refresh --ignore-missing || return
  run_owned "$io_seconds" "$name-authenticate-present" env GIT_INDEX_FILE="$index" git "$@" -C "$destination" diff-files --no-ext-diff --quiet --diff-filter=ACMRTUXB
}

# Literal directory exclusion must not swallow a prefix-sibling missing file.
qualification_select_missing() {
  local name=$1 index=$2 destination=$3 excluded=$4 output=$5
  shift 5
  local missing_options=(.) missing
  if [[ -n $excluded ]]; then missing_options+=(":(exclude,literal)$excluded"); fi
  command_output="$output" run_owned "$metadata_seconds" "$name-select-only-missing" env GIT_INDEX_FILE="$index" git "$@" -C "$destination" ls-files --deleted -z -- "${missing_options[@]}" || return
  if [[ -n $excluded ]]; then
    while IFS= read -r -d '' missing; do
      [[ $missing != "$excluded"* ]] || return 1
    done < "$output"
  fi
}
