#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
ROOT_DIR=$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)
CHECKER="$ROOT_DIR/check-node-prerequisites.sh"

die() { printf 'test failure: %s\n' "$1" >&2; exit 1; }

make_fixture() {
  local dir=$1 disable=$2
  mkdir -p "$dir/root/sys/module/apparmor/parameters" \
    "$dir/root/sys/kernel/security" "$dir/root/etc/containerd" \
    "$dir/root/usr/sbin" "$dir/bin"
  printf 'Y\n' >"$dir/root/sys/module/apparmor/parameters/enabled"
  : >"$dir/root/etc/containerd/config.toml"
  ln -s usr/sbin "$dir/root/sbin"
  cat >"$dir/root/usr/sbin/apparmor_parser" <<'EOF'
#!/usr/bin/env bash
if [[ ${1:-} == --version ]]; then
  printf 'AppArmor parser version 4.1.7\n'
  exit 0
fi
exit 0
EOF
  chmod 0755 "$dir/root/usr/sbin/apparmor_parser"
  cat >"$dir/bin/mountpoint" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF
  cat >"$dir/bin/systemctl" <<'EOF'
#!/usr/bin/env bash
[[ ${1:-} == is-active && ${2:-} == --quiet && ${3:-} == containerd ]]
EOF
  cat >"$dir/bin/crictl" <<EOF
#!/usr/bin/env bash
  if [[ \${1:-} == info ]]; then
  printf '{"config":{"disableApparmor":$disable}}\n'
  exit 0
fi
exit 1
EOF
  chmod 0755 "$dir/bin/mountpoint" "$dir/bin/systemctl" "$dir/bin/crictl"
}

run_case() {
  local disable=$1 expected=$2
  local dir
  dir=$(mktemp -d)
  trap 'rm -rf "$dir"' RETURN
  make_fixture "$dir" "$disable"
  set +e
  SANDBOX_ARCH_OVERRIDE=aarch64 PATH="$dir/bin:$PATH" "$CHECKER" --root "$dir/root" >"$dir/output" 2>&1
  local status=$?
  set -e
  [[ $status -eq $expected ]] || { cat "$dir/output" >&2; die "expected exit $expected, got $status"; }
  if [[ $expected -eq 0 ]]; then
    grep -q 'RESULT: PASS' "$dir/output" || { cat "$dir/output" >&2; die 'missing pass result'; }
  else
    grep -q 'disableApparmor=true' "$dir/output" || { cat "$dir/output" >&2; die 'missing CRI failure'; }
    grep -q 'RESULT: FAIL' "$dir/output" || { cat "$dir/output" >&2; die 'missing fail result'; }
  fi
}

run_case false 0
run_case true 1
printf 'node prerequisite script tests: ok\n'
