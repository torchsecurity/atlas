# Community-Edition Postgres View Support

Fork of [ariga/atlas](https://github.com/ariga/atlas) (Apache-2.0). Goal: make
`atlas migrate diff --env local` work, logged out, on Torch's `deployment/db`
schema, which contains 13 plain Postgres views (`view` HCL blocks with typed
columns, `as = <<-SQL` heredocs, and explicit `depends_on` including
view-on-view references).

## Provenance

The upstream OSS tree is a mechanically stripped mirror of Ariga's internal
repo. Commit `bd9f7111` ("all: remove all _ent usage in oss") deleted the
`*_oss.go` build-tag halves and re-emitted them without view/func/trigger
scaffolding. The parent commit `bd9f7111^` therefore contains the complete
shared-layer view code, published under this repo's Apache-2.0 license. We
restore that code (retaining copyright headers) and author only what was never
published:

1. Postgres view **inspection** (queries over `pg_catalog`).
2. Postgres view **DDL building** (`CREATE VIEW` / `DROP VIEW`).
3. `sortViewChanges` **topological ordering** (was a stub even pre-strip).

Everything else is hunk-porting from `git show bd9f7111^:<path>` onto current
master (master has drifted since March 2026 - port hunks, never revert whole
files).

## Scope cuts

- Postgres only. Plain views only: `materialized` blocks parse in shared
  layers (restored verbatim) but the postgres driver returns an explicit
  "materialized views are not supported by this version" error.
- No view indexes/triggers, no `check_option` behavior beyond parsing.
- `ModifyView` plans as `DROP VIEW` + `CREATE VIEW` (views hold no data; this
  sidesteps `CREATE OR REPLACE`'s column-shape restrictions).
- First-class `Schema.Views` shape everywhere (NOT the generic
  `AddObject`/`DropObject` shortcut - mixing shapes double-registers views).

## Architecture (file map)

Layer 1 - shared restoration (worker 1), blueprint = `bd9f7111^`:
- `sql/schema/schema.go`: View fields (`Indexes`, `Triggers`, `Refs`),
  `Schema.View()`, `Schema.Materialized()`, `View.Column/Pos/SetPos/AsTable/
  Materialized/SetMaterialized`, `ViewCheckOption` attrs.
- `sql/schema/migrate.go`: `AddView`/`DropView`/`ModifyView`/`RenameView` +
  `change()` impls.
- `sql/schema/dsl.go`: `NewMaterializedView`, View DSL methods; fix the
  `View.AddDeps`/`addRefs` asymmetry per pre-strip source.
- `sql/schema/exclude.go`: `typeV` selector, view handling in `excludeS`.
- `sql/sqlspec/sqlspec.go`: `View` spec struct, `Register("view")`,
  `Register("materialized")`.
- `sql/internal/specutil/convert.go` + `spec.go`: `View()`, `FromView()`,
  `ScanDoc.Views/Materialized`, `ScanFuncs.View`, `SchemaFuncs.View`,
  `ViewSpecRef`, view arms in `fromDependsOn`/`dependsOn`.
- `sql/internal/sqlx/diff.go`: view loops in `schemaDiff`/`RealmDiff`,
  `viewDiff`, `viewDefChanged` (uses existing `BodyDefChanged`),
  `columnDiffV`, `indexDiffV`, `addViewChange`, `findView`,
  `DiffDriver.ViewAttrChanges`.
- `sql/internal/sqlx/plan.go`: view bucket in `SortChanges`, `SameView`,
  view cases in `depOfAdd`/`depOfDrop`; `dependsOn` view cases in `sqlx.go`.
- `sql/internal/sqlx/dev.go`: view loops in `NormalizeRealm`/
  `NormalizeSchema`, `key2pos.putView`, `keyV`. **This is the linchpin**: the
  dev-db round trip is what canonicalizes desired view defs (apply to dev DB,
  read back `pg_get_viewdef`), making string comparison drift-free.
- `sql/internal/sqlx/sqlx.go`: `Builder.View()`, `Builder.ViewResource()`.
- `cmd/atlas/internal/cmdapi/project.go`: `add_view`/`drop_view`/
  `modify_view`/`rename_view` in `SkipChanges`.

Layer 2 - postgres HCL codec (worker 2), blueprint =
`bd9f7111^:sql/postgres/sqlspec_oss.go` + `driver_oss.go`:
- `doc.Views`/`doc.Materialized` fields + `merge`/`ScanDoc`.
- `convertView` / `viewSpec`, codec options `WithTypes("view.column.type")`,
  `WithTypes("materialized.column.type")`,
  `WithScopedEnums("view.check_option", ...)`.
- `specFuncs.View = viewSpec`, `scanFuncs.View = convertView`.
- Extend `convertTypes` (driver.go:772) to resolve `enum.*` column type refs
  in views, not just tables.
- `QualifyObjects`/`QualifyReferences` for view specs in `MarshalSpec`.

Layer 3 - postgres driver (worker 3), authored fresh:
- `inspect.go`: `inspectViews` - `pg_class` `relkind = 'v'` joined to
  `pg_namespace`, defs via `pg_get_viewdef(oid)`, columns via
  `information_schema.columns` (separate pass; do not overload `addColumn`'s
  table lookup), comments via `obj_description`. Hook into `InspectRealm` /
  `InspectSchema` behind `mode.Is(schema.InspectViews)` exactly as
  `bd9f7111^:sql/postgres/inspect_oss.go:66-68` did. Dependency discovery via
  `pg_depend`/`pg_rewrite` → populate `View.Deps` (needed for drop ordering).
- `migrate.go`: `case *schema.AddView / DropView / ModifyView` in `plan()`,
  `RenameView` in `topLevel`; builders `addView`/`dropView`/`modifyView`
  (modify = drop + create). SQLite idiom reference:
  `bd9f7111^:sql/sqlite/migrate.go:94-100`.
- `sortViewChanges` (sqlx): topological sort by `View.Deps` - creates in
  dependency order, drops in reverse. Implement fresh (pre-strip OSS had only
  a stub).
- `ViewAttrChanges` on the postgres `diff` type (comment changes only for v1).

## Acceptance gates

1. `go build ./...` in root module and `cmd/atlas`; existing test suites in
   touched packages stay green.
2. Logged out, on a copy of Torch `deployment/db` (13 views restored, no
   `atlas { cloud }` block): `atlas migrate diff --env local` →
   "The migration directory is synced with the desired state".
3. Scenario tests (each from the synced state, logged out):
   - add a new view → migration contains only `CREATE VIEW`;
   - edit a view's SQL → `DROP VIEW` + `CREATE VIEW` (or equivalent);
   - remove a view → `DROP VIEW`;
   - add a column to a table + reference in a dependent view → correct
     ordering (ALTER TABLE before view recreation);
   - whitespace-only reformat of a view's `as` SQL → **no diff** (proves
     dev-db canonicalization works).
4. `check_atlas_valid_and_no_diff.sh` passes with the forked binary.

## Known limitations (v1)

- Materialized views: parse-only; the postgres driver errors on plan/inspect
  paths (never silently half-works).
- `SchemaDiff` (single-schema scope) cannot recreate dependents living in
  other schemas; realm-level diff handles cross-schema closures. Torch is
  single-schema (`public`), so unaffected.
- Dependent recreation seeds only from view drops. A dropped/renamed *table*
  breaking a view keeps upstream behavior (no expansion).
- A base-view change recreates its whole dependent closure in one migration
  (correct but large; e.g. touching people_stateful_current recreates 5
  dependents).
- `RenameView` is planned but not order-controlled (harmless in Postgres:
  dependents track OIDs, not names).
- CockroachDB view inspection untested; may fail rather than silently skip.
- Cosmetic: adding/removing a declared view column without a def change
  produces no diff on its own (in practice the def always changes too, since
  defs are canonicalized through the dev database).

# `migrate rebase`

A second, unrelated fork addition. `atlas migrate apply` refuses a directory
whose files were not applied in order ("migration file ... was added out of
order"), and the prescribed fix - `atlas migrate rebase` - is one of the
commands the community build replaces with an abort stub. The stub is removed
and the command implemented here.

## Provenance

Unlike the view code, **nothing of this command was ever published**: it has no
pre-strip source in the upstream history, only a contract in the docs that were
themselves stripped later. `git show 22f2cea6^:doc/md/reference.md` (the
`atlas migrate rebase` entry) pins the usage line, the short description, the
examples and the flag set (`--dir`, `--dir-format` only); `atlasexec`'s
`MigrateRebaseParams` corroborates the argument shape. The implementation
behind it is authored, not restored, and the doc'd surface is reproduced
verbatim so the fork stays a drop-in for the official binary.

## Semantics

`atlas migrate rebase [flags] {name | version}...`, in
`cmd/atlas/internal/cmdapi/migrate.go`:

- Each argument selects one file, matched against `File.Name()` or
  `File.Version()` - i.e. `20060102150405` or `20060102150405_name.sql`.
  An argument matching nothing, and two arguments resolving to the same file,
  are errors; nothing is renamed before every argument resolves.
- The selected files are renamed to consecutive versions, one second apart,
  keeping their relative order and their descriptions
  (`<version>_<desc>.sql`, or `<version>.sql` for a file without one). File
  contents are never touched.
- The first new version is `migrate.NewVersion()` (now, UTC), unless the
  directory already holds a greater version - then it is the greatest
  remaining version plus one second, so the result sorts after every file that
  keeps its version even when the clock is behind. Versions are always stepped
  by parsing and adding a second, never by arithmetic on the digits.
- A rename that fails mid-way reverts the renames already done (best-effort)
  and reports the original error.
- `atlas.sum` is recomputed and rewritten last, exactly as `migrate hash`
  does. Success is silent, like `migrate hash` and `migrate new`.

## Deliberate restrictions

- **Atlas-format local directories only.** The dir is type-asserted to
  `*migrate.LocalDir`; goose/flyway/dbmate/liquibase directories are rejected
  with `'migrate rebase' supports only atlas directories, but got: %T`, as
  `migrate new --edit` rejects them. Their naming schemes encode versions
  differently, and a rename that guesses is worse than a refusal.
- **No checkpoint files.** A checkpoint states the schema as of its own
  version; moving it to the end of the directory silently changes what it
  checkpoints. Detected through the exported `migrate.CheckpointFile`
  interface, not through file naming.
- **A valid `atlas.sum` is required** (`checkDir(cmd, dirURL, false)` in
  `PreRunE`). Out-of-order files are an apply-time problem, not a checksum
  one, so the sum must be correct before the command rewrites it - otherwise
  a rebase would launder an unrelated, unnoticed edit into a fresh sum.

# Update check and version links

A third fork addition, unrelated to the two above and authored in full. The
community build inherited upstream's update notifier: every command reported
the running version to `vercheck.ariga.io`, and `atlas version` linked the
release notes and the install instructions of upstream's releases. A build of
this fork must never report itself upstream, and the releases it was offered
are not the ones it is built from.

## The flow

`cmd/atlas/main.go` runs the check once per command, around
`cmdapi.Root.ExecuteContext`: `checkForUpdate` returns a closure whose result
is printed to stderr after the command finished - in the background with a
500ms grace period on a TTY, synchronously otherwise. It is skipped when
`ATLAS_NO_UPDATE_NOTIFIER` is set to a non-empty value, as before, and now
also when the running version is not a release of this fork
(`cmdapi.IsForkVersion`, i.e. a semver whose pre-release part starts with
`-views-ce`). That gate subsumes upstream's "dev mode" skip: a development
build has no version stamped at all, and a build stamped with an upstream
version would be compared against the wrong repository's releases.

`vercheck.VerChecker.Check` is what changed underneath: one unauthenticated
`GET https://api.github.com/repos/torchsecurity/atlas/releases/latest` with a
3s client timeout, reading `tag_name` and `html_url` off the response and
reporting them through the existing `Payload`/`Latest` shape and notification
template. `~/.atlas/release.json` still throttles the check to one call per 24
hours and is still written only after a successful response, so the throttle
semantics are unchanged. A rate-limited or unreachable API is an error the
caller drops silently, never a command failure. That endpoint is the only host
the flow contacts: **no code path of the update check reaches ariga.io**, and
the request carries no token - the fork's releases are public.

The GitHub response has no equivalent of the vercheck service's security
advisories, so `Payload.Advisory` is never set; the field and its rendering
are kept rather than removed, since the notification template is upstream's.

## Comparison by tag inequality

The fork tags - `v1.3.0-views-ce.N` - are not a plain semver line: the suffix
parses as a pre-release, which sorts *before* the `v1.3.0` it is built on, so
upstream's `semver.Compare` against the endpoint's answer would misjudge them.

The client needs no ordering at all, though. `releases/latest` already answers
with the single release users should be on, so the check notifies when the
returned tag differs from the running version - not when it is greater. A
deliberate rollback (an older tag re-published as latest) is then reported as
well, where a "greater than" comparison would go quiet on exactly the release
that needs picking up. The cost is that a build stamped with a tag ahead of
the published latest is told about an older release; it is the same one-line
nag, and the fork-suffix gate keeps development builds out of it entirely.

## `atlas version`

`parseV` (`cmd/atlas/internal/cmdapi/cmdapi.go`) points the release-notes link
at this fork's tag page for fork versions, and `versionInfo`
(`internal/cmdapi/version.go`) replaces upstream's "To download an official
version" line with this fork's releases page:

```
atlas community version v1.3.0-views-ce.3
https://github.com/torchsecurity/atlas/releases/tag/v1.3.0-views-ce.3
Fork releases: https://github.com/torchsecurity/atlas/releases
```

Non-fork version strings keep upstream's behavior exactly - development builds
and canary versions link `ariga/atlas/releases/latest`, a plain semver links
its upstream tag - so a binary of this tree stamped with an upstream version
still prints what upstream's tests pin.
