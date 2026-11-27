# Isolated Upgrade: 2026.11.27-1

This release moves Longdao to an isolated application, PostgreSQL, Redis, and
private-file environment. It does not migrate or restart the SEP application.

- Application code: `ef79689f`, version `2026.11.27-1`.
- Candidate directory: `/www/longdao/environments/2026.11.27-1`.
- Application: `longdao-ai-next`; database: `longdao-ai-postgres-next`;
  Redis: `longdao-ai-redis-next`.
- Original green application and original database/Redis remain available for
  recovery. Do not start both applications against formal business traffic.
- Rehearsal: `/www/longdao/upgrades/20261127-rehearsal`.
- Final stopped-writer snapshot: `/www/longdao/upgrades/20261127-final`.
- Live application port: `172.19.0.1:18082`; neither database nor Redis publishes
  a host port. The data network is internal. Application egress is added only
  after isolated verification.

## Verification

The production backup was restored with the existing PostgreSQL 18 image and all
18 pending migration files were applied to the candidate only. Business-record
hashes were compared against an independent restore of the same backup, including
users, keys, groups, accounts, payments, subscriptions, usage and billing dedup.
Only API-key access timestamps touched by read-only authentication smoke checks
were excluded from those hashes. Model prices were separately compared offline
for standard, priority, flex, cache, long-context and image-tier calculations.

104 model tariffs were derived from existing effective legacy rates. These are
migration tariffs, not asserted supplier procurement quotes; discount and markup
are both 1 to avoid applying a second markup. The 17 missing legacy tariffs are
listed in the private price-import report and are not invented by the importer.
The existing group/user multipliers remain unchanged. Account 52 is explicitly
configured for `naoshengyun_gpt_async` in the new database only.

Redis restoration starts with AOF disabled so that the RDB is loaded. Media-task
values and absolute expiry times are compared, then AOF is enabled and its initial
rewrite is awaited. Original signing credentials and reference-file paths inside
the containers are retained. No paid generation call is part of these checks.

## Operation

The independent systemd unit is `longdao-isolated-upgrade-20261127` with a 20-minute
runtime limit and a recovery post-stop action. Private execution logs and phase
state are stored in the candidate directory. New formal requests receive HTTP
503 with `Retry-After` while maintenance is active. Existing request slots must
drain within five minutes or the maintenance gate is reverted.

The original application is stopped cleanly before the final backup. That backup
is restored, migrated and priced in the new database. Balances and record totals
are verified again before the private runtime is copied and the candidate starts.
Main-site and reference-image routing are switched together. SEP process identity
is checked for continuity.

Automatic recovery is allowed only before the `opening`/`live` phase. It restarts
the original application and restores its original routing/configuration, without
restoring or modifying the original database. Once production writes are open,
stale-database rollback is refused; new records must first be preserved and
reconciled. The original environment should be retained for at least seven days.

Inspect the phase and log before taking recovery action. Administrative recovery
before the write-open boundary is:

```sh
python3 /www/longdao/deploy/isolated_upgrade.py rollback
```

The previous shared-database release script is incompatible with this topology.
Do not run it or the old unqualified Compose wrapper after the isolated upgrade.
Use the candidate Compose with project name `longdao-upgrade` for inspection and
service maintenance; prepare subsequent releases against the active environment,
not the retained original database. The active topology is recorded in
`/www/longdao/deploy/isolated-active.json`.
