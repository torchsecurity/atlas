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
