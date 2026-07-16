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
(for `RESTIC_FROM_*` support); `bootstrap` needs **0.17+**. Prefer the
official restic binaries — some distro packages (e.g. Debian/Ubuntu) are
built without cloud backends like Azure.

Upgrade later with `restic-duper self-update`: it downloads the latest
release for your platform, verifies it against the release's
`checksums.txt`, and atomically replaces the executable (use `sudo` if the
binary lives in a root-owned directory; `--check` reports without
installing).

## Quick start

```sh
restic-duper init            # writes an example restic-duper.yaml
$EDITOR restic-duper.yaml
restic-duper bootstrap       # initialize destination repos that don't exist yet
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
    # retention:                            # applied to the DESTINATION by "forget"
    #   keep_daily: 14
    #   keep_weekly: 8
    #   keep_monthly: 12
    #   # keep_last / keep_hourly / keep_yearly / keep_within: 30d
    #   # forget_args: ["--group-by", "host"]
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
- Destination repositories must exist before `run` will copy into them —
  `run` never creates repositories. Use `restic-duper bootstrap` once to
  initialize missing destinations: it runs
  `restic init --copy-chunker-params --from-repo <source>` so copied
  snapshots deduplicate against future direct backups, and it only creates a
  repository when restic specifically reports "repository does not exist"
  (exit code 10, restic ≥ 0.17) — never on wrong-password, network, or other
  ambiguous errors, so a typo can't silently fork your backups to a new
  location.

## Commands

| Command | Purpose |
|---|---|
| `restic-duper run` | Copy snapshots for every pair (or `--pair name`, `--dry-run`) |
| `restic-duper status` (alias `verify`) | Per pair: snapshot counts, last backup time, size, and whether the destination has the source's latest snapshot |
| `restic-duper forget` | Apply each pair's `retention` policy to its destination and prune (`--dry-run`, `--no-prune`) |
| `restic-duper bootstrap` | Initialize destination repos that don't exist yet (`--pair` to limit) |
| `restic-duper check` | Validate the config; `--connect` also probes every repository |
| `restic-duper init [path]` | Write an example config |
| `restic-duper self-update` | Replace this binary with the latest GitHub release (`--check` to only look) |

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
`pairs`. Method-preserving redirects (307/308) are followed, up to five
hops. Method-changing redirects (301/302/303) are treated as delivery
failures — following them would turn the POST into a bodyless GET — and the
error message includes the redirect target so you can update `url` to the
final address.

### Event-ingest APIs (Axiom, etc.)

Set `format: events` to post a JSON **array** with one flat event per pair
instead of the single run object — the shape event-ingest APIs like
[Axiom](https://axiom.co) expect. Each event carries `_time` (RFC3339),
`level` (`info`/`error`) for severity highlighting, `status`, `pair`,
`from_repo`/`to_repo` (credentials redacted), `duration_seconds`, and the
snapshot counters. Use `on_success: true` so healthy runs are ingested too.

```yaml
notifications:
  webhook:
    # Axiom's API ingest path is /v1/datasets/<dataset>/ingest.
    # (Do NOT use app.axiom.co — that's the web UI; it redirects POSTs to a
    # login page that answers 200, which looks like a successful delivery.)
    url: https://api.axiom.co/v1/datasets/backup-events/ingest
    format: events
    headers:
      Authorization: Bearer ${AXIOM_INGEST_TOKEN}
    on_success: true
    on_failure: true
```

```json
[
  {
    "_time": "2026-07-15T20:45:00Z",
    "service": "restic-duper",
    "version": "0.4.0",
    "command": "run",
    "host": "prod-01",
    "pair": "main-to-offsite",
    "from_repo": "/srv/restic/main",
    "to_repo": "azure:backups:/main",
    "status": "success",
    "level": "info",
    "duration_seconds": 174.2,
    "snapshots_copied": 1,
    "snapshots_skipped": 0
  }
]
```

A setup failure that prevented any pair from running is sent as a single
run-level event with `level: "error"` and the error message.

### Simple JSON receivers

The default `format: payload` works with services that accept arbitrary JSON
POSTs (ntfy, or Discord/Slack via a small relay). For **healthchecks.io**-style dead-man's
switches, use the base ping URL with `on_success: true` and
`on_failure: false`: the check then alerts both when runs fail *and* when
they stop happening entirely. (Don't point failure notifications at a plain
ping URL — any POST registers as a success ping there.)

## Verifying replication

`restic-duper status` (alias: `verify`) inspects both sides of every pair —
read-only, no locks — and reports snapshot count, most recent snapshot time,
and repository size. It also checks that the destination contains a copy of
the source's **latest** snapshot (via the `original` field restic copy
records), and exits `2` if any repository is unreachable or any pair is
behind, so it works as a scheduled verification step. `--json` emits the
report as JSON on stdout.

```
PAIR   SIDE    REPO                  SNAPSHOTS  LATEST                     SIZE      STATE
test1  source  /srv/restic/test1     42         2026-07-15 06:00 (3h ago)  11.3 GiB
       dest    azure:backups:/test1  42         2026-07-15 06:14 (3h ago)  11.3 GiB  in sync
```

## Retention

Pairs with a `retention` block can have their **destination** pruned with
`restic-duper forget` (the source is never touched — its retention belongs
to whatever creates its backups). Runs `restic forget --prune` with the
pair's `keep_*` policy; `--dry-run` previews, `--no-prune` skips the space
reclamation. Failures notify the webhook like `run` does (payload
`command: "forget"`). Note prune takes exclusive repository locks — schedule
it when no copy is running.

## Scheduling

restic-duper is log-based (no TUI) so it drops straight into cron or a
systemd timer:

```
# /etc/cron.d/restic-duper
0 6 * * *  backup /usr/local/bin/restic-duper run    -c /etc/restic-duper/config.yaml
0 8 * * *  backup /usr/local/bin/restic-duper status -c /etc/restic-duper/config.yaml -q
0 3 * * 0  backup /usr/local/bin/restic-duper forget -c /etc/restic-duper/config.yaml
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
