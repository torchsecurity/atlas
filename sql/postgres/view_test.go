// Copyright 2021-present The Atlas Authors. All rights reserved.
// This source code is licensed under the Apache 2.0 license found
// in the LICENSE file in the root directory of this source tree.

package postgres

import (
	"context"
	"fmt"
	"testing"

	"ariga.io/atlas/sql/internal/sqltest"
	"ariga.io/atlas/sql/migrate"
	"ariga.io/atlas/sql/schema"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

// newView returns a view attached to a new schema named "public".
func newView(name, def string, columns ...*schema.Column) *schema.View {
	v := schema.NewView(name, def).AddColumns(columns...)
	schema.New("public").AddViews(v)
	return v
}

func TestPlanChanges_Views(t *testing.T) {
	tests := []struct {
		name    string
		changes []schema.Change
		want    []*migrate.Change
		wantErr string
	}{
		{
			name: "add view with explicit column list",
			changes: []schema.Change{
				&schema.AddView{
					V: newView(
						"user_names",
						"SELECT * FROM users",
						schema.NewIntColumn("id", "bigint"),
						schema.NewStringColumn("name", "text"),
					),
				},
			},
			want: []*migrate.Change{
				{
					Cmd:     `CREATE VIEW "public"."user_names" ("id", "name") AS SELECT * FROM users`,
					Reverse: `DROP VIEW "public"."user_names"`,
				},
			},
		},
		{
			name: "add view without a schema qualifier",
			changes: []schema.Change{
				&schema.AddView{
					V: schema.NewView("v1", "SELECT 1 AS c1").
						AddColumns(schema.NewIntColumn("c1", "integer")),
				},
			},
			want: []*migrate.Change{
				{
					Cmd:     `CREATE VIEW "v1" ("c1") AS SELECT 1 AS c1`,
					Reverse: `DROP VIEW "v1"`,
				},
			},
		},
		{
			name: "add view with comments",
			changes: []schema.Change{
				&schema.AddView{
					V: newView(
						"user_names",
						"SELECT id FROM users;",
						schema.NewIntColumn("id", "bigint").SetComment("the id"),
					).SetComment("names of users"),
				},
			},
			want: []*migrate.Change{
				{
					Cmd:     `CREATE VIEW "public"."user_names" ("id") AS SELECT id FROM users`,
					Reverse: `DROP VIEW "public"."user_names"`,
				},
				{
					Cmd:     `COMMENT ON VIEW "public"."user_names" IS 'names of users'`,
					Reverse: `COMMENT ON VIEW "public"."user_names" IS ''`,
				},
				{
					Cmd:     `COMMENT ON COLUMN "public"."user_names"."id" IS 'the id'`,
					Reverse: `COMMENT ON COLUMN "public"."user_names"."id" IS ''`,
				},
			},
		},
		{
			name: "add view with check option",
			changes: []schema.Change{
				&schema.AddView{
					V: newView("v1", "SELECT c1 FROM t1 WHERE c1 > 0", schema.NewIntColumn("c1", "integer")).
						SetCheckOption("LOCAL"),
				},
			},
			want: []*migrate.Change{
				{
					Cmd:     `CREATE VIEW "public"."v1" ("c1") AS SELECT c1 FROM t1 WHERE c1 > 0 WITH LOCAL CHECK OPTION`,
					Reverse: `DROP VIEW "public"."v1"`,
				},
			},
		},
		{
			name: "drop view",
			changes: []schema.Change{
				&schema.DropView{
					V: newView("v1", "SELECT c1 FROM t1", schema.NewIntColumn("c1", "integer")),
				},
			},
			want: []*migrate.Change{
				{
					Cmd:     `DROP VIEW "public"."v1"`,
					Reverse: `CREATE VIEW "public"."v1" ("c1") AS SELECT c1 FROM t1`,
				},
			},
		},
		{
			name: "drop view if exists",
			changes: []schema.Change{
				&schema.DropView{
					V:     newView("v1", "SELECT c1 FROM t1", schema.NewIntColumn("c1", "integer")),
					Extra: []schema.Clause{&schema.IfExists{}},
				},
			},
			want: []*migrate.Change{
				{
					Cmd:     `DROP VIEW IF EXISTS "public"."v1"`,
					Reverse: `CREATE VIEW "public"."v1" ("c1") AS SELECT c1 FROM t1`,
				},
			},
		},
		{
			name: "modify view definition is planned as drop and create",
			changes: []schema.Change{
				&schema.ModifyView{
					From: newView("v1", "SELECT c1 FROM t1", schema.NewIntColumn("c1", "integer")),
					To: newView("v1", "SELECT c1 FROM t1 WHERE c1 > 0",
						schema.NewIntColumn("c1", "integer"),
					),
				},
			},
			want: []*migrate.Change{
				{
					Cmd:     `DROP VIEW "public"."v1"`,
					Reverse: `CREATE VIEW "public"."v1" ("c1") AS SELECT c1 FROM t1`,
				},
				{
					Cmd:     `CREATE VIEW "public"."v1" ("c1") AS SELECT c1 FROM t1 WHERE c1 > 0`,
					Reverse: `DROP VIEW "public"."v1"`,
				},
			},
		},
		{
			name: "modify view definition restores its comments",
			changes: []schema.Change{
				&schema.ModifyView{
					From: newView("v1", "SELECT c1 FROM t1", schema.NewIntColumn("c1", "integer")).SetComment("c"),
					To: newView("v1", "SELECT c1 FROM t2", schema.NewIntColumn("c1", "integer")).
						SetComment("c"),
				},
			},
			want: []*migrate.Change{
				{
					Cmd:     `DROP VIEW "public"."v1"`,
					Reverse: `CREATE VIEW "public"."v1" ("c1") AS SELECT c1 FROM t1`,
				},
				{
					Cmd:     `CREATE VIEW "public"."v1" ("c1") AS SELECT c1 FROM t2`,
					Reverse: `DROP VIEW "public"."v1"`,
				},
				{
					Cmd:     `COMMENT ON VIEW "public"."v1" IS 'c'`,
					Reverse: `COMMENT ON VIEW "public"."v1" IS ''`,
				},
			},
		},
		{
			name: "modify view whitespace only is not a definition change",
			changes: []schema.Change{
				&schema.ModifyView{
					From: newView("v1", "SELECT c1\n    FROM t1", schema.NewIntColumn("c1", "integer")),
					To:   newView("v1", "SELECT c1\nFROM t1  ", schema.NewIntColumn("c1", "integer")),
					Changes: []schema.Change{
						&schema.ModifyAttr{
							From: &schema.Comment{Text: "old"},
							To:   &schema.Comment{Text: "new"},
						},
					},
				},
			},
			want: []*migrate.Change{
				{
					Cmd:     `COMMENT ON VIEW "public"."v1" IS 'new'`,
					Reverse: `COMMENT ON VIEW "public"."v1" IS 'old'`,
				},
			},
		},
		{
			name: "modify view comment only",
			changes: []schema.Change{
				func() schema.Change {
					from := newView("v1", "SELECT c1 FROM t1", schema.NewIntColumn("c1", "integer"))
					to := newView("v1", "SELECT c1 FROM t1", schema.NewIntColumn("c1", "integer")).SetComment("new")
					return &schema.ModifyView{
						From: from,
						To:   to,
						Changes: []schema.Change{
							&schema.AddAttr{A: &schema.Comment{Text: "new"}},
						},
					}
				}(),
			},
			want: []*migrate.Change{
				{
					Cmd:     `COMMENT ON VIEW "public"."v1" IS 'new'`,
					Reverse: `COMMENT ON VIEW "public"."v1" IS ''`,
				},
			},
		},
		{
			name: "modify view column comment only",
			changes: []schema.Change{
				func() schema.Change {
					c1 := schema.NewIntColumn("c1", "integer")
					c2 := schema.NewIntColumn("c1", "integer").SetComment("new")
					return &schema.ModifyView{
						From: newView("v1", "SELECT c1 FROM t1", c1),
						To:   newView("v1", "SELECT c1 FROM t1", c2),
						Changes: []schema.Change{
							&schema.ModifyColumn{From: c1, To: c2, Change: schema.ChangeComment},
						},
					}
				}(),
			},
			want: []*migrate.Change{
				{
					Cmd:     `COMMENT ON COLUMN "public"."v1"."c1" IS 'new'`,
					Reverse: `COMMENT ON COLUMN "public"."v1"."c1" IS ''`,
				},
			},
		},
		{
			name: "rename view",
			changes: []schema.Change{
				&schema.RenameView{
					From: newView("v1", "SELECT c1 FROM t1"),
					To:   newView("v2", "SELECT c1 FROM t1"),
				},
			},
			want: []*migrate.Change{
				{
					Cmd:     `ALTER VIEW "public"."v1" RENAME TO "v2"`,
					Reverse: `ALTER VIEW "public"."v2" RENAME TO "v1"`,
				},
			},
		},
		{
			name: "materialized views are not supported",
			changes: []schema.Change{
				&schema.AddView{
					V: schema.NewMaterializedView("m1", "SELECT c1 FROM t1"),
				},
			},
			wantErr: `postgres: materialized views are not supported by this version: "m1"`,
		},
		{
			name: "dropping materialized views is not supported",
			changes: []schema.Change{
				&schema.DropView{
					V: schema.NewMaterializedView("m1", "SELECT c1 FROM t1"),
				},
			},
			wantErr: `postgres: materialized views are not supported by this version: "m1"`,
		},
		{
			name: "view without a definition",
			changes: []schema.Change{
				&schema.AddView{V: schema.NewView("v1", "")},
			},
			wantErr: `missing definition for view "v1"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, mk, err := sqlmock.New()
			require.NoError(t, err)
			m := mock{mk}
			m.version("150000")
			drv, err := Open(db)
			require.NoError(t, err)
			plan, err := drv.PlanChanges(context.Background(), "wantPlan", tt.changes)
			if tt.wantErr != "" {
				require.EqualError(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			require.Len(t, plan.Changes, len(tt.want))
			for i, c := range plan.Changes {
				require.Equal(t, tt.want[i].Cmd, c.Cmd)
				require.Equal(t, tt.want[i].Reverse, c.Reverse)
			}
			require.True(t, plan.Reversible, "view changes are expected to be reversible")
		})
	}
}

// TestPlanChanges_ViewOrdering asserts the shared sorting (sqlx.SortChanges and
// sortViewChanges) orders view changes against the changes of the objects they
// depend on: views are created after them and dropped before them.
func TestPlanChanges_ViewOrdering(t *testing.T) {
	newState := func() (*schema.Table, *schema.View, *schema.View) {
		s := schema.New("public")
		users := schema.NewTable("users").AddColumns(schema.NewIntColumn("id", "bigint"))
		v1 := schema.NewView("v1", "SELECT id FROM users").AddColumns(schema.NewIntColumn("id", "bigint"))
		v2 := schema.NewView("v2", "SELECT id FROM v1").AddColumns(schema.NewIntColumn("id", "bigint"))
		s.AddTables(users)
		s.AddViews(v1, v2)
		v1.AddDeps(users)
		v2.AddDeps(v1)
		return users, v1, v2
	}
	plan := func(t *testing.T, changes ...schema.Change) []string {
		db, mk, err := sqlmock.New()
		require.NoError(t, err)
		m := mock{mk}
		m.version("150000")
		drv, err := Open(db)
		require.NoError(t, err)
		p, err := drv.PlanChanges(context.Background(), "wantPlan", changes)
		require.NoError(t, err)
		cmds := make([]string, len(p.Changes))
		for i, c := range p.Changes {
			cmds[i] = c.Cmd
		}
		return cmds
	}
	t.Run("create", func(t *testing.T) {
		users, v1, v2 := newState()
		require.Equal(t, []string{
			`CREATE TABLE "public"."users" ("id" bigint NOT NULL)`,
			`CREATE VIEW "public"."v1" ("id") AS SELECT id FROM users`,
			`CREATE VIEW "public"."v2" ("id") AS SELECT id FROM v1`,
		}, plan(t, &schema.AddView{V: v2}, &schema.AddView{V: v1}, &schema.AddTable{T: users}))
	})
	t.Run("drop", func(t *testing.T) {
		users, v1, v2 := newState()
		require.Equal(t, []string{
			`DROP VIEW "public"."v2"`,
			`DROP VIEW "public"."v1"`,
			`DROP TABLE "public"."users"`,
		}, plan(t, &schema.DropView{V: v1}, &schema.DropView{V: v2}, &schema.DropTable{T: users}))
	})
	t.Run("modify table before creating a view on it", func(t *testing.T) {
		users, v1, _ := newState()
		require.Equal(t, []string{
			`ALTER TABLE "public"."users" ADD COLUMN "name" text NOT NULL`,
			`CREATE VIEW "public"."v1" ("id") AS SELECT id FROM users`,
		}, plan(t,
			&schema.AddView{V: v1},
			&schema.ModifyTable{T: users, Changes: []schema.Change{
				&schema.AddColumn{C: schema.NewStringColumn("name", "text")},
			}},
		))
	})
}

func TestViewAttrChanges(t *testing.T) {
	var d diff
	require.Empty(t, d.ViewAttrChanges(
		schema.NewView("v1", "SELECT 1"),
		schema.NewView("v1", "SELECT 1"),
	))
	changes := d.ViewAttrChanges(
		schema.NewView("v1", "SELECT 1"),
		schema.NewView("v1", "SELECT 1").SetComment("c"),
	)
	require.Len(t, changes, 1)
	require.Equal(t, &schema.AddAttr{A: &schema.Comment{Text: "c"}}, changes[0])

	changes = d.ViewAttrChanges(
		schema.NewView("v1", "SELECT 1").SetComment("old"),
		schema.NewView("v1", "SELECT 1").SetComment("new"),
	)
	require.Len(t, changes, 1)
	require.Equal(t, &schema.ModifyAttr{
		From: &schema.Comment{Text: "old"},
		To:   &schema.Comment{Text: "new"},
	}, changes[0])
}

func TestDiff_Views(t *testing.T) {
	db, mk, err := sqlmock.New()
	require.NoError(t, err)
	mock{mk}.version("150000")
	drv, err := Open(db)
	require.NoError(t, err)
	viewSchema := func(views ...*schema.View) *schema.Schema {
		return schema.New("public").AddViews(views...)
	}
	t.Run("no changes", func(t *testing.T) {
		changes, err := drv.SchemaDiff(
			viewSchema(schema.NewView("v1", "SELECT c1\n    FROM t1")),
			viewSchema(schema.NewView("v1", "SELECT c1\nFROM t1")),
		)
		require.NoError(t, err)
		require.Empty(t, changes, "a reindented definition is not a change")
	})
	t.Run("add", func(t *testing.T) {
		changes, err := drv.SchemaDiff(viewSchema(), viewSchema(schema.NewView("v1", "SELECT 1")))
		require.NoError(t, err)
		require.Len(t, changes, 1)
		require.IsType(t, &schema.AddView{}, changes[0])
	})
	t.Run("drop", func(t *testing.T) {
		changes, err := drv.SchemaDiff(viewSchema(schema.NewView("v1", "SELECT 1")), viewSchema())
		require.NoError(t, err)
		require.Len(t, changes, 1)
		require.IsType(t, &schema.DropView{}, changes[0])
	})
	t.Run("modify definition", func(t *testing.T) {
		changes, err := drv.SchemaDiff(
			viewSchema(schema.NewView("v1", "SELECT 1")),
			viewSchema(schema.NewView("v1", "SELECT 2")),
		)
		require.NoError(t, err)
		require.Len(t, changes, 1)
		m, ok := changes[0].(*schema.ModifyView)
		require.True(t, ok)
		require.Empty(t, m.Changes)
	})
	t.Run("modify comment", func(t *testing.T) {
		changes, err := drv.SchemaDiff(
			viewSchema(schema.NewView("v1", "SELECT 1").SetComment("old")),
			viewSchema(schema.NewView("v1", "SELECT 1").SetComment("new")),
		)
		require.NoError(t, err)
		require.Len(t, changes, 1)
		m, ok := changes[0].(*schema.ModifyView)
		require.True(t, ok)
		require.Equal(t, []schema.Change{
			&schema.ModifyAttr{From: &schema.Comment{Text: "old"}, To: &schema.Comment{Text: "new"}},
		}, m.Changes)
	})
}

func TestInspectViews(t *testing.T) {
	db, m, err := sqlmock.New()
	require.NoError(t, err)
	mk := mock{m}
	mk.version("150000")
	drv, err := Open(db)
	require.NoError(t, err)
	mk.ExpectQuery(sqltest.Escape(fmt.Sprintf(schemasQueryArgs, "= $1"))).
		WithArgs("public").
		WillReturnRows(sqltest.Rows(`
 schema_name | comment
-------------+---------
 public      | nil
`))
	m.ExpectQuery(sqltest.Escape(fmt.Sprintf(viewsQuery, "$1"))).
		WithArgs("public").
		WillReturnRows(
			sqlmock.NewRows([]string{"oid", "schema_name", "view_name", "definition", "comment"}).
				AddRow(1001, "public", "user_names", " SELECT users.id,\n    users.name\n   FROM users;", "names").
				AddRow(1002, "public", "admin_names", " SELECT user_names.id\n   FROM user_names;", nil),
		)
	m.ExpectQuery(sqltest.Escape(fmt.Sprintf(columnsQuery, "$2, $3"))).
		WithArgs("public", "user_names", "admin_names").
		WillReturnRows(
			sqlmock.NewRows(columnsQueryColumns).
				AddRow("user_names", "id", "bigint", "int8", "YES", nil, nil, 64, nil, 0, nil, nil, nil, "NO", nil, nil, nil, nil, nil, nil, "b", nil, 20, 1).
				AddRow("user_names", "name", "text", "text", "YES", nil, nil, nil, nil, nil, nil, nil, nil, "NO", nil, nil, nil, nil, nil, "the name", "b", nil, 25, 2).
				AddRow("admin_names", "id", "bigint", "int8", "YES", nil, nil, 64, nil, 0, nil, nil, nil, "NO", nil, nil, nil, nil, nil, nil, "b", nil, 20, 1),
		)
	m.ExpectQuery(sqltest.Escape(fmt.Sprintf(viewDepsQuery, "$1"))).
		WithArgs("public").
		WillReturnRows(
			sqlmock.NewRows([]string{"schema_name", "view_name", "ref_schema_name", "ref_name", "ref_kind"}).
				// A view on a view. The referenced table resides in a schema
				// that was not inspected, hence it is not linked.
				AddRow("public", "admin_names", "public", "user_names", "v").
				AddRow("public", "user_names", "other", "users", "r"),
		)
	s, err := drv.InspectSchema(context.Background(), "public", &schema.InspectOptions{
		Mode: schema.InspectSchemas | schema.InspectViews,
	})
	require.NoError(t, err)
	require.Len(t, s.Views, 2)

	v1, ok := s.View("user_names")
	require.True(t, ok)
	require.False(t, v1.Materialized())
	require.Equal(t, "SELECT users.id,\n    users.name\n   FROM users", v1.Def)
	require.Equal(t, []schema.Attr{&OID{V: 1001}, &schema.Comment{Text: "names"}}, v1.Attrs)
	require.Len(t, v1.Columns, 2)
	require.Equal(t, "id", v1.Columns[0].Name)
	require.Equal(t, &schema.IntegerType{T: "bigint"}, v1.Columns[0].Type.Type)
	require.Equal(t, "name", v1.Columns[1].Name)
	require.Equal(t, []schema.Attr{&schema.Comment{Text: "the name"}}, v1.Columns[1].Attrs)
	require.Empty(t, v1.Deps, "dependencies outside the inspected scope are not linked")

	v2, ok := s.View("admin_names")
	require.True(t, ok)
	require.Equal(t, []schema.Attr{&OID{V: 1002}}, v2.Attrs)
	require.Equal(t, []schema.Object{schema.Object(v1)}, v2.Deps)
	require.Equal(t, []schema.Object{schema.Object(v2)}, v1.Refs)
}

// columnsQueryColumns are the columns returned by the columns query,
// which is reused by the views inspection for the view columns.
var columnsQueryColumns = []string{
	"table_name", "column_name", "data_type", "format_type", "is_nullable", "column_default",
	"character_maximum_length", "numeric_precision", "datetime_precision", "numeric_scale",
	"interval_type", "character_set_name", "collation_name", "is_identity", "identity_start",
	"identity_increment", "identity_last", "identity_generation", "generation_expression",
	"comment", "typtype", "typelem", "oid", "attnum",
}
