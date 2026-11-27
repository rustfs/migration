#!/usr/bin/env python3
"""Build standalone binaries, tar/zip archives, DEB and RPM packages, and SHA-256 checksums."""
import argparse
import hashlib
import math
import os
from pathlib import Path
import re
import shutil
import subprocess
import tarfile
import tempfile
import zipfile

ROOT = Path(__file__).resolve().parents[1]
TARGETS = ("linux-amd64", "linux-arm64", "darwin-amd64", "darwin-arm64", "windows-amd64")
DEB_ARCH = {"amd64": "amd64", "arm64": "arm64"}
RPM_ARCH = {"amd64": "x86_64", "arm64": "aarch64"}


def default_version():
    helptext = (ROOT / "cmd" / "helptext.go").read_text(encoding="utf-8")
    match = re.search(r'\bVersion\s*=\s*"([^"]+)"', helptext)
    return match.group(1) if match else "0.2.0"


def resolve_version():
    for key in ("MIGRATION_VERSION", "GITHUB_REF_NAME"):
        val = os.environ.get(key, "").strip()
        if val and (key == "MIGRATION_VERSION" or os.environ.get("GITHUB_REF_TYPE") == "tag"):
            if val.startswith("v") and len(val) > 1 and val[1].isdigit():
                return val[1:]
            return val
    return default_version()


def sanitize_pkg_version(version):
    """Normalize a version string so both dpkg-deb and rpmbuild accept it."""
    cleaned = version.strip()
    if cleaned.startswith("v") and len(cleaned) > 1 and cleaned[1].isdigit():
        cleaned = cleaned[1:]
    cleaned = cleaned.replace("-", "~")
    cleaned = re.sub(r"[^0-9A-Za-z.+~]", ".", cleaned).strip(".")
    if not cleaned or not cleaned[0].isdigit():
        cleaned = f"0.0.0~{cleaned}" if cleaned else "0.0.0"
    return cleaned


def write_sha256(artifact):
    checksum = hashlib.sha256(artifact.read_bytes()).hexdigest()
    artifact.with_name(artifact.name + ".sha256").write_bytes(
        f"{checksum}  {artifact.name}\n".encode("utf-8")
    )


def render_deb_control(pkg_version, deb_arch, installed_kib):
    return (
        f"Package: rustfs-migration\n"
        f"Version: {pkg_version}\n"
        f"Section: utils\n"
        f"Priority: optional\n"
        f"Architecture: {deb_arch}\n"
        f"Installed-Size: {installed_kib}\n"
        f"Maintainer: RustFS Developers <hello@rustfs.com>\n"
        f"Homepage: https://github.com/rustfs/migration\n"
        f"Provides: migration\n"
        f"Description: Safe one-way S3 object storage migration tool\n"
        f" Copies current objects from one S3-compatible cluster to another with\n"
        f" atomic conditional writes, SQLite progress checkpoints, and full-body\n"
        f" SHA-256 plus metadata verification.\n"
    )


def render_rpm_spec(pkg_version):
    return (
        "%global debug_package %{nil}\n"
        "%define __strip /bin/true\n"
        "%define _build_id_links none\n"
        "%define _binary_payload w9.gzdio\n"
        "\n"
        "Name:           rustfs-migration\n"
        f"Version:        {pkg_version}\n"
        "Release:        1\n"
        "Summary:        Safe one-way S3 object storage migration tool\n"
        "License:        Apache-2.0\n"
        "URL:            https://github.com/rustfs/migration\n"
        "Provides:       migration = %{version}-%{release}\n"
        "\n"
        "%description\n"
        "Copies current objects from one S3-compatible cluster to another with\n"
        "atomic conditional writes, SQLite progress checkpoints, and full-body\n"
        "SHA-256 plus metadata verification.\n"
        "\n"
        "%prep\n"
        "\n"
        "%build\n"
        "\n"
        "%install\n"
        "rm -rf %{buildroot}\n"
        "install -D -m 0755 %{_sourcedir}/migration %{buildroot}/usr/bin/migration\n"
        "install -D -m 0644 %{_sourcedir}/config.example.yaml %{buildroot}/etc/rustfs-migration/config.example.yaml\n"
        "install -D -m 0644 %{_sourcedir}/LICENSE %{buildroot}/usr/share/doc/rustfs-migration/LICENSE\n"
        "install -D -m 0644 %{_sourcedir}/README.md %{buildroot}/usr/share/doc/rustfs-migration/README.md\n"
        "\n"
        "%files\n"
        "%defattr(-,root,root,-)\n"
        "/usr/bin/migration\n"
        "%dir /etc/rustfs-migration\n"
        "%config(noreplace) /etc/rustfs-migration/config.example.yaml\n"
        "%dir /usr/share/doc/rustfs-migration\n"
        "%doc /usr/share/doc/rustfs-migration/LICENSE\n"
        "%doc /usr/share/doc/rustfs-migration/README.md\n"
    )


