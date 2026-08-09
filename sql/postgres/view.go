// Copyright 2021-present The Atlas Authors. All rights reserved.
// This source code is licensed under the Apache 2.0 license found
// in the LICENSE file in the root directory of this source tree.

package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"ariga.io/atlas/sql/internal/sqlx"
	"ariga.io/atlas/sql/migrate"
	"ariga.io/atlas/sql/schema"
)

// inspectViews inspects the plain (non-materialized) views of the given realm,
// their columns, comments and the objects they depend on.
func (i *inspect) inspectViews(ctx context.Context, r *schema.Realm, opts *schema.InspectOptions) error {
	if err := i.views(ctx, r, opts); err != nil {
		return err
	}
	for _, s := range r.Schemas {
		if len(s.Views) == 0 {
			continue
		}
		if err := i.viewColumns(ctx, s); err != nil {
			return err
		}
	}
	if !hasViews(r) {
		return nil
	}
	return i.viewDeps(ctx, r)
}

// views queries and appends the views of the given realm.
func (i *inspect) views(ctx context.Context, realm *schema.Realm, opts *schema.InspectOptions) error {
	var (
		args  []any
		query = fmt.Sprintf(viewsQuery, nArgs(0, len(realm.Schemas)))
	)
	for _, s := range realm.Schemas {
		args = append(args, s.Name)
	}
	// The InspectOptions.Tables option narrows the inspection to a set of
	// relation names. Views are filtered by the same list, as the option
	// defines the scope of the inspected schema.
	if opts != nil && len(opts.Tables) > 0 {
		for _, t := range opts.Tables {
			args = append(args, t)
		}
		query = fmt.Sprintf(viewsQueryArgs, nArgs(0, len(realm.Schemas)), nArgs(len(realm.Schemas), len(opts.Tables)))
	}
	rows, err := i.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("postgres: querying views: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		if err := scanView(realm, rows); err != nil {
			return fmt.Errorf("postgres: %w", err)
		}
	}
	return rows.Err()
}

// scanView scans the current row and adds the view it describes to its schema.
func scanView(realm *schema.Realm, rows *sql.Rows) error {
	var (
		oid                         sql.NullInt64
		vSchema, name, def, comment sql.NullString
	)
	if err := rows.Scan(&oid, &vSchema, &name, &def, &comment); err != nil {
		return fmt.Errorf("scan view information: %w", err)
	}
	if !sqlx.ValidString(vSchema) || !sqlx.ValidString(name) {
		return fmt.Errorf("invalid schema or view name: %q.%q", vSchema.String, name.String)
	}
	s, ok := realm.Schema(vSchema.String)
	if !ok {
		return fmt.Errorf("schema %q was not found in realm", vSchema.String)
	}
	v := schema.NewView(name.String, viewDef(def.String))
	if oid.Valid {
		v.AddAttrs(&OID{V: oid.Int64})
	}
	if sqlx.ValidString(comment) {
		v.SetComment(comment.String)
	}
	s.AddViews(v)
	return nil
}

// viewDef normalizes a view definition. "pg_get_viewdef" terminates the
// definition it returns with a semicolon and pads it, but the definition
// Atlas keeps (and writes back on CREATE) is the bare query.
func viewDef(def string) string {
	return strings.TrimSuffix(strings.TrimSpace(def), ";")
}

// viewColumns queries and appends the columns of the schema views. Note, the
// table columns query is reused, as "information_schema.columns" reports the
// columns of views as well. Unlike tables, the columns are attached to their
// view and not looked up in the schema tables.
func (i *inspect) viewColumns(ctx context.Context, s *schema.Schema) error {
	query := columnsQuery
	if i.crdb {
		query = crdbColumnsQuery
	}
	args := []any{s.Name}
	for _, v := range s.Views {
		args = append(args, v.Name)
	}
	rows, err := i.QueryContext(ctx, fmt.Sprintf(query, nArgs(1, len(s.Views))), args...)
	if err != nil {
		return fmt.Errorf("postgres: querying schema %q view columns: %w", s.Name, err)
	}
	defer rows.Close()
	for rows.Next() {
		if err := i.addViewColumn(s, rows); err != nil {
			return fmt.Errorf("postgres: %w", err)
		}
	}
	return rows.Err()
}

// addViewColumn scans the current row and adds the column it describes to its view.
func (i *inspect) addViewColumn(s *schema.Schema, rows *sql.Rows) error {
	name, c, err := i.scanColumn(s, rows)
	if err != nil {
		return err
	}
	v, ok := s.View(name)
	if !ok {
		return fmt.Errorf("view %q was not found in schema", name)
	}
	v.AddColumns(c)
	return nil
}

