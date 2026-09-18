"""Host orchestration; no Docker call occurs before all input trust gates."""
import argparse
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tempfile

from artifact_set import INPUTS, check_hashes
from elf_audit import inspect_elf
from source_policy import read_lock, check_keyring, verify_signature, verify_sha256, audit_archive

ROOT = Path(__file__).resolve().parents[1]


class QuietParser(argparse.ArgumentParser):
    def error(self, message):
        raise ValueError("invalid or missing CLI arguments; use --help")


def parse(mode, argv):
    parser = QuietParser(description="AppArmor HCE local build/export and audit")
    parser.add_argument("--arch", required=True, choices=["amd64", "arm64"])
    parser.add_argument("--builder-image", required=True)
    parser.add_argument("--output-dir", required=True)
    parser.add_argument("--source-lock")
    parser.add_argument("--trusted-keyring", required=True)
    if mode == "verify":
        parser.add_argument("--structural-only", action="store_true")
    config = parser.parse_args(argv)
    if not re.fullmatch(r"[a-zA-Z0-9][a-zA-Z0-9._:/-]*@sha256:[0-9a-f]{64}", config.builder_image):
        raise ValueError("--builder-image must be a digest-pinned image reference")
    output = Path(config.output_dir)
    if not output.is_absolute() or any(char in config.output_dir for char in "\n\r\t,"):
        raise ValueError("--output-dir must be an absolute path without control characters or commas")
    if output.is_symlink() or (output.exists() and not output.is_dir()):
        raise ValueError("--output-dir must be a real directory")
    if mode == "build" and output.exists() and any(output.iterdir()):
        raise ValueError("--output-dir must be absent or empty; existing artifacts are never overwritten")
    if not output.parent.is_dir():
        raise ValueError("--output-dir parent must already exist")
    config.output = output
    config.lock_path = Path(config.source_lock) if config.source_lock else (ROOT / "source.lock" if mode == "build" else output / "source/source.lock")
    config.lock = read_lock(config.lock_path)
    config.keyring = Path(config.trusted_keyring).resolve()
    check_keyring(config.keyring, config.lock["trusted_public_key_fingerprint"], config.lock["trusted_signing_key_fingerprint"])
    return config


def copy_inputs(context):
    for name in sorted(INPUTS):
        source = ROOT / name
        if source.is_symlink() or not source.is_file():
            raise ValueError("build input is missing or is a symbolic link")
        target = context / name
        target.parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(source, target)


def docker_build(config, context, output, verify=False):
    # Use the existing buildx builder; no global builder/emulator changes.
    command = ["docker", "buildx", "build", "--platform", "linux/" + config.arch,
               "--file", str(context / ("Verify.Containerfile" if verify else "Containerfile")),
               "--build-arg", "HCE_BUILDER_IMAGE=" + config.builder_image,
               "--secret", "id=apparmor_trusted_keyring,src=" + str(config.keyring),
               "--output", "type=local,dest=" + str(output), "--progress", "plain", str(context)]
    with (context.parent / "docker-private.log").open("wb") as log:
        try:
            result = subprocess.run(command, stdout=log, stderr=subprocess.STDOUT, timeout=7200)
        except (OSError, subprocess.TimeoutExpired):
            raise ValueError("Docker buildx unavailable or build timed out") from None
    if result.returncode:
        raise ValueError("isolated HCE build/audit failed; artifacts were not published")


def structural_verify(root, config):
    check_hashes(root, config.arch)
    lock = read_lock(root / "source/source.lock")
    if lock != config.lock:
        raise ValueError("artifact source.lock differs from the supplied lock")
    archive = root / "source/apparmor-v4.1.7.tar.gz"
    verify_sha256(archive, lock["archive_sha256"])
    verify_signature(archive, root / "source/apparmor-v4.1.7.tar.gz.asc", config.keyring,
                     lock["trusted_public_key_fingerprint"], lock["trusted_signing_key_fingerprint"])
    check_keyring(root / "source/trusted-public.gpg", lock["trusted_public_key_fingerprint"], lock["trusted_signing_key_fingerprint"])
    audit_archive(archive)
    inspect_elf(root / "apparmor_parser", config.arch)
    provenance = json.loads((root / "provenance.json").read_text())
    audit = json.loads((root / "audit.json").read_text())
    if (provenance.get("builder_image") != config.builder_image or provenance.get("arch") != config.arch
            or audit.get("signature_status") != "unsigned-testing-only"
            or audit.get("native_live_hce_verified") is not False):
        raise ValueError("artifact provenance/audit does not match requested testing contract")


def main(mode, argv=None):
    try:
        config = parse(mode, argv)
        if mode == "verify":
            structural_verify(config.output, config)
            if config.structural_only:
                print("Passed: source trust, inventory, hashes, ELF structure. HCE execution was NOT repeated.")
                return 0
        with tempfile.TemporaryDirectory(prefix=".apparmor-hce-", dir=str(config.output.parent)) as temporary:
            task = Path(temporary)
            context, staged = task / "context", task / "export"
            context.mkdir()
            copy_inputs(context)
            shutil.copyfile(config.lock_path, context / "source.lock")
            if read_lock(context / "source.lock") != config.lock:
                raise ValueError("source.lock changed during input staging")
            public = task / "public-keyring"
            shutil.copyfile(config.keyring, public)
            config.keyring = public
            check_keyring(public, config.lock["trusted_public_key_fingerprint"], config.lock["trusted_signing_key_fingerprint"])
            if mode == "verify":
                shutil.copytree(config.output, context / "artifacts")
                structural_verify(context / "artifacts", config)
            docker_build(config, context, staged, verify=mode == "verify")
            structural_verify(staged, config)
            if mode == "build":
                # rmdir refuses nonempty user directories even when user data
                # was created after initial validation and during the build.
                if config.output.exists():
                    config.output.rmdir()
                os.rename(staged, config.output)
        print("Passed: isolated HCE userspace artifact audit; unsigned testing artifacts only. Native live HCE is unverified.")
        return 0
    except (ValueError, OSError) as error:
        message = str(error) if type(error) is ValueError else "input/output validation failed"
        print("error: " + message, file=sys.stderr)
        return 2


if __name__ == "__main__":
    sys.exit(main(sys.argv[1], sys.argv[2:]))