def build_deb(binary, goarch, pkg_version, output_dir):
    if shutil.which("dpkg-deb") is None:
        raise RuntimeError("dpkg-deb is required to build .deb packages")
    deb_arch = DEB_ARCH[goarch]
    deb_path = output_dir / f"rustfs-migration_{pkg_version}_{deb_arch}.deb"
    with tempfile.TemporaryDirectory(prefix="migration-deb-") as temp:
        root = Path(temp) / "pkg"
        (root / "DEBIAN").mkdir(parents=True)
        (root / "usr" / "bin").mkdir(parents=True)
        (root / "etc" / "rustfs-migration").mkdir(parents=True)
        (root / "usr" / "share" / "doc" / "rustfs-migration").mkdir(parents=True)

        bin_dst = root / "usr" / "bin" / "migration"
        shutil.copy2(binary, bin_dst)
        bin_dst.chmod(0o755)

        cfg_dst = root / "etc" / "rustfs-migration" / "config.example.yaml"
        shutil.copy2(ROOT / "config.example.yaml", cfg_dst)
        cfg_dst.chmod(0o644)

        total_bytes = bin_dst.stat().st_size + cfg_dst.stat().st_size
        for doc_name in ("LICENSE", "README.md"):
            dst = root / "usr" / "share" / "doc" / "rustfs-migration" / doc_name
            shutil.copy2(ROOT / doc_name, dst)
            dst.chmod(0o644)
            total_bytes += dst.stat().st_size

        for dirpath, dirnames, _ in os.walk(root):
            Path(dirpath).chmod(0o755)
            for d in dirnames:
                (Path(dirpath) / d).chmod(0o755)

        installed_kib = max(1, math.ceil(total_bytes / 1024))
        control = root / "DEBIAN" / "control"
        control.write_text(render_deb_control(pkg_version, deb_arch, installed_kib), encoding="utf-8")
        control.chmod(0o644)

        subprocess.run(
            ["dpkg-deb", "--root-owner-group", "-Zgzip", "--build", str(root), str(deb_path)],
            check=True,
        )
    write_sha256(deb_path)
    return deb_path


def build_rpm(binary, goarch, pkg_version, output_dir):
    if shutil.which("rpmbuild") is None:
        raise RuntimeError("rpmbuild is required to build .rpm packages")
    rpm_arch = RPM_ARCH[goarch]
    rpm_path = output_dir / f"rustfs-migration-{pkg_version}-1.{rpm_arch}.rpm"
    with tempfile.TemporaryDirectory(prefix="migration-rpm-") as temp:
        topdir = Path(temp) / "rpmbuild"
        for sub in ("BUILD", "BUILDROOT", "RPMS", "SOURCES", "SPECS", "SRPMS"):
            (topdir / sub).mkdir(parents=True)

        shutil.copy2(binary, topdir / "SOURCES" / "migration")
        for asset in ("config.example.yaml", "LICENSE", "README.md"):
            shutil.copy2(ROOT / asset, topdir / "SOURCES" / asset)

        spec = topdir / "SPECS" / "rustfs-migration.spec"
        spec.write_text(render_rpm_spec(pkg_version), encoding="utf-8")

        subprocess.run(
            [
                "rpmbuild",
                "--define", f"_topdir {topdir}",
                "--target", f"{rpm_arch}-linux",
                "-bb", str(spec),
            ],
            check=True,
        )
        built = list((topdir / "RPMS").rglob("*.rpm"))
        if len(built) != 1:
            raise RuntimeError(f"expected 1 built RPM, found {built}")
        shutil.copy2(built[0], rpm_path)
    write_sha256(rpm_path)
    return rpm_path


def build_target(target, output_dir, linux_packages=False):
    goos, goarch = target.split("-")
    windows = goos == "windows"
    version = resolve_version()
    pkg_version = sanitize_pkg_version(version)
    env = {**os.environ, "CGO_ENABLED": "0", "GOOS": goos, "GOARCH": goarch}
    output_dir.mkdir(parents=True, exist_ok=True)

    ldflags = f"-s -w -X github.com/rustfs/migration/cmd.Version={version}"
    standalone_name = f"migration-{target}.exe" if windows else f"migration-{target}"
    standalone_path = output_dir / standalone_name
    archive_suffix = ".zip" if windows else ".tar.gz"
    archive_path = output_dir / f"migration-{target}{archive_suffix}"

    with tempfile.TemporaryDirectory(prefix="migration-build-") as temp:
        binary = Path(temp) / ("migration.exe" if windows else "migration")
        subprocess.run(
            ["go", "build", "-trimpath", f"-ldflags={ldflags}", "-o", str(binary), "."],
            cwd=ROOT, env=env, check=True,
        )
        if not windows:
            binary.chmod(0o755)

        shutil.copy2(binary, standalone_path)
        if not windows:
            standalone_path.chmod(0o755)
        write_sha256(standalone_path)

        files = [binary, ROOT / "LICENSE", ROOT / "README.md", ROOT / "config.example.yaml"]
        if windows:
            with zipfile.ZipFile(archive_path, "w", zipfile.ZIP_DEFLATED) as package:
                for file in files:
                    package.write(file, file.name)
        else:
            with tarfile.open(archive_path, "w:gz") as package:
                for file in files:
                    package.add(file, arcname=file.name)
        write_sha256(archive_path)

        if linux_packages:
            if goos != "linux" or goarch not in DEB_ARCH:
                raise ValueError(f"--linux-packages is only supported for linux-amd64/linux-arm64, got {target}")
            build_deb(binary, goarch, pkg_version, output_dir)
            build_rpm(binary, goarch, pkg_version, output_dir)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("target", choices=TARGETS)
    parser.add_argument("output", type=Path, help="directory for built binaries, packages, and checksums")
    parser.add_argument(
        "--linux-packages",
        action="store_true",
        help="also build .deb and .rpm packages (requires dpkg-deb and rpmbuild)",
    )
    args = parser.parse_args(argv)
    build_target(args.target, args.output, linux_packages=args.linux_packages)


if __name__ == "__main__":
    main()