// viewDeps queries the objects the realm views depend on and links them.
func (i *inspect) viewDeps(ctx context.Context, realm *schema.Realm) error {
	var (
		args  []any
		query = fmt.Sprintf(viewDepsQuery, nArgs(0, len(realm.Schemas)))
	)
	for _, s := range realm.Schemas {
		args = append(args, s.Name)
	}
	rows, err := i.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("postgres: querying view dependencies: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		if err := scanViewDep(realm, rows); err != nil {
			return fmt.Errorf("postgres: %w", err)
		}
	}
	return rows.Err()
}

// scanViewDep scans the current row and links the view it describes to the object it depends on.
func scanViewDep(realm *schema.Realm, rows *sql.Rows) error {
	var vSchema, vName, refSchema, refName, refKind string
	if err := rows.Scan(&vSchema, &vName, &refSchema, &refName, &refKind); err != nil {
		return fmt.Errorf("scan view dependency: %w", err)
	}
	s, ok := realm.Schema(vSchema)
	if !ok {
		return fmt.Errorf("schema %q was not found in realm", vSchema)
	}
	v, ok := s.View(vName)
	if !ok {
		return fmt.Errorf("view %q was not found in schema %q", vName, vSchema)
	}
	o, ok := realmObject(realm, refSchema, refName, refKind)
	// Objects that reside outside the inspected scope (e.g., a view on a
	// table of a schema that was not inspected) are not linked.
	if !ok {
		return nil
	}
	v.AddDeps(o)
	return nil
}

// realmObject returns the table or the view the given "pg_class" row describes,
// and reports if it was found in the inspected realm.
func realmObject(realm *schema.Realm, ns, name, relkind string) (schema.Object, bool) {
	s, ok := realm.Schema(ns)
	if !ok {
		return nil, false
	}
	switch relkind {
	// Materialized views ('m') are not inspected by this version,
	// and hence are never linked as dependencies.
	case "v":
		v, ok := s.View(name)
		if !ok {
			return nil, false
		}
		return v, true
	case "r", "p", "f":
		t, ok := s.Table(name)
		if !ok {
			return nil, false
		}
		return t, true
	}
	return nil, false
}

// hasViews reports if any schema in the realm has views.
func hasViews(realm *schema.Realm) bool {
	for _, s := range realm.Schemas {
		if len(s.Views) > 0 {
			return true
		}
	}
	return false
}

// addView builds and appends the statements for creating a view.
func (s *state) addView(add *schema.AddView) error {
	if err := supportedView(add.V); err != nil {
		return err
	}
	create, err := s.createView(add.V)
	if err != nil {
		return err
	}
	s.append(&migrate.Change{
		Cmd:     create,
		Source:  add,
		Comment: fmt.Sprintf("create %q view", add.V.Name),
		Reverse: s.Build("DROP VIEW").View(add.V).String(),
	})
	s.addViewComments(add, add.V)
	return nil
}

// dropView builds and appends the statement for dropping a view. Note, the
// statement is never extended with a CASCADE clause on its own, as dropping
// dependent views is the job of the changes sorting.
func (s *state) dropView(drop *schema.DropView) error {
	if err := supportedView(drop.V); err != nil {
		return err
	}
	reverse, err := s.createView(drop.V)
	if err != nil {
		return fmt.Errorf("calculate reverse for drop view %q: %w", drop.V.Name, err)
	}
	b := s.Build("DROP VIEW")
	if sqlx.Has(drop.Extra, &schema.IfExists{}) {
		b.P("IF EXISTS")
	}
	b.View(drop.V)
	if sqlx.Has(drop.Extra, &Cascade{}) {
		b.P("CASCADE")
	}
	s.append(&migrate.Change{
		Cmd:     b.String(),
		Source:  drop,
		Comment: fmt.Sprintf("drop %q view", drop.V.Name),
		Reverse: reverse,
	})
	return nil
}

