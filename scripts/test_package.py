"""Unit tests for multi-format release packaging (binaries, archives, DEB, RPM)."""
import importlib.util
import os
from pathlib import Path
import tempfile
import unittest
from unittest import mock

SPEC = importlib.util.spec_from_file_location("package", Path(__file__).with_name("package.py"))
packager = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(packager)


class PackageScriptTests(unittest.TestCase):
    def test_sanitize_pkg_version_handles_tags_and_prereleases(self):
        self.assertEqual(packager.sanitize_pkg_version("v0.2.0"), "0.2.0")
        self.assertEqual(packager.sanitize_pkg_version("0.1"), "0.1")
        self.assertEqual(packager.sanitize_pkg_version("v1.0.4-rc.1"), "1.0.4~rc.1")
        self.assertEqual(packager.sanitize_pkg_version("nightly"), "0.0.0~nightly")

    def test_resolve_version_prefers_explicit_env_and_tags(self):
        with mock.patch.dict(os.environ, {"MIGRATION_VERSION": "v0.3.1"}, clear=True):
            self.assertEqual(packager.resolve_version(), "0.3.1")
        with mock.patch.dict(os.environ, {"GITHUB_REF_TYPE": "tag", "GITHUB_REF_NAME": "0.2"}, clear=True):
            self.assertEqual(packager.resolve_version(), "0.2")
        with mock.patch.dict(os.environ, {"GITHUB_REF_TYPE": "branch", "GITHUB_REF_NAME": "main"}, clear=True):
            self.assertEqual(packager.resolve_version(), packager.default_version())

    def test_deb_control_and_rpm_spec_metadata(self):
        control = packager.render_deb_control("0.2.0~rc.1", "arm64", 12345)
        self.assertIn("Package: rustfs-migration\n", control)
        self.assertIn("Version: 0.2.0~rc.1\n", control)
        self.assertIn("Architecture: arm64\n", control)

        spec = packager.render_rpm_spec("0.2.0~rc.1")
        self.assertIn("Name:           rustfs-migration\n", spec)
        self.assertIn("Version:        0.2.0~rc.1\n", spec)
        self.assertNotIn("Version:        0.2.0-", spec)
        self.assertIn("/usr/bin/migration\n", spec)

    def test_build_deb_and_rpm_invocations_with_mocked_tools(self):
        with tempfile.TemporaryDirectory() as temp:
            tmp = Path(temp)
            fake_bin = tmp / "migration"
            fake_bin.write_bytes(b"ELF-binary-placeholder")
            fake_bin.chmod(0o755)
            out = tmp / "dist"
            out.mkdir()

            def fake_run(cmd, **kwargs):
                if cmd[0] == "dpkg-deb":
                    Path(cmd[-1]).write_bytes(b"fake-deb-payload")
                elif cmd[0] == "rpmbuild":
                    topdir = Path(cmd[2].split(" ", 1)[1])
                    arch = cmd[4].split("-", 1)[0]
                    rpm_dir = topdir / "RPMS" / arch
                    rpm_dir.mkdir(parents=True, exist_ok=True)
                    (rpm_dir / f"rustfs-migration-0.2.0-1.{arch}.rpm").write_bytes(b"fake-rpm-payload")

            with mock.patch("shutil.which", return_value="/usr/bin/mock-tool"), \
                 mock.patch("subprocess.run", side_effect=fake_run):
                for goarch, deb_arch, rpm_arch in (("amd64", "amd64", "x86_64"), ("arm64", "arm64", "aarch64")):
                    deb = packager.build_deb(fake_bin, goarch, "0.2.0", out)
                    rpm = packager.build_rpm(fake_bin, goarch, "0.2.0", out)
                    self.assertEqual(deb.name, f"rustfs-migration_0.2.0_{deb_arch}.deb")
                    self.assertEqual(rpm.name, f"rustfs-migration-0.2.0-1.{rpm_arch}.rpm")
                    self.assertTrue(deb.with_name(deb.name + ".sha256").is_file())
                    self.assertTrue(rpm.with_name(rpm.name + ".sha256").is_file())


if __name__ == "__main__":
    unittest.main()
