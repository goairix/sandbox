"""Exact export inventory and complete SHA256 coverage, excluding the hash file."""
from pathlib import Path
import re
from source_policy import sha256
from profile_policy import CHART_INPUTS, PROFILE_FILES

HELPERS = {"source_policy.py", "elf_audit.py", "rpm_audit.py", "artifact_set.py", "workflow.py", "hce_stage.py", "cpio_audit.py", "parser_checks.py", "profile_policy.py"}
INPUTS = {"build.sh", "verify.sh", "Containerfile", "Verify.Containerfile", "check-sbin-layout.sh",
          "rpm/sandbox-apparmor-parser.spec"} | {"lib/" + name for name in HELPERS} | CHART_INPUTS


def rpm_filename(arch):
    return "sandbox-apparmor-parser-4.1.7-1." + {"amd64": "x86_64", "arm64": "aarch64"}[arch] + ".rpm"


def expected_files(arch):
    return {rpm_filename(arch), "sandbox-apparmor-parser-4.1.7-1.src.rpm", "apparmor_parser",
            "source/apparmor-v4.1.7.tar.gz", "source/apparmor-v4.1.7.tar.gz.asc", "source/trusted-public.gpg",
            "source/source.lock", "provenance.json", "audit.json", "build-packages.txt", "runtime-packages.txt",
            "build-tests.log", "abi.txt", "dependency-closure.txt", "parser-version.txt", "parser-version.stderr", "SHA256SUMS"} | {"inputs/" + name for name in INPUTS} | PROFILE_FILES


def check_file_set(root, arch, include_hashes=True):
    root = Path(root)
    if not root.is_dir() or root.is_symlink():
        raise ValueError("artifact root must be a real directory")
    expected = expected_files(arch) - (set() if include_hashes else {"SHA256SUMS"})
    actual = set()
    allowed_dirs = {str(parent) for name in expected for parent in Path(name).parents if str(parent) != "."}
    for path in root.rglob("*"):
        relative = path.relative_to(root).as_posix()
        if path.is_symlink() or not (path.is_file() or path.is_dir()):
            raise ValueError("artifact inventory contains a link or special file")
        if path.is_dir():
            if relative not in allowed_dirs:
                raise ValueError("artifact inventory contains an unexpected directory")
        else:
            actual.add(relative)
    if actual != expected:
        raise ValueError("artifact inventory has unexpected or missing files")
    return expected


def write_hashes(root, arch):
    root = Path(root)
    if (root / "SHA256SUMS").exists():
        check_file_set(root, arch)
    else:
        check_file_set(root, arch, include_hashes=False)
    content = "".join(f"{sha256(root / name)}  {name}\n" for name in sorted(expected_files(arch) - {"SHA256SUMS"}))
    (root / "SHA256SUMS").write_text(content, encoding="ascii")


def check_hashes(root, arch):
    root = Path(root)
    expected = check_file_set(root, arch) - {"SHA256SUMS"}
    seen = set()
    for line in (root / "SHA256SUMS").read_text(encoding="ascii").splitlines():
        match = re.fullmatch(r"([0-9a-f]{64})  ([A-Za-z0-9_./-]+)", line)
        if not match or match[2] not in expected or match[2] in seen:
            raise ValueError("artifact hash manifest has malformed or duplicate entries")
        seen.add(match[2])
        if sha256(root / match[2]) != match[1]:
            raise ValueError("artifact checksum mismatch")
    if seen != expected:
        raise ValueError("artifact hash manifest does not cover every required file")