// modifyView builds and appends the statements that bring the view into its
// modified state. Views hold no data, hence a definition change is planned as
// a DROP and a CREATE, sidestepping the column-shape restrictions of the
// "CREATE OR REPLACE VIEW" command. Changes that do not touch the definition
// (i.e. comments) are planned in place.
//
// Note, the diffing never reports a definition change as a ModifyView anymore
// (see sqlx.recreateViewChanges), as the drop and the create must be interleaved
// with those of the dependent views. The definition path below is kept for
// changesets that are built by hand and passed directly to the planner.
func (s *state) modifyView(modify *schema.ModifyView) error {
	if err := supportedView(modify.From); err != nil {
		return err
	}
	if err := supportedView(modify.To); err != nil {
		return err
	}
	if !sqlx.BodyDefChanged(modify.From.Def, modify.To.Def) {
		return s.modifyViewAttrs(modify)
	}
	create, err := s.createView(modify.To)
	if err != nil {
		return err
	}
	reverse, err := s.createView(modify.From)
	if err != nil {
		return fmt.Errorf("calculate reverse for modify view %q: %w", modify.From.Name, err)
	}
	s.append(
		&migrate.Change{
			Cmd:     s.Build("DROP VIEW").View(modify.From).String(),
			Source:  modify,
			Comment: fmt.Sprintf("drop %q view before recreating it", modify.From.Name),
			Reverse: reverse,
		},
		&migrate.Change{
			Cmd:     create,
			Source:  modify,
			Comment: fmt.Sprintf("recreate %q view", modify.To.Name),
			Reverse: s.Build("DROP VIEW").View(modify.To).String(),
		},
	)
	// The comments of the view and its columns are
	// dropped with it and need to be set again.
	s.addViewComments(modify, modify.To)
	return nil
}

// modifyViewAttrs plans the view changes that do not require
// recreating it. Currently, only comments are supported.
func (s *state) modifyViewAttrs(modify *schema.ModifyView) error {
	for _, c := range modify.Changes {
		switch c := c.(type) {
		case *schema.AddAttr, *schema.ModifyAttr:
			from, to, err := commentChange(c)
			if err != nil {
				return err
			}
			s.append(s.viewComment(modify, modify.To, to, from))
		case *schema.ModifyColumn:
			if c.Change != schema.ChangeComment {
				return fmt.Errorf("unsupported change for view column %q: only comments can be modified", c.To.Name)
			}
			from, to, err := commentChange(sqlx.CommentDiff(c.From.Attrs, c.To.Attrs))
			if err != nil {
				return err
			}
			s.append(s.viewColumnComment(modify, modify.To, c.To, to, from))
		default:
			return fmt.Errorf("unsupported view change %T", c)
		}
	}
	return nil
}

// renameView builds and appends the statement for renaming a view.
func (s *state) renameView(c *schema.RenameView) error {
	if err := supportedView(c.From); err != nil {
		return err
	}
	if err := supportedView(c.To); err != nil {
		return err
	}
	s.append(&migrate.Change{
		Source:  c,
		Comment: fmt.Sprintf("rename a view from %q to %q", c.From.Name, c.To.Name),
		Cmd:     s.Build("ALTER VIEW").View(c.From).P("RENAME TO").Ident(c.To.Name).String(),
		Reverse: s.Build("ALTER VIEW").View(c.To).P("RENAME TO").Ident(c.From.Name).String(),
	})
	return nil
}

// createView returns the "CREATE VIEW" statement for the given view. The column
// list is always written explicitly, as the definition might select all columns
// of its underlying relations (i.e. "SELECT *"), and the list is what pins the
// names of the view columns.
func (s *state) createView(v *schema.View) (string, error) {
	def := viewDef(v.Def)
	if def == "" {
		return "", fmt.Errorf("missing definition for view %q", v.Name)
	}
	b := s.Build("CREATE VIEW").View(v)
	if len(v.Columns) > 0 {
		b.Wrap(func(b *sqlx.Builder) {
			b.MapComma(v.Columns, func(i int, b *sqlx.Builder) {
				b.Ident(v.Columns[i].Name)
			})
		})
	}
	b.P("AS", def)
	if o := (schema.ViewCheckOption{}); sqlx.Has(v.Attrs, &o) && !strings.EqualFold(o.V, "NONE") {
		b.P("WITH", strings.ToUpper(o.V), "CHECK OPTION")
	}
	return b.String(), nil
}

// addViewComments appends the comments of the view and its columns, if any.
func (s *state) addViewComments(src schema.Change, v *schema.View) {
	var c schema.Comment
	if sqlx.Has(v.Attrs, &c) && c.Text != "" {
		s.append(s.viewComment(src, v, c.Text, ""))
	}
	for _, col := range v.Columns {
		if sqlx.Has(col.Attrs, &c) && c.Text != "" {
			s.append(s.viewColumnComment(src, v, col, c.Text, ""))
		}
	}
}

