"""Run the CLI against two disposable S3 servers. Requires Docker, Go and boto3."""
import json
import os
import subprocess
import tempfile
import time
from concurrent.futures import ThreadPoolExecutor
from contextlib import contextmanager
from pathlib import Path
from urllib.request import urlopen

import boto3
from botocore.config import Config

ROOT = Path(__file__).resolve().parents[1]
MIB = 1024 * 1024
MINIO = "cgr.dev/chainguard/minio@sha256:3b6e40c30ecd5cea627e382ff0b82ca841f58c52294578787043ee80c8ef7c82"
RUSTFS = "ghcr.io/rustfs/rustfs:1.0.4@sha256:ed9210d6411d77ffceb70d94b964a3cb7d225983368bc25bebe01e995293dcc2"


@contextmanager
def server(image, access, secret, health, env_names, *args):
    container = subprocess.check_output([
        "docker", "run", "-d", "-p", "127.0.0.1::9000",
        "-e", f"{env_names[0]}={access}", "-e", f"{env_names[1]}={secret}", image, *args,
    ], text=True).strip()
    try:
        address = subprocess.check_output(["docker", "port", container, "9000/tcp"], text=True).strip()
        endpoint = "http://" + address
        for _ in range(60):
            try:
                with urlopen(endpoint + health, timeout=2):
                    break
            except OSError:
                time.sleep(1)
        else:
            raise RuntimeError(f"{image} did not become ready")
        client = boto3.client(
            "s3", endpoint_url=endpoint, aws_access_key_id=access, aws_secret_access_key=secret,
            region_name="us-east-1", config=Config(s3={"addressing_style": "path"}),
        )
        yield client, {"endpoint": endpoint, "access_key": access, "secret_key": secret}
    except Exception:
        subprocess.run(["docker", "logs", "--tail", "40", container], check=False)
        raise
    finally:
        subprocess.run(["docker", "rm", "-fv", container], check=True, stdout=subprocess.DEVNULL)


