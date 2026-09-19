"""Real Helm rendering plus subprocess-boundary parser tests, not HCE evidence."""
import hashlib
import io
import json
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

from test_validation import ROOT, module

CHART = ROOT.parents[1] / "deploy/helm/sandbox"


class Profile(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.root = Path(self.tmp.name)
        self.addCleanup(self.tmp.cleanup)

    @unittest.skipUnless(shutil.which("helm"), "Helm required for real Chart-equivalence test")
    def test_real_helm_render_matches_full_production_profile(self):
        policy = module("profile_policy")
        workflow = module("workflow")
        workflow.copy_inputs(self.root)
        policy.render_profile(self.root)
        actual = (self.root / "profile/workspace.profile").read_text()
        empty_config = self.root / "reference-kubeconfig"
        empty_config.write_text("apiVersion: v1\nkind: Config\nclusters: []\ncontexts: []\nusers: []\n")
        empty_config.chmod(0o600)
        import os
        result = subprocess.run(["helm", "template", "sandbox", str(CHART), "--namespace", "release-ns",
                                 "--show-only", "templates/apparmor-loader.yaml", "--set", "apparmorLoader.enabled=true",
                                 "--set", "config.workspace.lsmProfile="], capture_output=True, text=True, timeout=30,
                                env={**os.environ, "KUBECONFIG": str(empty_config)})
        self.assertEqual(result.returncode, 0, "actual Chart must render successfully")
        match = re.search(r"(?m)^  profile: \|\n((?:    .*\n|\n)+)", result.stdout)
        self.assertIsNotNone(match)
        chart_profile = "\n".join(line[4:] for line in match[1].splitlines() if line.startswith("    ")) + "\n"
        self.assertEqual(actual, chart_profile)
        original = (CHART / "files/apparmor/workspace-mounter.profile").read_bytes()
        meta = policy.validate_render(self.root, expected_template=original)
        self.assertIn("sandbox.apparmor.profile-digest: \"" + meta["template_sha256"] + "\"", result.stdout)
        self.assertIn("/dev/fuse rw,", actual)
        self.assertIn("mount fstype=fuse.s3fs -> /workspace/,", actual)
        for changed in [actual.replace("/dev/fuse rw,", ""), actual.replace("/workspace/** rwkl,", "/** rwkl,"),
                        actual.replace("sandbox-fuse-", "__SANDBOX_PROFILE_NAME__", 1)]:
            (self.root / "profile/workspace.profile").write_text(changed)
            with self.assertRaises(ValueError):
                policy.validate_render(self.root, expected_template=original)

    def test_normalization_preserves_policy_and_profile_name_binding(self):
        policy = module("profile_policy")
        original = (CHART / "files/apparmor/workspace-mounter.profile").read_bytes()
        normal = policy.normalize(original)
        self.assertEqual(policy.normalize(b"\r\n" + original.replace(b"\n", b"\r\n") + b" \r\n"), normal)
        self.assertEqual(normal[-1:], b"\n")
        self.assertEqual(normal.count(b"__SANDBOX_PROFILE_NAME__"), 1)

    def test_features_fixture_is_exact_source_member_and_not_target_kernel(self):
        policy = module("profile_policy")
        archive = self.root / "source.tar.gz"
        fixture = b"mount {mask {mount umount\n}\n}\n"
        with tarfile.open(archive, "w:gz") as tar:
            item = tarfile.TarInfo(policy.FEATURE_MEMBER)
            item.size = len(fixture)
            tar.addfile(item, io.BytesIO(fixture))
        policy.extract_feature_fixture(archive, self.root)
        report = policy.validate_feature_fixture(archive, self.root)
        self.assertEqual(report["kind"], "upstream-test-fixture")
        self.assertFalse(report["target_kernel_compatibility"])
        self.assertEqual(report["sha256"], hashlib.sha256(fixture).hexdigest())
        self.assertEqual(report["source_archive_sha256"], hashlib.sha256(archive.read_bytes()).hexdigest())
        (self.root / "profile/kernel-features.fixture").write_bytes(b"tampered")
        with self.assertRaises(ValueError):
            policy.validate_feature_fixture(archive, self.root)

    def test_default_abi_is_extracted_from_signed_source_and_bound_separately(self):
        policy = module("profile_policy")
        archive = self.root / "source.tar.gz"
        source = b'''const char *default_features_abi =
"query {label {multi_transaction {yes\\\n}\\\n}}\\\n";
'''
        with tarfile.open(archive, "w:gz") as tar:
            item = tarfile.TarInfo(policy.DEFAULT_MEMBER)
            item.size = len(source)
            tar.addfile(item, io.BytesIO(source))
        policy.extract_default_abi(archive, self.root)
        report = policy.validate_default_abi(archive, self.root)
        self.assertEqual(report["kind"], "source-default-policy-abi")
        self.assertFalse(report["target_kernel_compatibility"])
        self.assertEqual((self.root / "profile/policy-features.default").read_bytes(), b"query {label {multi_transaction {yes}}}")
        self.assertEqual(report["sha256"], hashlib.sha256((self.root / "profile/policy-features.default").read_bytes()).hexdigest())
        (self.root / "profile/policy-features.default").write_bytes(b"query {label {multi_transaction {no}}}\n")
        with self.assertRaises(ValueError):
            policy.validate_default_abi(archive, self.root)

    def test_compile_rejects_stdout_stderr_nonzero_and_records_audit(self):
        checks = module("parser_checks")
        profile, output, features = self.root / "workspace.profile", self.root / "workspace.bin", self.root / "features"
        policy_features = self.root / "policy-features"
        profile.write_text("fixture")
        features.write_text("fixture")
        policy_features.write_text("default ABI fixture")
        for code, stdout, stderr in [(0, b"", b"Warning: unsupported features"),
                                     (0, b"Warning: ABI mismatch", b""), (1, b"", b"compile failed")]:
            output.write_bytes(b"fixture output")
            with patch.object(checks.subprocess, "run", return_value=SimpleNamespace(returncode=code, stdout=stdout, stderr=stderr)):
                with self.assertRaises(ValueError):
                    checks.compile_profile("/usr/sbin/apparmor_parser", profile, output, features=features,
                                           policy_features=policy_features)
            report = json.loads((self.root / "compile.json").read_text())
            self.assertFalse(report["passed"])
            self.assertEqual(report["exit_status"], code)
            self.assertEqual(report["stdout_sha256"], hashlib.sha256(stdout).hexdigest())
            self.assertEqual(report["stderr_sha256"], hashlib.sha256(stderr).hexdigest())

    def test_compile_enables_werror_and_never_overrides_policy_abi(self):
        checks = module("parser_checks")
        profile, output, features = self.root / "workspace.profile", self.root / "workspace.bin", self.root / "features"
        policy_features = self.root / "policy-features"
        profile.write_text("fixture")
        features.write_text("fixture")
        policy_features.write_text("default ABI fixture")
        def compiled(*args, **kwargs):
            output.write_bytes(b"fixture output")
            return SimpleNamespace(returncode=0, stdout=b"", stderr=b"")
        with patch.object(checks.subprocess, "run", side_effect=compiled) as run:
            result = checks.compile_profile("/usr/sbin/apparmor_parser", profile, output, features=features,
                                           policy_features=policy_features)
        args = run.call_args.args[0]
        self.assertIn("--Werror", args)
        self.assertIn("--kernel-features", args)
        self.assertIn("--policy-features", args)
        for forbidden in ("--override-policy-abi", "-M", "-m", "--quiet", "-q", "-d"):
            self.assertNotIn(forbidden, args)
        self.assertTrue(result["passed"])

    def test_compile_does_not_accept_stale_output_or_timeout(self):
        checks = module("parser_checks")
        profile, output = self.root / "workspace.profile", self.root / "workspace.bin"
        profile.write_text("fixture")
        output.write_bytes(b"stale result from a previous invocation")
        with patch.object(checks.subprocess, "run", return_value=SimpleNamespace(returncode=0, stdout=b"", stderr=b"")):
            with self.assertRaises(ValueError):
                checks.compile_profile("/usr/sbin/apparmor_parser", profile, output)
        with patch.object(checks.subprocess, "run", side_effect=subprocess.TimeoutExpired("parser", 120, output=b"partial", stderr=b"diagnostic")):
            with self.assertRaises(ValueError):
                checks.compile_profile("/usr/sbin/apparmor_parser", profile, output)
        report = json.loads((self.root / "compile.json").read_text())
        self.assertEqual(report["execution_failure"], "timeout")
        self.assertIsNone(report["exit_status"])

    def test_compilation_report_binds_profile_features_binary_and_diagnostics(self):
        checks = module("parser_checks")
        target = self.root / "profile"
        target.mkdir()
        profile, features, output = target / "workspace.profile", target / "kernel-features.fixture", target / "workspace.bin"
        policy_features = target / "policy-features.default"
        profile.write_text("fixture")
        features.write_text("source fixture")
        policy_features.write_text("default ABI fixture")
        def compiled(*args, **kwargs):
            output.write_bytes(b"subprocess boundary fixture, not AppArmor binary")
            return SimpleNamespace(returncode=0, stdout=b"", stderr=b"")
        with patch.object(checks.subprocess, "run", side_effect=compiled):
            checks.compile_profile("/usr/sbin/apparmor_parser", profile, output, features=features,
                                  policy_features=policy_features)
        self.assertTrue(checks.validate_compilation(self.root)["passed"])
        (target / "compile.stderr").write_bytes(b"warning")
        with self.assertRaises(ValueError):
            checks.validate_compilation(self.root)

    @unittest.skipUnless(shutil.which("helm"), "Helm required for full profile invocation test")
    def test_runtime_invokes_complete_rendered_profile_and_records_both_commands(self):
        checks, policy, workflow = module("parser_checks"), module("profile_policy"), module("workflow")
        workflow.copy_inputs(self.root)
        metadata = policy.render_profile(self.root)
        (self.root / "source").mkdir()
        archive = self.root / "source/apparmor-v4.1.7.tar.gz"
        fixture = b"file {mask {read write\n}\n}\n"
        default_source = b'''const char *default_features_abi =\n"query {label {multi_transaction {yes\\\n}\\\n}}\\\n";\n'''
        with tarfile.open(archive, "w:gz") as tar:
            item = tarfile.TarInfo(policy.FEATURE_MEMBER)
            item.size = len(fixture)
            tar.addfile(item, io.BytesIO(fixture))
            item = tarfile.TarInfo(policy.DEFAULT_MEMBER)
            item.size = len(default_source)
            tar.addfile(item, io.BytesIO(default_source))
        policy.extract_feature_fixture(archive, self.root)
        policy.extract_default_abi(archive, self.root)
        def parser(args, **kwargs):
            if args[-1] == "--version":
                self.assertEqual(args[-2:], ["--config-file=/dev/null", "--version"])
                return SimpleNamespace(returncode=0, stdout=b"AppArmor parser version 4.1.7\n", stderr=b"")
            self.assertEqual(Path(args[-1]), self.root / "profile/workspace.profile")
            self.assertEqual(hashlib.sha256(Path(args[-1]).read_bytes()).hexdigest(), metadata["profile_sha256"])
            self.assertIn("mount fstype=fuse.s3fs", Path(args[-1]).read_text())
            Path(args[args.index("-o") + 1]).write_bytes(b"subprocess fixture; no real parser execution")
            return SimpleNamespace(returncode=0, stdout=b"", stderr=b"")
        with patch.object(checks.subprocess, "run", side_effect=parser):
            result = checks.runtime_checks(Path("/usr/sbin/apparmor_parser"), self.root)
        self.assertEqual(result["version_command"]["exit_status"], 0)
        self.assertEqual(result["workspace_profile"]["profile_sha256"], metadata["profile_sha256"])
        self.assertTrue(result["workspace_profile"]["passed"])
        self.assertFalse(result["compile_features"]["target_kernel_compatibility"])


if __name__ == "__main__":
    unittest.main()
