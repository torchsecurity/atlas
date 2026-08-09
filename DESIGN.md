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
