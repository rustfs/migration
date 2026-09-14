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

Safety, state machine, tuning and troubleshooting: see the Chinese manual and `docs/DESIGN.md`.
