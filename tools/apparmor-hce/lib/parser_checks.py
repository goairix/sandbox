"""Userspace-only runtime checks; workspace policy/feature checks are Task 4."""
from pathlib import Path
import re
import subprocess
import tempfile


def compile_profile(parser, profile, output, features=None):
    """Reusable Task 4 hook: compile with kernel loading/cache access disabled."""
    args = [str(parser), "-Q", "-K", "--config-file=/dev/null", "-o", str(output)]
    if features is not None:
        args += ["--policy-features", str(features)]
    args.append(str(profile))
    result = subprocess.run(args, capture_output=True, timeout=120)
    if result.returncode or not Path(output).is_file() or not Path(output).stat().st_size:
        raise ValueError("installed parser failed userspace-only profile compilation")


def runtime_checks(parser, output):
    version = subprocess.run([str(parser), "--version"], capture_output=True, timeout=30)
    if version.returncode or not re.search(rb"\bversion 4\.1\.7(?:\s|$)", version.stdout):
        raise ValueError("installed parser version check failed")
    (output / "parser-version.txt").write_bytes(version.stdout)
    with tempfile.TemporaryDirectory(prefix="apparmor-compile-") as temporary:
        root = Path(temporary)
        profile = root / "smoke.profile"
        profile.write_text("profile sandbox_hce_build_smoke {\n  / r,\n}\n")
        compile_profile(parser, profile, root / "smoke.bin")
    return {"version": "4.1.7", "compile_smoke": "passed: -Q -K; no kernel load or cache",
            "workspace_profile": "pending Task 4", "host_kernel_policy_load": "not performed"}
