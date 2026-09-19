"""Full immutable Chart policy compilation; no host/kernel policy operations."""
import hashlib
import json
from pathlib import Path
import re
import subprocess

from source_policy import sha256
from profile_policy import validate_render, validate_feature_fixture, validate_default_abi

COMPILE_FLAGS = ["-Q", "-K", "--config-file=/dev/null", "--Werror", "--warn=all"]


def capture(args, timeout):
    try:
        result = subprocess.run(args, capture_output=True, timeout=timeout)
        return result.returncode, result.stdout, result.stderr, None
    except subprocess.TimeoutExpired as error:
        return None, error.stdout or b"", error.stderr or b"", "timeout"
    except OSError:
        return None, b"", b"", "execution-error"


def result_record(status, stdout, stderr, failure=None):
    return {"exit_status": status, "stdout_sha256": hashlib.sha256(stdout).hexdigest(),
            "stderr_sha256": hashlib.sha256(stderr).hexdigest(), "execution_failure": failure}


def compile_profile(parser, profile, output, features=None, policy_features=None):
    """Reusable Task 4 hook: strict diagnostics; never override the policy ABI."""
    output = Path(output)
    if output.is_symlink() or (output.exists() and not output.is_file()):
        raise ValueError("compiled profile output must be a regular file")
    # Reverification must produce fresh bytes, not accept a previous binary.
    if output.exists():
        output.unlink()
    args = [str(parser)] + COMPILE_FLAGS + ["-o", str(output)]
    if policy_features is not None:
        args += ["--policy-features", str(policy_features)]
    if features is not None:
        args += ["--kernel-features", str(features)]
    args.append(str(profile))
    status, stdout, stderr, failure = capture(args, 120)
    report = result_record(status, stdout, stderr, failure)
    report.update({"profile_sha256": sha256(profile), "features_sha256": sha256(features) if features else None,
                   "policy_features_sha256": sha256(policy_features) if policy_features else None,
                   "flags": COMPILE_FLAGS, "features_option": "--kernel-features" if features else None,
                   "policy_features_option": "--policy-features" if policy_features else None,
                   "policy_abi_overridden": False, "kernel_load": False})
    # --Werror covers compiler warnings; reject every diagnostic stream too,
    # including warnings/errors that older parser paths emit with status 0.
    report["passed"] = bool(status == 0 and not stdout and not stderr and output.is_file() and output.stat().st_size)
    report["compiled_sha256"] = sha256(output) if output.is_file() else None
    (output.parent / "compile.stdout").write_bytes(stdout)
    (output.parent / "compile.stderr").write_bytes(stderr)
    (output.parent / "compile.json").write_text(json.dumps(report, indent=2, sort_keys=True) + "\n")
    if not report["passed"]:
        raise ValueError("installed parser compilation failed or emitted diagnostics; compile audit recorded")
    return report


def validate_compilation(root):
    target = Path(root) / "profile"
    report = json.loads((target / "compile.json").read_text())
    files = {"profile_sha256": "workspace.profile", "features_sha256": "kernel-features.fixture",
             "policy_features_sha256": "policy-features.default",
             "compiled_sha256": "workspace.bin", "stdout_sha256": "compile.stdout", "stderr_sha256": "compile.stderr"}
    if (report.get("passed") is not True or report.get("exit_status") != 0 or report.get("execution_failure") is not None
            or report.get("flags") != COMPILE_FLAGS or report.get("features_option") != "--kernel-features"
            or report.get("policy_features_option") != "--policy-features"
            or report.get("policy_abi_overridden") is not False or report.get("kernel_load") is not False
            or any(report.get(key) != sha256(target / path) for key, path in files.items())
            or (target / "compile.stdout").stat().st_size or (target / "compile.stderr").stat().st_size
            or not (target / "workspace.bin").stat().st_size):
        raise ValueError("profile compile report does not match strict flags, status, or artifact hashes")
    return report


def runtime_checks(parser, output):
    # Keep the version probe independent of a host-installed parser.conf. HCE
    # nodes intentionally only receive this parser RPM, so an implicit global
    # config would turn a valid binary check into a diagnostic failure.
    status, stdout, stderr, failure = capture([str(parser), "--config-file=/dev/null", "--version"], 30)
    (output / "parser-version.txt").write_bytes(stdout)
    (output / "parser-version.stderr").write_bytes(stderr)
    version = result_record(status, stdout, stderr, failure)
    if status != 0 or stderr or not re.search(rb"\bversion 4\.1\.7(?:\s|$)", stdout):
        raise ValueError("installed parser version check failed")
    render = validate_render(output)
    fixture = validate_feature_fixture(output / "source/apparmor-v4.1.7.tar.gz", output)
    policy_abi = validate_default_abi(output / "source/apparmor-v4.1.7.tar.gz", output)
    profile = output / "profile/workspace.profile"
    compiled = compile_profile(parser, profile, output / "profile/workspace.bin",
                               features=output / "profile/kernel-features.fixture",
                               policy_features=output / "profile/policy-features.default")
    return {"version": "4.1.7", "version_command": version, "workspace_profile": compiled,
            "profile_render": render, "compile_features": fixture, "policy_features": policy_abi,
            "kernel_compatibility": "not verified; upstream userspace test fixture only",
            "host_kernel_policy_load": "not performed"}
