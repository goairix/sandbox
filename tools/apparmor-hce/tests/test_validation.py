"""Local validator tests only: synthetic ELF is not executable/HCE evidence."""
import importlib.util
import hashlib
import io
from pathlib import Path
import shutil
import struct
import subprocess
import sys
import tarfile
import tempfile
import unittest
from contextlib import redirect_stderr
from types import SimpleNamespace
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "lib"))


def module(name):
    spec = importlib.util.find_spec(name)
    if spec is None:
        raise AssertionError(f"missing validator module: {name}")
    return __import__(name)


def lock_text(fingerprint="A" * 40):
    return "\n".join([
        "version=4.1.7",
        "release_archive_url=https://gitlab.com/apparmor/apparmor/-/archive/v4.1.7/apparmor-v4.1.7.tar.gz",
        "signature_url=https://gitlab.com/api/v4/projects/4484878/packages/generic/signatures/4.1.7/apparmor-v4.1.7.tar.gz.asc",
        "archive_sha256=" + "b" * 64,
        "trusted_public_key_fingerprint=" + fingerprint,
        "trusted_signing_key_fingerprint=" + fingerprint,
        "source_revision=9676b7aa934408748de2aaebad8680e69d7c3912",
        "source_revision_verification=unverified",
    ]) + "\n"


def elf(machine=62):
    # ET_DYN with one load segment and the architecture's normal interpreter.
    interp = {62: b"/lib64/ld-linux-x86-64.so.2\0", 183: b"/lib/ld-linux-aarch64.so.1\0"}[machine]
    size = 64 + 2 * 56 + len(interp)
    header = b"\x7fELF\x02\x01\x01" + bytes(9)
    header += struct.pack("<HHIQQQIHHHHHH", 3, machine, 1, 0x400000, 64, 0, 0, 64, 56, 2, 0, 0, 0)
    load = struct.pack("<IIQQQQQQ", 1, 5, 0, 0x400000, 0, size, size, 4096)
    intr = struct.pack("<IIQQQQQQ", 3, 4, 176, 0, 0, len(interp), len(interp), 1)
    return header + load + intr + interp


def cpio_entry(name, data=b"", mode=0o100644):
    name = name.encode() + b"\0"
    values = [1, mode, 0, 0, 1, 0, len(data), 0, 0, 0, 0, len(name), 0]
    raw = b"070701" + b"".join(f"{value:08x}".encode() for value in values) + name
    raw += bytes((-len(raw)) % 4) + data
    return raw + bytes((-len(data)) % 4)


