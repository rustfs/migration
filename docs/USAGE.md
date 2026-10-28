# migration — English usage (short)

The full operator manual is in Chinese: [使用手册.md](./使用手册.md).

```bash
cp config.example.yaml config.yaml
./bin/migration --help
./bin/migration run  -c config.yaml
./bin/migration diff -c config.yaml
```

- Empty `bucket:` migrates every source bucket.
- `--bucket NAME` pins one bucket.
- `run` = scan + copy. `scan` / `migrate` stay available separately.
- `diff` HEADs the target for every source key; exit 1 if anything is missing or mismatched.
- Never overwrites. Never deletes. Re-run after Ctrl-C.
- A target object that already matches the inventory (same size; same ETag when both are present and neither is a multipart ETag `hash-N`) is marked `done`, not `conflict`. A different size or comparable ETag is still a conflict and is never overwritten. A Ctrl-C after a successful PUT therefore resumes as `done` instead of a false conflict.

Safety, state machine, tuning and troubleshooting: see the Chinese manual and `docs/DESIGN.md`.
