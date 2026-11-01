"""Launcher validation tests; no installed RustFS or real S3 service is used."""
import contextlib
import hashlib
import importlib.util
import io
import os
from pathlib import Path
import tempfile
import unittest
from unittest import mock

SPEC = importlib.util.spec_from_file_location("audit_local_rustfs", Path(__file__).with_name("audit-local-rustfs.py"))
launcher = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(launcher)


class ReleaseValidationTests(unittest.TestCase):
    def binary(self, directory, version):
        path = Path(directory) / "rustfs"
        path.write_text("#!/bin/sh\nprintf '%s\\n' 'rustfs " + version + "'\n")
        path.chmod(0o700)
        return path

    def test_exact_release_and_digest_are_recorded(self):
        with tempfile.TemporaryDirectory() as directory:
            binary = self.binary(directory, "1.0.4")
            result = launcher.inspect_binary(str(binary), "1.0.4")
            self.assertEqual(result["version"], "1.0.4")
            self.assertEqual(result["binary_path"], str(binary.resolve()))
            self.assertEqual(result["sha256"], hashlib.sha256(binary.read_bytes()).hexdigest())

    def test_wrong_release_and_prerelease_are_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            for version in ("1.0.3", "1.0.4-alpha.1", "1.0.40"):
                with self.subTest(version=version):
                    binary = self.binary(directory, version)
                    with self.assertRaisesRegex(ValueError, "version mismatch"):
                        launcher.inspect_binary(str(binary), "1.0.4")

    def test_relative_binary_is_rejected(self):
        with self.assertRaisesRegex(ValueError, "absolute path"):
            launcher.inspect_binary("rustfs", "1.0.4")

    def test_missing_binary_does_not_fall_back_to_path(self):
        with mock.patch.dict(os.environ, {"PATH": "/some/old/rustfs"}, clear=True):
            with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit) as error:
                launcher.main([])
            self.assertEqual(error.exception.code, 2)

    def test_node_environment_excludes_external_credentials(self):
        with mock.patch.dict(os.environ, {
            "PATH": "/bin", "RUSTFS_ACCESS_KEY": "external", "AWS_SECRET_ACCESS_KEY": "external",
            "RUSTFS_VOLUMES": "/external/data", "RUSTFS_CONSOLE_ENABLE": "true",
        }, clear=True):
            self.assertEqual(launcher.clean_rustfs_env(), {"PATH": "/bin", "RUSTFS_CONSOLE_ENABLE": "false"})

    def test_unrecognized_version_output_is_rejected(self):
        for output in ("1.0.4", "unrelated 1.0.4", "rustfs development"):
            with self.subTest(output=output), self.assertRaises(ValueError):
                launcher.parse_version(output)

    def test_release_long_version_output(self):
        output = """rustfs 1.0.4
build time   : 2026-10-25 16:26:17
build target : aarch64-apple-darwin
git commit   : 46337942d55f3a8e6a97b147ca98d9ea8115555d
git tag      : 1.0.4
"""
        self.assertEqual(launcher.parse_version(output), "1.0.4")


if __name__ == "__main__":
    unittest.main()
