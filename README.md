# restic-duper

Replicate [restic](https://restic.net/) repositories with `restic copy` — a
simple, cron-friendly way to maintain **offsite or redundant copies** of your
backups.

You give it a list of repository pairs; for each pair it copies the latest
(or all) snapshots from the source repository into the destination repository.
Any backend restic supports works on either side: local paths, sftp, S3, B2,
Azure, GCS, rest-server, rclone, …

Because `restic copy` re-encrypts data with the destination repository's key,
your offsite copy does not share keys with the primary — losing one password
does not compromise the other.

## Install

Download a binary from the [releases page](https://github.com/jclement/restic-duper/releases), or:

```sh
go install github.com/jclement/restic-duper@latest
```

Requires `restic` **0.15.0 or newer** on the machine running restic-duper
(for `RESTIC_FROM_*` support).

## Quick start

```sh
restic-duper init            # writes an example restic-duper.yaml
$EDITOR restic-duper.yaml
restic-duper check --connect # validate config and reach every repository
restic-duper run
```

## Configuration

restic-duper looks for `./restic-duper.yaml`,
`~/.config/restic-duper/config.yaml`, then `/etc/restic-duper/config.yaml`,
or use `--config`.

```yaml
# Optional: path to the restic binary (default: "restic" from PATH)
# restic_binary: /usr/local/bin/restic

notifications:
  webhook:
    url: https://example.com/hooks/restic-duper
    # method: POST                          # default
    # headers:
    #   Authorization: Bearer ${WEBHOOK_TOKEN}
    on_failure: true                        # default
    on_success: false                       # default
    # timeout: 30s

pairs:
  - name: main-to-offsite
    from:
      repo: /srv/restic/main
      password_file: /etc/restic/main.pass
    to:
      repo: s3:s3.us-east-1.amazonaws.com/my-offsite-bucket/restic
      password: ${OFFSITE_RESTIC_PASSWORD}
      env:
        AWS_ACCESS_KEY_ID: ${OFFSITE_AWS_KEY}
        AWS_SECRET_ACCESS_KEY: ${OFFSITE_AWS_SECRET}
    # snapshots: latest                     # "latest" (default) or "all"
    # copy_args: ["--host", "myserver"]     # extra args passed to restic copy
    # timeout: 6h
    # allow_empty: false                    # zero matched snapshots = failure (default)
```

Notes:

- `${VAR}` references in config values are expanded from the environment at
  load time. Expansion happens after YAML parsing, so secrets containing
  `#`, `:`, `$`, or newlines are always taken literally (bare `$VAR` is left
  alone too). Referencing an unset variable is an error; `${VAR}` inside
  comments is ignored.
- Each repo needs exactly one of `password`, `password_file`, or
  `password_command`. Passwords are passed to restic via environment
  variables, never on the command line.
- `env` sets backend credentials (AWS keys, `B2_ACCOUNT_ID`, …) for that
  side. Since `restic copy` is a single process with a single environment,
  **the same variable cannot hold different values for the two sides** —
  restic-duper rejects such configs at load time. If both sides use the same
  backend type with different credentials, use restic's
  [rclone backend](https://restic.readthedocs.io/en/stable/030_preparing_a_new_repo.html#other-services-via-rclone)
  for one of them.
- `snapshots: latest` copies the single most recent snapshot (as resolved by
  restic, honoring any `--host`/`--path`/`--tag` filters you add in
  `copy_args`). If your source repo holds backups from several hosts and you
  want the latest of each, either add one pair per host with a `--host`
  filter, or use `snapshots: all`.
- **A run that copies nothing is treated as a failure.** If restic's
  snapshot filter matches zero snapshots (empty source repo, or a `--host`
  filter that no longer matches), restic itself exits 0 — but for a
  replication tool that almost always means the primary backup is broken, so
  restic-duper fails the pair and notifies. Set `allow_empty: true` on a
  pair if zero matches is genuinely expected.
- Ambient `RESTIC_*` repository/credential variables in the parent
  environment are scrubbed before invoking restic, so a stray
  `RESTIC_PASSWORD_FILE` in your shell can't override the pair's configured
  credentials. Cache and backend variables pass through.
- Destination repositories must already be initialized (`restic init`). For
  deduplication across copies, consider initializing the destination with
  `restic init --copy-chunker-params --from-repo <source>`.

## Commands

| Command | Purpose |
|---|---|
| `restic-duper run` | Copy snapshots for every pair (or `--pair name`, `--dry-run`) |
| `restic-duper check` | Validate the config; `--connect` also probes every repository |
| `restic-duper init [path]` | Write an example config |

Global flags: `--config/-c`, `--json` (structured logs), `--quiet/-q`,
`--verbose/-v` (streams full restic output).

Pairs run sequentially. Exit code is `0` on success, `2` if any pair failed,
`1` on config or setup errors.

## Failure notifications

When a run finishes, restic-duper can POST a JSON payload to a webhook —
by default only when something failed. Delivery is retried three times.

```json
{
  "tool": "restic-duper",
  "version": "1.0.0",
  "host": "backup01",
  "status": "failure",
  "started_at": "2026-07-15T06:00:00Z",
  "finished_at": "2026-07-15T06:14:02Z",
  "pairs": [
    {
      "name": "main-to-offsite",
      "status": "failure",
      "error": "exit status 1: Fatal: unable to open repository ...",
      "duration_seconds": 12.4,
      "snapshots_copied": 0,
      "snapshots_skipped": 0
    }
  ]
}
```

Setup failures that prevent any pair from running (bad pair name, missing
restic binary) are reported too, with a top-level `error` field instead of
`pairs`. Redirects are treated as delivery failures — point the webhook at
the final URL.

This works directly with services that accept arbitrary JSON POSTs (ntfy, or
Discord/Slack via a small relay). For **healthchecks.io**-style dead-man's
switches, use the base ping URL with `on_success: true` and
`on_failure: false`: the check then alerts both when runs fail *and* when
they stop happening entirely. (Don't point failure notifications at a plain
ping URL — any POST registers as a success ping there.)

## Scheduling

restic-duper is log-based (no TUI) so it drops straight into cron or a
systemd timer:

```
# /etc/cron.d/restic-duper — offsite copy at 06:00 daily
0 6 * * * backup /usr/local/bin/restic-duper run -c /etc/restic-duper/config.yaml
```

## Security notes

- Keep the config file `chmod 600` if it contains inline passwords
  (restic-duper warns otherwise). Prefer `password_file` or
  `password_command`, or inject secrets via `${VAR}`.
- restic-duper never prints passwords; they are handed to restic only through
  its documented environment variables, and credentials embedded in
  repository URLs (`rest:https://user:pass@…`) are redacted from logs.
- Auto-discovered config files (found via the search path rather than
  `--config`) must be owned by you or root and not world-writable —
  `password_command` executes shell commands, so a planted config would be
  code execution.
- On Ctrl-C/SIGTERM or a pair `timeout`, restic receives SIGINT and gets 30
  seconds to shut down cleanly and release its repository locks before being
  killed. If a run is ever hard-killed, `restic unlock` clears stale locks.

## License

MIT
