// Copyright 2021-present The Atlas Authors. All rights reserved.
// This source code is licensed under the Apache 2.0 license found
// in the LICENSE file in the root directory of this source tree.

package postgres

import (
	"strings"
	"testing"

	"ariga.io/atlas/sql/internal/sqlx"
	"ariga.io/atlas/sql/schema"

	"github.com/stretchr/testify/require"
)

func TestUnmarshalSpec_View(t *testing.T) {
	f := `
schema "public" {
}

enum "employment_status" {
  schema = schema.public
  values = ["active", "terminated"]
}

table "employees" {
  schema = schema.public
  column "id" {
    type = integer
  }
  column "status" {
    type = enum.employment_status
  }
}

view "active_employees" {
  schema = schema.public
  column "id" {
    type = integer
  }
  column "status" {
    type = enum.employment_status
  }
  as = <<-SQL
  SELECT id, status
    FROM employees
   WHERE status = 'active'
  SQL
  depends_on = [table.employees]
  comment    = "currently employed people"
}

view "active_managers" {
  schema = schema.public
  column "id" {
    type = integer
  }
  as = <<-SQL
  SELECT id FROM active_employees WHERE is_manager
  SQL
  depends_on = [table.employees, view.active_employees]
}
`
	var s schema.Schema
	require.NoError(t, EvalHCLBytes([]byte(f), &s, nil))
	require.Len(t, s.Views, 2)

	v, ok := s.View("active_employees")
	require.True(t, ok)
	require.Equal(t, "active_employees", v.Name)
	require.Equal(t, "public", v.Schema.Name)
	require.False(t, v.Materialized())
	// Heredoc definitions are dedented by the HCL parser and
	// retain the trailing newline of the last line.
	require.Equal(t, "SELECT id, status\n  FROM employees\n WHERE status = 'active'\n", v.Def)
	c := schema.Comment{}
	require.True(t, sqlx.Has(v.Attrs, &c))
	require.Equal(t, "currently employed people", c.Text)

	// Typed columns, including the enum-typed one, are resolved.
	require.Len(t, v.Columns, 2)
	id, ok := v.Column("id")
	require.True(t, ok)
	require.Equal(t, &schema.IntegerType{T: TypeInteger}, id.Type.Type)
	status, ok := v.Column("status")
	require.True(t, ok)
	e, ok := status.Type.Type.(*schema.EnumType)
	require.True(t, ok, "expected the view column to be resolved to an enum, got %T", status.Type.Type)
	require.Equal(t, "employment_status", e.T)
	require.Equal(t, []string{"active", "terminated"}, e.Values)
	require.Equal(t, "public", e.Schema.Name)

	// Dependencies on tables and on other views.
	tbl, ok := s.Table("employees")
	require.True(t, ok)
	require.Equal(t, []schema.Object{schema.Object(tbl)}, v.Deps)

	m, ok := s.View("active_managers")
	require.True(t, ok)
	require.Equal(t, "SELECT id FROM active_employees WHERE is_manager\n", m.Def)
	require.Equal(t, []schema.Object{schema.Object(tbl), schema.Object(v)}, m.Deps)
}

func TestUnmarshalSpec_MaterializedView(t *testing.T) {
	f := `
schema "public" {
}

materialized "counters" {
  schema = schema.public
  column "total" {
    type = bigint
  }
  as = "SELECT count(*) AS total FROM employees"
}
`
	var s schema.Schema
	require.NoError(t, EvalHCLBytes([]byte(f), &s, nil))
	require.Len(t, s.Views, 1)
	v, ok := s.Materialized("counters")
	require.True(t, ok)
	require.True(t, v.Materialized())
	require.Equal(t, "SELECT count(*) AS total FROM employees", v.Def)
	require.Len(t, v.Columns, 1)
	require.Equal(t, &schema.IntegerType{T: TypeBigInt}, v.Columns[0].Type.Type)
	_, ok = s.View("counters")
	require.False(t, ok, "a materialized view is not reported as a plain view")
}

func TestMarshalSpec_View(t *testing.T) {
	e := &schema.EnumType{T: "employment_status", Values: []string{"active", "terminated"}}
	tbl := schema.NewTable("employees").
		AddColumns(
			schema.NewIntColumn("id", TypeInteger),
			schema.NewColumn("status").SetType(e),
		)
	v := schema.NewView("active_employees", "SELECT id, status\n  FROM employees\n WHERE status = 'active'").
		AddColumns(
			schema.NewIntColumn("id", TypeInteger),
			schema.NewColumn("status").SetType(e),
		).
		AddDeps(tbl).
		SetComment("currently employed people")
	v2 := schema.NewView("active_managers", "SELECT id FROM active_employees WHERE is_manager").
		AddColumns(schema.NewIntColumn("id", TypeInteger)).
		AddDeps(tbl, v)
	s := schema.New("public").
		AddObjects(e).
		AddTables(tbl).
		AddViews(v, v2)
	e.Schema = s

	buf, err := MarshalHCL(s)
	require.NoError(t, err)
	const expected = `table "employees" {
  schema = schema.public
  column "id" {
    null = false
    type = integer
  }
  column "status" {
    null = false
    type = enum.employment_status
  }
}
view "active_employees" {
  schema = schema.public
  column "id" {
    null = false
    type = integer
  }
  column "status" {
    null = false
    type = enum.employment_status
  }
  as         = <<-SQL
  SELECT id, status
    FROM employees
   WHERE status = 'active'
  SQL
  depends_on = [table.employees]
  comment    = "currently employed people"
}
view "active_managers" {
  schema = schema.public
  column "id" {
    null = false
    type = integer
  }
  as         = "SELECT id FROM active_employees WHERE is_manager"
  depends_on = [table.employees, view.active_employees]
}
enum "employment_status" {
  schema = schema.public
  values = ["active", "terminated"]
}
schema "public" {
}
`
	require.Equal(t, expected, string(buf))

	// Round-trip: the marshaled document evaluates back to the same views.
	var s1 schema.Schema
	require.NoError(t, EvalHCLBytes(buf, &s1, nil))
	require.Len(t, s1.Views, 2)
	rv, ok := s1.View("active_employees")
	require.True(t, ok)
	require.Equal(t, v.Def, strings.TrimSuffix(rv.Def, "\n"))
	require.Len(t, rv.Columns, 2)
	require.Equal(t, "id", rv.Columns[0].Name)
	require.Equal(t, "status", rv.Columns[1].Name)
	re, ok := rv.Columns[1].Type.Type.(*schema.EnumType)
	require.True(t, ok)
	require.Equal(t, e.T, re.T)
	require.Equal(t, e.Values, re.Values)
	rt, ok := s1.Table("employees")
	require.True(t, ok)
	require.Equal(t, []schema.Object{schema.Object(rt)}, rv.Deps)
	rv2, ok := s1.View("active_managers")
	require.True(t, ok)
	require.Equal(t, v2.Def, rv2.Def)
	require.Equal(t, []schema.Object{schema.Object(rt), schema.Object(rv)}, rv2.Deps)
}
