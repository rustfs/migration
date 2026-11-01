#!/usr/bin/env python3
"""Run real_s3 tests against an explicitly selected, verified RustFS release."""
import argparse
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import re
import signal
import socket
import subprocess
import sys
import tempfile
import time

ROOT = Path(__file__).resolve().parents[1]
DEFAULT_VERSION = "1.0.4"
TOTAL_TIMEOUT_SECONDS = 420
GO_TIMEOUT_SECONDS = 360  # Includes compilation; the test binary gets five minutes.


def clean_rustfs_env():
    """Do not inherit unrelated RustFS or cloud credentials/settings."""
    env = {key: os.environ[key] for key in ("PATH", "HOME", "TMPDIR") if key in os.environ}
    env["RUSTFS_CONSOLE_ENABLE"] = "false"
    return env


def parse_version(output):
    match = re.search(r"(?im)^\s*rustfs(?:\s+version)?[\s:=]+v?(\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.+-]+)?)(?=\s|$)", output)
    if not match:
        raise ValueError(f"unrecognized RustFS --version output: {output!r}")
    return match.group(1)


def inspect_binary(binary, expected_version):
    path = Path(binary)
    if not path.is_absolute():
        raise ValueError("--rustfs-binary / RUSTFS_BINARY must be an absolute path")
    path = path.resolve(strict=True)
    if not path.is_file() or not os.access(path, os.X_OK):
        raise ValueError(f"RustFS binary is not an executable file: {path}")
    result = subprocess.run([str(path), "--version"], env=clean_rustfs_env(),
                            capture_output=True, text=True, timeout=15, check=True)
    output = (result.stdout + result.stderr).strip()
    version = parse_version(output)
    if version != expected_version:
        raise ValueError(f"RustFS version mismatch: expected {expected_version!r}, got {version!r}; refusing to start nodes")
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    return {
        "binary_path": str(path), "sha256": digest.hexdigest(),
        "expected_version": expected_version, "version": version,
        "version_output": output,
        "verified_at_utc": datetime.now(timezone.utc).isoformat(),
    }


def free_port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def stop_process(proc):
    """Terminate a whole launcher-owned process group, including test children."""
    try:
        if os.name == "posix":
            os.killpg(proc.pid, signal.SIGTERM)
        elif proc.poll() is None:
            proc.terminate()
    except ProcessLookupError:
        pass
    try:
        proc.wait(timeout=10)
    except subprocess.TimeoutExpired:
        if os.name == "posix":
            try:
                os.killpg(proc.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
        else:
            proc.kill()
        proc.wait(timeout=10)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--rustfs-binary", default=os.environ.get("RUSTFS_BINARY"),
                        help="absolute path to the release executable (or RUSTFS_BINARY); never resolved from PATH")
    parser.add_argument("--expect-version", default=DEFAULT_VERSION,
                        help="exact release version required before starting either node (default: 1.0.4)")
    parser.add_argument("--output-dir", type=Path,
                        help="directory for node logs and binary provenance (default: audit-results/rustfs-<version>)")
    args = parser.parse_args(argv)
    if not args.rustfs_binary:
        parser.error("provide --rustfs-binary or RUSTFS_BINARY; an installed PATH version is never used")
    if not re.fullmatch(r"\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.+-]+)?", args.expect_version):
        parser.error("--expect-version must be an exact semantic version")
    metadata = inspect_binary(args.rustfs_binary, args.expect_version)
    output = (args.output_dir or ROOT / "audit-results" / f"rustfs-{args.expect_version}").resolve()
    output.mkdir(parents=True, exist_ok=True)
    metadata_path = output / "rustfs-binary.json"
    metadata_path.write_text(json.dumps(metadata, indent=2) + "\n")
    print(f"Verified RustFS {metadata['version']}: {metadata['binary_path']}", flush=True)
    print(f"SHA256 {metadata['sha256']}; evidence: {metadata_path}", flush=True)

    processes, logs, endpoints = [], [], []
    deadline = time.monotonic() + TOTAL_TIMEOUT_SECONDS
    go_proc = None
    try:
        with tempfile.TemporaryDirectory(prefix="migration-audit-rustfs-") as base:
            try:
                for name in ("source", "target"):
                    data = Path(base) / name
                    data.mkdir()
                    port = free_port()
                    endpoint = f"http://127.0.0.1:{port}"
                    log = (output / f"rustfs-{name}.log").open("w")
                    logs.append(log)
                    proc = subprocess.Popen([
                        metadata["binary_path"], "server", "--address", f"127.0.0.1:{port}",
                        "--access-key", "audit-local", "--secret-key", "audit-local-secret", str(data),
                    ], cwd=base, env=clean_rustfs_env(), stdout=log,
                        stderr=subprocess.STDOUT, start_new_session=True)
                    processes.append(proc)
                    start_deadline = min(deadline, time.monotonic() + 45)
                    while True:
                        if proc.poll() is not None:
                            raise RuntimeError(f"{name} exited {proc.returncode}; see {log.name}")
                        try:
                            with socket.create_connection(("127.0.0.1", port), timeout=.2):
                                break
                        except OSError:
                            if time.monotonic() >= start_deadline:
                                raise RuntimeError(f"{name} did not start within 45 seconds; see {log.name}")
                            time.sleep(.2)
                    endpoints.append(endpoint)
                env = os.environ.copy()
                env.update(AUDIT_SOURCE=endpoints[0], AUDIT_TARGET=endpoints[1])
                command = ["go", "test", "-race", "-tags", "real_s3", "-run", "^TestRealS3",
                           "-count=1", "-timeout=5m", "-v", "./internal/migrator"]
                metadata.update(test_command=command, endpoints=endpoints)
                metadata_path.write_text(json.dumps(metadata, indent=2) + "\n")
                go_proc = subprocess.Popen(command, cwd=ROOT, env=env, start_new_session=True)
                try:
                    returncode = go_proc.wait(timeout=min(GO_TIMEOUT_SECONDS, max(1, deadline - time.monotonic())))
                except subprocess.TimeoutExpired:
                    stop_process(go_proc)
                    raise RuntimeError("real S3 tests exceeded the launcher deadline") from None
                metadata.update(test_exit_code=returncode, finished_at_utc=datetime.now(timezone.utc).isoformat())
                metadata_path.write_text(json.dumps(metadata, indent=2) + "\n")
                return returncode
            finally:
                if go_proc is not None and go_proc.poll() is None:
                    stop_process(go_proc)
                for proc in reversed(processes):
                    stop_process(proc)
    finally:
        for log in logs:
            log.close()


if __name__ == "__main__":
    def interrupted(signum, frame):
        raise KeyboardInterrupt

    signal.signal(signal.SIGTERM, interrupted)
    try:
        raise SystemExit(main())
    except KeyboardInterrupt:
        print("RustFS audit interrupted; owned processes were stopped", file=sys.stderr)
        raise SystemExit(130)
    except (ValueError, OSError, RuntimeError, subprocess.SubprocessError) as exc:
        print(f"RustFS audit failed: {exc}", file=sys.stderr)
        raise SystemExit(1)
