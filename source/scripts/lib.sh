# shellcheck shell=bash
# Sourced helper. load_env reads ./.env (KEY=VALUE lines, optional quotes) but
# never overrides variables that are already set in the environment, so
# `make images TAG=v2` or `TAG=v2 scripts/render.sh` win over .env.
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

load_env() {
  local f="${ENV_FILE:-$ROOT/.env}" line key val
  [ -f "$f" ] || { echo "note: $f not found (copy .env.example to .env)" >&2; return 0; }
  while IFS= read -r line || [ -n "$line" ]; do
    line="${line%$'\r'}"
    case "$line" in ''|'#'*) continue ;; esac
    key="${line%%=*}"; val="${line#*=}"
    key="${key#export }"
    [[ "$key" =~ ^[A-Za-z_][A-Za-z0-9_]*$ ]] || continue
    if [[ "$val" == \"*\" ]]; then val="${val:1:${#val}-2}"
    elif [[ "$val" == \'*\' ]]; then val="${val:1:${#val}-2}"; fi
    [ -n "${!key+x}" ] || export "$key=$val"
  done < "$f"
}

need() {  # need VAR... : abort if any is empty
  local v miss=0
  for v in "$@"; do
    if [ -z "${!v:-}" ]; then echo "error: $v is not set (see .env.example)" >&2; miss=1; fi
  done
  [ "$miss" = 0 ] || exit 1
}