def exercise(binary, directory, old, new):
    src, src_config = old
    dst, dst_config = new
    config = {
        "old": src_config, "new": dst_config, "workers": 2,
        "source_frozen": True,
        "multipart_threshold": 5 * MIB, "multipart_part_size": 5 * MIB,
        "multipart_concurrency": 2,
    }
    (directory / "config.yaml").write_text(json.dumps(config), encoding="utf-8")

    def cli(*args, exit_code=0):
        # Ignore the developer's OLD_*, NEW_* and state-path environment overrides.
        print(f"$ migration {' '.join(args)} (expect exit {exit_code})", flush=True)
        result = subprocess.run([binary, *args], cwd=directory, env={"PATH": os.environ["PATH"]},
                                capture_output=True, text=True, timeout=90)
        print(result.stdout, result.stderr, sep="", flush=True)
        assert result.returncode == exit_code, f"{args}: exit {result.returncode}, expected {exit_code}"
        return result.stdout

    def assert_status(*expected):
        rows = [line.split() for line in cli("status").splitlines()]
        row = next(row for row in rows if row[:1] == [bucket])
        assert tuple(map(int, row[1:])) == expected, row

    bucket = "migration-e2e"
    empty_bucket = "migration-empty"
    src.create_bucket(Bucket=bucket)
    src.create_bucket(Bucket=empty_bucket)
    dst.create_bucket(Bucket=bucket)
    dst.put_bucket_versioning(Bucket=bucket, VersioningConfiguration={"Status": "Enabled"})
    # Distinct, unequal-length parts catch swapped ranges as well as truncated transfers.
    bodies = {"empty": b"", "photos/样本 1+%.txt": b"small object\n",
              "large.bin": b"A" * (5 * MIB) + b"B" * (2 * MIB + 17)}
    attrs = {"ContentType": "application/octet-stream", "CacheControl": "max-age=120",
             "Metadata": {"source": "e2e"}}
    for key, body in bodies.items():
        src.put_object(Bucket=bucket, Key=key, Body=body, **attrs)

    assert empty_bucket in cli("list")
    cli("scan")
    # The flag is only an assertion; rejecting a false assertion prevents writes.
    cli("migrate", "--source-frozen=false", exit_code=1)
    assert not dst.list_objects_v2(Bucket=bucket).get("Contents"), "freeze guard wrote objects"
    assert empty_bucket not in {item["Name"] for item in dst.list_buckets()["Buckets"]}, "freeze guard created a bucket"
    cli("migrate")
    dst.head_bucket(Bucket=empty_bucket)
    for key, expected in bodies.items():
        got = dst.get_object(Bucket=bucket, Key=key)
        with got["Body"] as body:
            assert body.read() == expected, f"incorrect bytes for {key!r}"
        for field, value in attrs.items():
            assert got[field] == value, f"{key!r}: incorrect {field}: {got[field]!r}"
    # ETags are opaque; exact bytes and metadata above are the correctness test.
    assert dst.head_object(Bucket=bucket, Key="large.bin")["ETag"]
    initial_diff = cli("diff")
    assert "ok=3" in initial_diff and "content_mismatch=0" in initial_diff, initial_diff

    def versions():
        return sorted((v["Key"], v["VersionId"]) for v in
                      dst.list_object_versions(Bucket=bucket)["Versions"])

    initial_versions = versions()
    cli("run")
    assert versions() == initial_versions, "resume wrote new versions of completed objects"

    # A source update after scan must fail the old inventory without creating a target.
    src.put_object(Bucket=bucket, Key="changed", Body=b"first")
    cli("scan")
    src.put_object(Bucket=bucket, Key="changed", Body=b"later")
    cli("migrate", exit_code=1)
    assert versions() == initial_versions, "copied a different source revision than the one scanned"
    cli("run")
    with dst.get_object(Bucket=bucket, Key="changed")["Body"] as body:
        assert body.read() == b"later", "rescan did not retry the changed object"

    # Both objects have identical explicit metadata and size, so diff must
    # compare their bytes rather than reporting a server-default header mismatch.
    src.put_object(Bucket=bucket, Key="conflict", Body=b"source", **attrs)
    dst.put_object(Bucket=bucket, Key="conflict", Body=b"target", **attrs)
    for client in (src, dst):
        head = client.head_object(Bucket=bucket, Key="conflict")
        for field, value in attrs.items():
            assert head[field] == value, f"conflict: incorrect {field}: {head.get(field)!r}"
    dst.put_object(Bucket=bucket, Key="extra", Body=b"target only")
    before_conflict = versions()
    cli("run")
    cli("migrate", "--strict", exit_code=1)
    assert_status(5, 0, 4, 1, 0, 0)
    assert versions() == before_conflict, "conflict or target-only object was overwritten"
    for key, expected in {"conflict": b"target", "extra": b"target only"}.items():
        with dst.get_object(Bucket=bucket, Key=key)["Body"] as body:
            assert body.read() == expected, key
    # Wait for the live upload ID, so the application write lands between HEAD and Complete.
    race_key = "race.bin"
    src.copy_object(Bucket=bucket, Key=race_key, CopySource={"Bucket": bucket, "Key": "large.bin"})
    cli("scan")
    with ThreadPoolExecutor(max_workers=1) as pool:
        migration = pool.submit(cli, "migrate", "--rate-limit", "1MiB")
        deadline = time.monotonic() + 30
        while time.monotonic() < deadline:
            uploads = dst.list_multipart_uploads(Bucket=bucket).get("Uploads", [])
            if any(upload["Key"] == race_key for upload in uploads):
                break
            if migration.done():
                migration.result()
                raise AssertionError("migration finished before a multipart upload was visible")
            time.sleep(0.1)
        else:
            raise AssertionError("timed out waiting for multipart upload")
        winner = dst.put_object(Bucket=bucket, Key=race_key, Body=b"live application write")
        migration.result()
    assert versions() == sorted(before_conflict + [(race_key, winner["VersionId"])]), versions()
    with dst.get_object(Bucket=bucket, Key=race_key)["Body"] as body:
        assert body.read() == b"live application write"
    assert not dst.list_multipart_uploads(Bucket=bucket).get("Uploads")
    assert_status(6, 0, 4, 2, 0, 0)

    report = directory / "issues.jsonl"
    gaps = cli("diff", "--strict", "--issues-jsonl", str(report), exit_code=1)
    assert "content_mismatch=1" in gaps and "size_mismatch=1" in gaps and "extra_on_target=1" in gaps, gaps
    issues = [json.loads(line) for line in report.read_text(encoding="utf-8").splitlines()]
    assert {(issue["bucket"], issue["key"], issue["kind"]) for issue in issues} == {
        (bucket, "conflict", "content"), (bucket, race_key, "size"), (bucket, "extra", "extra")
    }, issues
    # Explicitly breaking the source freeze above is adversarial fault injection;
    # a production migration must keep the source frozen through final acceptance.


def main():
    with tempfile.TemporaryDirectory() as temp:
        directory = Path(temp)
        binary = directory / "migration"
        subprocess.run(["go", "build", "-o", binary, "."], cwd=ROOT, check=True,
                       env={**os.environ, "CGO_ENABLED": "0"})
        with server(MINIO, "source-user", "source-secret",
                    "/minio/health/ready", ("MINIO_ROOT_USER", "MINIO_ROOT_PASSWORD"), "server", "/tmp/data") as old:
            with server(RUSTFS, "target-user", "target-secret", "/health/ready",
                        ("RUSTFS_ACCESS_KEY", "RUSTFS_SECRET_KEY")) as new:
                exercise(binary, directory, old, new)
    print("MinIO to RustFS end-to-end test passed.")


if __name__ == "__main__":
    main()
