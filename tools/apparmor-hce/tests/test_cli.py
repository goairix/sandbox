"""No Docker/network invocation: exercise actual early CLI rejection paths."""
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]


class CLI(unittest.TestCase):
    def test_rejection_has_no_output_side_effect_or_secret(self):
        with tempfile.TemporaryDirectory() as tmp:
            output = Path(tmp) / "artifacts"
            base = [str(ROOT / "build.sh"), "--arch", "amd64", "--builder-image",
                    "example.invalid/hce@sha256:" + "a" * 64, "--output-dir", str(output)]
            cases = [[], ["--source-lock", str(Path(tmp) / "absent")],
                     ["--trusted-keyring", str(Path(tmp) / "absent")],
                     ["--unknown=SENTINEL_SECRET"], ["--arch", "SENTINEL_SECRET"]]
            for args in cases:
                result = subprocess.run(base + args, text=True, capture_output=True, timeout=10,
                                        env={**os.environ, "APPARMOR_SECRET": "SENTINEL_SECRET"})
                self.assertNotEqual(result.returncode, 0)
                self.assertNotIn("SENTINEL_SECRET", result.stdout + result.stderr)
                self.assertFalse(output.exists(), "invalid input created output before trust validation")


if __name__ == "__main__":
    unittest.main()
