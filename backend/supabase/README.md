# Supabase Local Development

This directory holds the local Supabase stack configuration for the backend.

- CLI: `supabase` v2.x (installed as a dev dependency, run via `npx supabase`)
- Postgres version: 17 (see `config.toml`)
- Local database URL: `postgresql://postgres:postgres@127.0.0.1:54322/postgres` (see `../.env`)

All commands below are run from the project root (`backend/`), where the `supabase/` folder lives.

## Common commands

```bash
# Start the local stack (Postgres, Studio, etc.)
npx supabase start

# Check container health and status
npx supabase status

# Stop the stack (keeps data)
npx supabase stop

# Stop and remove everything (volumes, images)
npx supabase stop --no-backup
```

Useful `db` subcommands:

```bash
# Reset the local database: applies all migrations, then runs the seed
npx supabase db reset

# Apply pending migrations to the local database
npx supabase migration up

# Run a raw SQL query against the local database
npx supabase db query "select 1"
```

## Making migrations

There are two common workflows. Prefer writing migrations by hand for new
changes, and use `db diff` to capture schema changes you made directly in
Studio or via SQL.

### 1. Create an empty migration and edit it

```bash
npx supabase migration new my_change_name
```

This creates `supabase/migrations/<timestamp>_my_change_name.sql`. Add your SQL
(e.g. `CREATE TABLE`, `ALTER TABLE`, policies) to that file, then apply it:

```bash
npx supabase migration up
```

### 2. Diff your local schema into a migration

Make changes directly in the local database (Studio or `db query`), then:

```bash
npx supabase db diff -f my_change_name
```

This writes a new migration file matching your local schema changes. Review it,
then verify it applies cleanly:

```bash
npx supabase db reset
```

## Seed data

Seed data is loaded **after** migrations during `db reset`. It is sourced from
`supabase/seed.sql` (configured via `[db.seed].sql_paths` in `config.toml`,
enabled by default).

1. Edit `supabase/seed.sql` with idempotent SQL so re-running is safe
   (use `TRUNCATE ... CASCADE` or `ON CONFLICT DO NOTHING` before inserts).

2. Apply it:

```bash
npx supabase db reset
```

## Conventions

- One migration per logical change; do not edit migrations that have already
  been applied to a shared/remote environment.
- Keep `seed.sql` idempotent and deterministic.
- Never commit `../.env` (secrets); use `../.env.example` as the template.
