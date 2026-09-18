"""Container-only build and audit stages. Never run this on a host/node."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import re
import shutil
import subprocess
import sys
import tempfile

from artifact_set import INPUTS, check_hashes, rpm_filename, write_hashes
from cpio_audit import read_payload
from elf_audit import inspect_elf
from rpm_audit import audit_rpm, command, PACKAGE
from source_policy import read_lock, audit_archive, verify_sha256, verify_signature, sha256
from profile_policy import (RENDER_FILES, validate_render, extract_feature_fixture, validate_feature_fixture,
                            extract_default_abi, validate_default_abi)

OUT = Path("/out")
KEY = Path("/run/secrets/apparmor_trusted_keyring")


def check_hce(release, machine, arch):
    fields = dict(line.split("=", 1) for line in release.splitlines() if "=" in line and not line.startswith("#"))
    if (fields.get("ID", "").strip('"') != "hce" or fields.get("VERSION_ID", "").strip('"') != "2.0"
            or machine != {"amd64": "x86_64", "arm64": "aarch64"}.get(arch)):
        raise ValueError("execution environment is not matching-architecture HCE 2.0 userspace")
    return {"os": "HCE 2.0", "uname_machine": machine,
            "execution_mode": "unknown: BuildKit may use native or emulated execution",
            "native_live_hce_verified": False}


def audit_dynamic(output):
    needed = re.findall(r"\(NEEDED\).*\[([^\]]+)\]", output)
    if re.search(r"\((?:RPATH|RUNPATH)\)", output) or any("apparmor" in name or "/" in name for name in needed):
        raise ValueError("parser ELF has an unexpected private dependency or runtime search path")
    if not needed or "libc.so.6" not in needed:
        raise ValueError("parser ELF has no expected C runtime dependency")
    return needed


def run(args, *, cwd=None, log=None, timeout=1800):
    try:
        result = subprocess.run(args, cwd=cwd, stdout=log if log else subprocess.PIPE,
                                stderr=subprocess.STDOUT, timeout=timeout)
    except (OSError, subprocess.TimeoutExpired):
        raise ValueError("isolated build/audit command unavailable or timed out") from None
    if result.returncode:
        raise ValueError("isolated build/audit command failed")
    return result.stdout.decode("utf-8", errors="replace") if not log else ""


def write_json(path, data):
    path.write_text(json.dumps(data, indent=2, sort_keys=True) + "\n")


def trust_source(root):
    lock = read_lock(root / "source.lock")
    archive = root / "apparmor-v4.1.7.tar.gz"
    verify_sha256(archive, lock["archive_sha256"])
    identity = verify_signature(archive, root / "apparmor-v4.1.7.tar.gz.asc", KEY,
                                lock["trusted_public_key_fingerprint"], lock["trusted_signing_key_fingerprint"])
    audit_archive(archive)
    return lock, identity


def build(arch, image, environment):
    lock = read_lock("/work/source.lock")
    OUT.mkdir()
    source = OUT / "source"
    source.mkdir()
    shutil.copyfile("/work/source.lock", source / "source.lock")
    for key, filename in [("release_archive_url", "apparmor-v4.1.7.tar.gz"), ("signature_url", "apparmor-v4.1.7.tar.gz.asc")]:
        run(["curl", "--fail", "--silent", "--show-error", "--location", "--max-redirs", "5",
             "--proto", "=https", "--proto-redir", "=https", "--tlsv1.2", "--connect-timeout", "15",
             "--max-time", "180", "--max-filesize", "67108864", "--retry", "2", "--retry-max-time", "360",
             lock[key], "--output", str(source / filename)], timeout=400)
    lock, identity = trust_source(source)
    # Public-only key data is retained intentionally as source provenance.
    shutil.copyfile(KEY, source / "trusted-public.gpg")
    for name in INPUTS:
        target = OUT / "inputs" / name
        target.parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(Path("/work") / name, target)
    for name in RENDER_FILES:
        target = OUT / name
        target.parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(Path("/work") / name, target)
    profile = validate_render(OUT)
    extract_feature_fixture(source / "apparmor-v4.1.7.tar.gz", OUT)
    feature_fixture = validate_feature_fixture(source / "apparmor-v4.1.7.tar.gz", OUT)
    extract_default_abi(source / "apparmor-v4.1.7.tar.gz", OUT)
    policy_abi = validate_default_abi(source / "apparmor-v4.1.7.tar.gz", OUT)
    top = Path("/tmp/apparmor-rpmbuild")
    top.mkdir()
    for folder in ("SOURCES", "SPECS", "BUILD", "BUILDROOT", "RPMS", "SRPMS"):
        (top / folder).mkdir()
    shutil.copyfile(source / "apparmor-v4.1.7.tar.gz", top / "SOURCES/apparmor-v4.1.7.tar.gz")
    shutil.copyfile("/work/rpm/sandbox-apparmor-parser.spec", top / "SPECS/sandbox-apparmor-parser.spec")
    with (OUT / "build-tests.log").open("wb") as log:
        run(["rpmbuild", "-ba", "--define", "_topdir " + str(top), str(top / "SPECS/sandbox-apparmor-parser.spec")], log=log, timeout=5400)
    built = list((top / "RPMS").rglob("*.rpm")) + list((top / "SRPMS").glob("*.rpm"))
    if {path.name for path in built} != {rpm_filename(arch), PACKAGE + "-4.1.7-1.src.rpm"} or len(built) != 2:
        raise ValueError("rpmbuild produced an unexpected package set")
    for path in built:
        shutil.copyfile(path, OUT / path.name)
    (OUT / "build-packages.txt").write_text(command(["rpm", "-qa", "--qf", "%{NAME} %{EPOCHNUM}:%{VERSION}-%{RELEASE} %{ARCH}\n"]))
    tools = {name: run([name, "--version"]).splitlines()[0] for name in ("gcc", "g++", "make", "rpmbuild", "autoconf", "automake", "bison", "flex", "gpg", "python3")}
    write_json(OUT / "provenance.json", {"arch": arch, "builder_image": image, "source": lock,
               "archive_signature": identity, "source_revision_trust": "unverified metadata; archive trust is SHA256 plus detached signature",
               "tool_versions": tools, "environment": environment, "signature_status": "unsigned-testing-only",
               "workspace_profile": profile, "compile_features": feature_fixture, "policy_features": policy_abi})


def audit(arch, image, environment, repeat=False):
    if repeat:
        check_hashes(OUT, arch)
    lock, identity = trust_source(OUT / "source")
    profile = validate_render(OUT)
    feature_fixture = validate_feature_fixture(OUT / "source/apparmor-v4.1.7.tar.gz", OUT)
    policy_abi = validate_default_abi(OUT / "source/apparmor-v4.1.7.tar.gz", OUT)
    provenance = json.loads((OUT / "provenance.json").read_text())
    if provenance.get("builder_image") != image or provenance.get("arch") != arch or provenance.get("source") != lock:
        raise ValueError("artifact provenance does not match current verification environment")
    if (provenance.get("workspace_profile") != profile or provenance.get("compile_features") != feature_fixture
            or provenance.get("policy_features") != policy_abi):
        raise ValueError("artifact profile/features provenance does not match audited source")
    binary = OUT / rpm_filename(arch)
    srpm = OUT / (PACKAGE + "-4.1.7-1.src.rpm")
    packages = []
    for path, is_source in [(binary, False), (srpm, True)]:
        command(["rpm", "--checksig", str(path)])
        metadata = audit_rpm(path, arch, source=is_source)
        payload = read_payload(path, metadata["payload"])
        packages.append(metadata)
        if is_source:
            if (payload["apparmor-v4.1.7.tar.gz"] != (OUT / "source/apparmor-v4.1.7.tar.gz").read_bytes()
                    or payload["sandbox-apparmor-parser.spec"] != (OUT / "inputs/rpm/sandbox-apparmor-parser.spec").read_bytes()):
                raise ValueError("source RPM contents differ from retained build inputs")
        else:
            parser_bytes = payload["usr/sbin/apparmor_parser"]
    parser = OUT / "apparmor_parser"
    if repeat and parser.read_bytes() != parser_bytes:
        raise ValueError("exported parser differs from the RPM payload")
    parser.write_bytes(parser_bytes)
    parser.chmod(0o755)
    elf = inspect_elf(parser, arch)
    dynamic = command(["readelf", "--wide", "--dynamic", str(parser)])
    needed = audit_dynamic(dynamic)
    abi = dynamic + command(["readelf", "--wide", "--dyn-syms", "--version-info", str(parser)])
    (OUT / "abi.txt").write_text(abi)
    # Resolve requirements against actual HCE packages before the artifact's
    # own rpm --test / install transaction. No dependency bypass flags.
    requirements = command(["rpm", "-qp", "--requires", str(binary)]).splitlines()
    external = [item for item in requirements if not item.startswith("rpmlib(")]
    if external:
        run(["dnf", "install", "-y", "--setopt=install_weak_deps=False"] + external)
    run(["rpm", "--test", "-i", str(binary)])
    run(["rpm", "-i", str(binary)])
    run(["rpm", "-V", PACKAGE])
    installed = Path("/usr/sbin/apparmor_parser")
    if sha256(installed) != hashlib.sha256(parser_bytes).hexdigest():
        raise ValueError("installed parser differs from audited RPM payload")
    layout = Path("/sbin/apparmor_parser")
    if layout.resolve() != installed or not installed.is_file():
        raise ValueError("HCE /sbin parser path does not resolve to the installed ELF")
    interpreter = Path(elf["interpreter"])
    if not interpreter.is_file():
        raise ValueError("ELF interpreter is absent in matching HCE userspace")
    from parser_checks import runtime_checks
    checks = runtime_checks(installed, OUT)
    closure = []
    for requirement in external:
        closure.append(requirement + "\n" + command(["rpm", "-q", "--whatprovides", requirement]))
    (OUT / "dependency-closure.txt").write_text("\n".join(closure))
    (OUT / "runtime-packages.txt").write_text(command(["rpm", "-qa", "--qf", "%{NAME} %{EPOCHNUM}:%{VERSION}-%{RELEASE} %{ARCH}\n"]))
    write_json(OUT / "audit.json", {"packages": packages, "elf": elf, "needed": needed, "source_signature": identity,
               "dependency_check": "matching HCE provider resolution, rpm --test -i, rpm -i, rpm -V",
               "parser_checks": checks, "environment": environment, "native_live_hce_verified": False,
               "signature_status": "unsigned-testing-only", "production_signature_gate": "required; not performed"})
    write_hashes(OUT, arch)


def main():
    args = argparse.ArgumentParser()
    args.add_argument("stage", choices=["build", "audit", "repeat"])
    args.add_argument("--arch", choices=["amd64", "arm64"], required=True)
    args.add_argument("--builder-image", required=True)
    config = args.parse_args()
    try:
        environment = check_hce(Path("/etc/os-release").read_text(), platform.machine(), config.arch)
        if config.stage == "build":
            build(config.arch, config.builder_image, environment)
        else:
            audit(config.arch, config.builder_image, environment, repeat=config.stage == "repeat")
    except (ValueError, OSError) as error:
        print("error: " + (str(error) if type(error) is ValueError else "HCE artifact stage failed"), file=sys.stderr)
        return 2
    return 0


if __name__ == "__main__":
    sys.exit(main())
