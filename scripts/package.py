"""Build one standalone migration archive and its SHA-256 checksum."""
import argparse
import hashlib
import os
from pathlib import Path
import subprocess
import tarfile
import tempfile
import zipfile

ROOT = Path(__file__).resolve().parents[1]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("target", choices=["linux-amd64", "linux-arm64", "windows-amd64"])
    parser.add_argument("output", type=Path, help="directory for the archive and checksum")
    args = parser.parse_args()
    goos, goarch = args.target.split("-")
    windows = goos == "windows"
    env = {**os.environ, "CGO_ENABLED": "0", "GOOS": goos, "GOARCH": goarch}
    args.output.mkdir(parents=True, exist_ok=True)
    suffix = ".zip" if windows else ".tar.gz"
    archive = args.output / f"migration-{args.target}{suffix}"
    with tempfile.TemporaryDirectory() as temp:
        binary = Path(temp) / ("migration.exe" if windows else "migration")
        subprocess.run(["go", "build", "-trimpath", "-ldflags=-s -w", "-o", binary, "."],
                       cwd=ROOT, env=env, check=True)
        files = [binary, ROOT / "LICENSE", ROOT / "config.example.yaml"]
        if windows:
            with zipfile.ZipFile(archive, "w", zipfile.ZIP_DEFLATED) as package:
                for file in files:
                    package.write(file, file.name)
        else:
            with tarfile.open(archive, "w:gz") as package:
                for file in files:
                    package.add(file, arcname=file.name)
    checksum = hashlib.sha256(archive.read_bytes()).hexdigest()
    archive.with_name(archive.name + ".sha256").write_bytes(f"{checksum}  {archive.name}\n".encode())


if __name__ == "__main__":
    main()