class Validators(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.root = Path(self.tmp.name)
        self.addCleanup(self.tmp.cleanup)

    def test_lock_positive_and_bad_fields(self):
        policy = module("source_policy")
        path = self.root / "source.lock"
        path.write_text(lock_text())
        self.assertEqual(policy.read_lock(path)["version"], "4.1.7")
        for bad in [lock_text() + "version=4.1.7\n", lock_text().replace("b" * 64, "bad"),
                    lock_text().replace("https://gitlab.com/", "http://gitlab.com/"),
                    lock_text().replace("source_revision_verification=unverified", "source_revision_verification=verified")]:
            with self.subTest(bad=bad[:40]):
                path.write_text(bad)
                with self.assertRaises(ValueError):
                    policy.read_lock(path)

    def test_archive_rejects_escape_duplicate_and_devices(self):
        policy = module("source_policy")
        for member, kind in [("apparmor-v4.1.7/README", "valid"), ("../escape", "file"),
                             ("apparmor-v4.1.7/../../escape", "file"),
                             ("apparmor-v4.1.7/link", "link"),
                             ("apparmor-v4.1.7/dev", "device"),
                             ("apparmor-v4.1.7/a", "duplicate")]:
            path = self.root / "source.tar.gz"
            with tarfile.open(path, "w:gz") as archive:
                item = tarfile.TarInfo(member)
                if kind == "link":
                    item.type, item.linkname = tarfile.SYMTYPE, "../../escape"
                elif kind == "device":
                    item.type = tarfile.CHRTYPE
                archive.addfile(item, io.BytesIO())
                if kind == "duplicate":
                    archive.addfile(item, io.BytesIO())
            if kind == "valid":
                self.assertEqual(policy.audit_archive(path), "apparmor-v4.1.7")
            else:
                with self.assertRaises(ValueError):
                    policy.audit_archive(path)

        # Each target looks lexically contained, but following a then '..'
        # would escape the top directory. Never traverse archive link chains.
        with tarfile.open(path, "w:gz") as archive:
            for name, target in [("apparmor-v4.1.7/dir/a", ".."), ("apparmor-v4.1.7/dir/b", "a/../../escape")]:
                item = tarfile.TarInfo(name)
                item.type, item.linkname = tarfile.SYMTYPE, target
                archive.addfile(item)
        with self.assertRaises(ValueError):
            policy.audit_archive(path)

    def test_elf_real_structure_and_wrong_arch(self):
        audit = module("elf_audit")
        path = self.root / "parser"
        path.write_bytes(elf())
        self.assertEqual(audit.inspect_elf(path, "amd64")["machine"], 62)
        with self.assertRaises(ValueError):
            audit.inspect_elf(path, "arm64")
        for contents in [elf()[:64], elf().replace(b"/lib64/", b"/tmp/x/"),
                         elf()[:16] + struct.pack("<H", 1) + elf()[18:]]:
            path.write_bytes(contents)
            with self.assertRaises(ValueError):
                audit.inspect_elf(path, "amd64")

    def test_layout_checker_uses_complete_elf_structure(self):
        (self.root / "usr/sbin").mkdir(parents=True)
        (self.root / "sbin").symlink_to("usr/sbin")
        parser = self.root / "usr/sbin/apparmor_parser"
        parser.write_bytes(elf())
        parser.chmod(0o755)
        args = [str(ROOT / "check-sbin-layout.sh"), str(self.root), "/usr/sbin/apparmor_parser", "x86_64"]
        self.assertEqual(subprocess.run(args, capture_output=True).returncode, 0)
        parser.write_bytes(elf()[:64])
        self.assertNotEqual(subprocess.run(args, capture_output=True).returncode, 0)

    def test_rpm_metadata_policy(self):
        audit = module("rpm_audit")
        payload = [dict(path="/usr/sbin/apparmor_parser", mode=0o100755, user="root", group="root", link="", caps=""),
                   dict(path="/usr/share/licenses/sandbox-apparmor-parser", mode=0o40755, user="root", group="root", link="", caps=""),
                   dict(path="/usr/share/licenses/sandbox-apparmor-parser/COPYING.GPL", mode=0o100644, user="root", group="root", link="", caps="")]
        audit.validate_payload(payload)
        for patch in [dict(path="/etc/evil"), dict(mode=0o104755), dict(caps="cap_sys_admin=ep"),
                      dict(user="nobody"), dict(mode=0o120777, link="../../etc/passwd")]:
            with self.subTest(patch=patch), self.assertRaises(ValueError):
                audit.validate_payload([{**payload[0], **patch}] + payload[1:])
        with self.assertRaises(ValueError):
            audit.validate_payload(payload + [payload[0]])
        for tags in [{"PREIN": "echo bad"}, {"FILETRIGGERSCRIPTS": "echo bad"},
                     {"TRANSFILETRIGGERSCRIPTPROG": "/bin/sh"}, {"VERIFYSCRIPT": "echo bad"},
                     {"FILETRIGGERNAME": "/etc"}, {"TRIGGERNAME": "other-package"}]:
            with self.assertRaises(ValueError):
                audit.reject_scripts(tags)
        audit.reject_scripts({"POSTIN": "(none)"})

    def test_actual_cpio_members_traversal_duplicate_and_mismatch(self):
        audit = module("cpio_audit")
        trailer = cpio_entry("TRAILER!!!")
        items = audit.parse_newc(cpio_entry("./usr/sbin/apparmor_parser", b"elf", 0o100755) + trailer)
        self.assertEqual(items[0]["data"], b"elf")
        for data in [cpio_entry("../../escape") + trailer,
                     cpio_entry("./a") + cpio_entry("./a") + trailer,
                     cpio_entry("./a", b"/etc", 0o120777) + trailer,
                     cpio_entry("./a")[:-1]]:
            with self.assertRaises(ValueError):
                audit.parse_newc(data)

    def test_hce_identity_and_abi_gates_do_not_claim_native(self):
        stage = module("hce_stage")
        self.assertFalse(stage.check_hce('ID="hce"\nVERSION_ID="2.0"\n', "x86_64", "amd64")["native_live_hce_verified"])
        for release, machine, arch in [("ID=ubuntu\nVERSION_ID=2.0\n", "x86_64", "amd64"),
                                        ("ID=hce\nVERSION_ID=2.0\n", "aarch64", "amd64")]:
            with self.assertRaises(ValueError):
                stage.check_hce(release, machine, arch)
        good = " 0x0000000000000001 (NEEDED) Shared library: [libc.so.6]\n"
        self.assertEqual(stage.audit_dynamic(good), ["libc.so.6"])
        for bad in [good + " (RPATH) Library rpath: [/tmp]\n", good + " (NEEDED) Shared library: [libapparmor.so.1]\n"]:
            with self.assertRaises(ValueError):
                stage.audit_dynamic(bad)

    def test_hce_build_vendors_the_only_missing_autoconf_archive_macro(self):
        artifacts = module("artifact_set")
        macro = ROOT / "vendor/ax_check_compile_flag.m4"
        self.assertEqual(hashlib.sha256(macro.read_bytes()).hexdigest(),
                         "629dc6835eb1e2bd586fd842a4db66541bc442bcc2b13d6f24907631c5a688b0")
        self.assertIn("vendor/ax_check_compile_flag.m4", artifacts.INPUTS)
        containerfile = (ROOT / "Containerfile").read_text()
        spec = (ROOT / "rpm/sandbox-apparmor-parser.spec").read_text()
        self.assertNotIn("autoconf-archive", containerfile)
        self.assertNotIn("BuildRequires:  autoconf-archive", spec)
        self.assertIn("Source1:        ax_check_compile_flag.m4", spec)
        self.assertIn("libraries/libapparmor/m4/ax_check_compile_flag.m4", spec)

    def test_parser_runtime_hook_does_not_accept_a_different_version(self):
        checks = module("parser_checks")
        with patch.object(checks.subprocess, "run", return_value=SimpleNamespace(returncode=0, stdout=b"AppArmor parser version 4.1.70\n", stderr=b"")):
            with self.assertRaisesRegex(ValueError, "version"):
                checks.runtime_checks(Path("/usr/sbin/apparmor_parser"), self.root)

    def test_parser_compile_hook_uses_no_load_and_no_cache_flags(self):
        checks = module("parser_checks")
        profile, output = self.root / "test.profile", self.root / "test.bin"
        profile.write_text("fixture")
        def compiled(*args, **kwargs):
            output.write_bytes(b"fixture; not a real profile result")
            return SimpleNamespace(returncode=0, stdout=b"", stderr=b"")
        with patch.object(checks.subprocess, "run", side_effect=compiled) as run:
            checks.compile_profile("/usr/sbin/apparmor_parser", profile, output)
            command = run.call_args.args[0]
            self.assertEqual(command[:3], ["/usr/sbin/apparmor_parser", "-Q", "-K"])
        with patch.object(checks.subprocess, "run", return_value=SimpleNamespace(returncode=1, stdout=b"", stderr=b"")):
            with self.assertRaises(ValueError):
                checks.compile_profile("/usr/sbin/apparmor_parser", profile, output)

    def test_strict_artifact_set_and_checksum_coverage(self):
        artifacts = module("artifact_set")
        expected = artifacts.expected_files("amd64")
        for name in expected - {"SHA256SUMS"}:
            target = self.root / name
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_bytes(name.encode())
        artifacts.write_hashes(self.root, "amd64")
        artifacts.check_hashes(self.root, "amd64")
        (self.root / "unrelated-secret").write_text("private")
        with self.assertRaises(ValueError):
            artifacts.check_hashes(self.root, "amd64")
        (self.root / "unrelated-secret").unlink()
        victim = self.root / sorted(expected - {"SHA256SUMS"})[0]
        victim.write_text("corruption")
        with self.assertRaises(ValueError):
            artifacts.check_hashes(self.root, "amd64")
        victim.unlink()
        with self.assertRaises(ValueError):
            artifacts.check_hashes(self.root, "amd64")


@unittest.skipUnless(shutil.which("gpg"), "GPG required for local ephemeral signature fixtures")
class Signatures(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.tmp = tempfile.TemporaryDirectory()
        cls.root = Path(cls.tmp.name)
        cls.home = cls.root / "gnupg"
        cls.home.mkdir(mode=0o700)
        cls.base = ["gpg", "--no-options", "--homedir", str(cls.home), "--batch", "--pinentry-mode", "loopback", "--passphrase", ""]
        subprocess.run(cls.base + ["--quick-generate-key", "Ephemeral test only <fixture@example.invalid>", "ed25519", "sign", "1d"], check=True, capture_output=True, timeout=30)
        data = subprocess.run(cls.base + ["--with-colons", "--list-keys"], check=True, capture_output=True, text=True, timeout=10).stdout
        cls.fingerprint = next(line.split(":")[9] for line in data.splitlines() if line.startswith("fpr:"))
        cls.keyring = cls.root / "public.gpg"
        cls.keyring.write_bytes(subprocess.run(cls.base + ["--export"], check=True, capture_output=True, timeout=10).stdout)

    @classmethod
    def tearDownClass(cls):
        subprocess.run(["gpgconf", "--homedir", str(cls.home), "--kill", "gpg-agent"], capture_output=True, timeout=10)
        cls.tmp.cleanup()

    def test_real_gpg_good_wrong_signature_signer_and_sha(self):
        policy = module("source_policy")
        source = self.root / "archive"
        source.write_bytes(b"test archive; not AppArmor release evidence")
        signature = self.root / "archive.asc"
        subprocess.run(self.base + ["--yes", "--armor", "--detach-sign", "--output", str(signature), str(source)], check=True, capture_output=True, timeout=10)
        policy.check_keyring(self.keyring, self.fingerprint, self.fingerprint)
        result = policy.verify_signature(source, signature, self.keyring, self.fingerprint, self.fingerprint)
        self.assertEqual(result["primary_fingerprint"], self.fingerprint)
        with self.assertRaises(ValueError):
            policy.verify_signature(source, signature, self.keyring, "F" * 40, "F" * 40)
        with self.assertRaises(ValueError):
            policy.verify_sha256(source, "0" * 64)
        source.write_bytes(b"tampered")
        with self.assertRaises(ValueError):
            policy.verify_signature(source, signature, self.keyring, self.fingerprint, self.fingerprint)

    @unittest.skipUnless(shutil.which("helm"), "Helm required for positive build orchestration fixture")
    def test_orchestration_reaches_docker_with_narrow_context_and_cleans_failure(self):
        workflow = module("workflow")
        artifacts = module("artifact_set")
        lock = self.root / "source.lock"
        lock.write_text(lock_text(self.fingerprint))
        destination = self.root / "artifacts"
        arguments = ["--arch", "amd64", "--builder-image", "example.invalid/hce@sha256:" + "a" * 64,
                     "--output-dir", str(destination), "--source-lock", str(lock), "--trusted-keyring", str(self.keyring)]
        observed = []

        def docker_failure(config, context, output, verify=False):
            observed.append(context)
            files = {path.relative_to(context).as_posix() for path in context.rglob("*") if path.is_file()}
            from profile_policy import RENDER_FILES
            self.assertEqual(files, artifacts.INPUTS | {"source.lock"} | RENDER_FILES)
            self.assertNotIn("trusted-public.gpg", files)
            raise ValueError("intentional Docker boundary fixture")

        with redirect_stderr(io.StringIO()), patch.object(workflow, "docker_build", side_effect=docker_failure):
            self.assertEqual(workflow.main("build", arguments), 2)
        self.assertEqual(len(observed), 1, "valid CLI must reach build rather than unconditionally reject")
        self.assertFalse(observed[0].exists(), "temporary context leaked")
        self.assertFalse(destination.exists(), "failed build published artifacts")
        destination.mkdir()
        (destination / "user-data").write_text("preserve")
        with redirect_stderr(io.StringIO()), patch.object(workflow, "docker_build") as docker:
            self.assertEqual(workflow.main("build", arguments), 2)
            docker.assert_not_called()
        self.assertEqual((destination / "user-data").read_text(), "preserve")

    @unittest.skipUnless(shutil.which("helm"), "Helm required to reach the Docker boundary")
    def test_real_cli_without_docker_fails_after_valid_trust_without_publishing(self):
        import os
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            binaries = root / "bin"
            binaries.mkdir()
            for name in ("bash", "dirname", "python3", "gpg", "helm"):
                (binaries / name).symlink_to(shutil.which(name))
            lock = root / "source.lock"
            lock.write_text(lock_text(self.fingerprint))
            output = root / "out"
            result = subprocess.run([str(ROOT / "build.sh"), "--arch", "amd64", "--builder-image",
                                     "example.invalid/hce@sha256:" + "a" * 64, "--output-dir", str(output),
                                     "--source-lock", str(lock), "--trusted-keyring", str(self.keyring)],
                                    env={**os.environ, "PATH": str(binaries)}, capture_output=True, text=True, timeout=30)
            self.assertEqual(result.returncode, 2)
            self.assertIn("Docker buildx unavailable", result.stderr)
            self.assertFalse(output.exists())
            self.assertFalse(list(root.glob(".apparmor-hce-*")))

    def test_signing_subkey_and_primary_fingerprints_are_distinct(self):
        policy = module("source_policy")
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            base = ["gpg", "--no-options", "--homedir", str(root), "--batch", "--pinentry-mode", "loopback", "--passphrase", ""]
            subprocess.run(base + ["--quick-generate-key", "Subkey fixture <subkey@example.invalid>", "ed25519", "cert", "1d"], check=True, capture_output=True, timeout=30)
            listed = subprocess.run(base + ["--with-colons", "--list-keys"], check=True, capture_output=True, text=True, timeout=10).stdout
            primary = next(line.split(":")[9] for line in listed.splitlines() if line.startswith("fpr:"))
            subprocess.run(base + ["--quick-add-key", primary, "ed25519", "sign", "1d"], check=True, capture_output=True, timeout=30)
            listed = subprocess.run(base + ["--with-colons", "--list-keys"], check=True, capture_output=True, text=True, timeout=10).stdout
            signing = [line.split(":")[9] for line in listed.splitlines() if line.startswith("fpr:")][-1]
            self.assertNotEqual(primary, signing)
            keyring, source, signature = root / "public.gpg", root / "data", root / "sig"
            keyring.write_bytes(subprocess.run(base + ["--export"], check=True, capture_output=True, timeout=10).stdout)
            source.write_bytes(b"ephemeral signing-subkey fixture")
            subprocess.run(base + ["--detach-sign", "--output", str(signature), str(source)], check=True, capture_output=True, timeout=10)
            self.assertEqual(policy.verify_signature(source, signature, keyring, primary, signing),
                             {"primary_fingerprint": primary, "signing_fingerprint": signing})
            with self.assertRaises(ValueError):
                policy.verify_signature(source, signature, keyring, primary, primary)
            subprocess.run(["gpgconf", "--homedir", str(root), "--kill", "gpg-agent"], capture_output=True, timeout=10)


if __name__ == "__main__":
    unittest.main()
