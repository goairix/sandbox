import hashlib
import unittest
from pathlib import Path
import tempfile
from unittest.mock import patch
from types import SimpleNamespace

from test_validation import module


class UnixProbe(unittest.TestCase):
    def test_candidate_removes_only_one_unix_allowance(self):
        probe = module("unix_probe")
        original = b"profile __SANDBOX_PROFILE_NAME__ {\n  network inet stream,\n  network unix stream,\n}\n"
        name, result = probe.candidate_bytes(original)
        self.assertTrue(name.startswith("sandbox-fuse-unix-probe-"))
        self.assertIn(b"  network inet stream,\n", result)
        self.assertNotIn(b"  network unix stream,\n", result)
        expected = original.replace(b"  network unix stream,\n", b"", 1)
        self.assertEqual(result, expected.replace(b"__SANDBOX_PROFILE_NAME__", name.encode()))

    def test_candidate_rejects_missing_or_duplicate_unix_rule(self):
        probe = module("unix_probe")
        for source in (b"profile __SANDBOX_PROFILE_NAME__ {}\n",
                       b"profile __SANDBOX_PROFILE_NAME__ {\n  network unix stream,\n  network unix stream,\n}\n"):
            with self.assertRaises(ValueError):
                probe.candidate_bytes(source)

    def test_compile_candidate_uses_strict_flags_and_both_target_feature_inputs(self):
        probe = module("unix_probe")
        with tempfile.TemporaryDirectory() as directory:
            profile = Path(directory) / "candidate.profile"
            features = Path(directory) / "features"
            profile.write_text("profile candidate {}\n")
            features.write_text("features\n")
            output = Path(directory) / "candidate.bin"
            def parser(args, **kwargs):
                output.write_bytes(b"compiled")
                return SimpleNamespace(returncode=0, stdout=b"", stderr=b"")
            with patch.object(probe.parser_checks.subprocess, "run", side_effect=parser) as run:
                result = probe.compile_candidate("/tmp/parser", profile, features, output)
        args = run.call_args.args[0]
        self.assertEqual(args[:6], ["/tmp/parser", "-Q", "-K", "--config-file=/dev/null", "--Werror", "--warn=all"])
        self.assertEqual(Path(args[args.index("--policy-features") + 1]), features)
        self.assertEqual(Path(args[args.index("--kernel-features") + 1]), features)
        self.assertFalse(result["kernel_load"])


if __name__ == "__main__":
    unittest.main()