func (s *state) viewComment(src schema.Change, v *schema.View, to, from string) *migrate.Change {
	b := s.Build("COMMENT ON VIEW").View(v).P("IS")
	return &migrate.Change{
		Cmd:     b.Clone().P(quote(to)).String(),
		Source:  src,
		Comment: fmt.Sprintf("set comment to view: %q", v.Name),
		Reverse: b.Clone().P(quote(from)).String(),
	}
}

func (s *state) viewColumnComment(src schema.Change, v *schema.View, c *schema.Column, to, from string) *migrate.Change {
	b := s.Build("COMMENT ON COLUMN").ViewResource(v, c).P("IS")
	return &migrate.Change{
		Cmd:     b.Clone().P(quote(to)).String(),
		Source:  src,
		Comment: fmt.Sprintf("set comment to column: %q on view: %q", c.Name, v.Name),
		Reverse: b.Clone().P(quote(from)).String(),
	}
}

// supportedView reports an error in case the view is not
// supported by this version. i.e., materialized views.
func supportedView(v *schema.View) error {
	if v.Materialized() {
		return fmt.Errorf("postgres: materialized views are not supported by this version: %q", v.Name)
	}
	return nil
}

const (
	// Query to list the plain (non-materialized) views of the given schemas.
	// Note, relkind 'm' (materialized views) is deliberately not selected.
	viewsQuery = `
SELECT
	t1.oid,
	t2.nspname AS schema_name,
	t1.relname AS view_name,
	pg_catalog.pg_get_viewdef(t1.oid, true) AS definition,
	pg_catalog.obj_description(t1.oid, 'pg_class') AS comment
FROM
	pg_catalog.pg_class AS t1
	JOIN pg_catalog.pg_namespace AS t2 ON t2.oid = t1.relnamespace
	LEFT JOIN pg_catalog.pg_depend AS t3 ON t3.classid = 'pg_catalog.pg_class'::regclass::oid AND t3.objid = t1.oid AND t3.deptype = 'e'
WHERE
	t1.relkind = 'v'
	AND t2.nspname IN (%s)
	AND t3.objid IS NULL
ORDER BY
	t2.nspname, t1.relname
`
	// Query to list the plain views of the given schemas by their names.
	viewsQueryArgs = `
SELECT
	t1.oid,
	t2.nspname AS schema_name,
	t1.relname AS view_name,
	pg_catalog.pg_get_viewdef(t1.oid, true) AS definition,
	pg_catalog.obj_description(t1.oid, 'pg_class') AS comment
FROM
	pg_catalog.pg_class AS t1
	JOIN pg_catalog.pg_namespace AS t2 ON t2.oid = t1.relnamespace
	LEFT JOIN pg_catalog.pg_depend AS t3 ON t3.classid = 'pg_catalog.pg_class'::regclass::oid AND t3.objid = t1.oid AND t3.deptype = 'e'
WHERE
	t1.relkind = 'v'
	AND t2.nspname IN (%s)
	AND t1.relname IN (%s)
	AND t3.objid IS NULL
ORDER BY
	t2.nspname, t1.relname
`
	// Query to list the relations the views depend on. A view holds a "_RETURN"
	// rewrite rule, and the rule depends on every relation its query references.
	viewDepsQuery = `
SELECT
	t2.nspname AS schema_name,
	t1.relname AS view_name,
	t6.nspname AS ref_schema_name,
	t5.relname AS ref_name,
	t5.relkind AS ref_kind
FROM
	pg_catalog.pg_class AS t1
	JOIN pg_catalog.pg_namespace AS t2 ON t2.oid = t1.relnamespace
	JOIN pg_catalog.pg_rewrite AS t3 ON t3.ev_class = t1.oid AND t3.rulename = '_RETURN'
	JOIN pg_catalog.pg_depend AS t4 ON t4.classid = 'pg_catalog.pg_rewrite'::regclass::oid AND t4.objid = t3.oid AND t4.refclassid = 'pg_catalog.pg_class'::regclass::oid AND t4.deptype = 'n'
	JOIN pg_catalog.pg_class AS t5 ON t5.oid = t4.refobjid
	JOIN pg_catalog.pg_namespace AS t6 ON t6.oid = t5.relnamespace
WHERE
	t1.relkind = 'v'
	AND t5.oid <> t1.oid
	AND t5.relkind IN ('r', 'p', 'v', 'm', 'f')
	AND t2.nspname IN (%s)
GROUP BY
	1, 2, 3, 4, 5
ORDER BY
	1, 2, 3, 4
`
)
