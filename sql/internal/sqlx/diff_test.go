// Copyright 2021-present The Atlas Authors. All rights reserved.
// This source code is licensed under the Apache 2.0 license found
// in the LICENSE file in the root directory of this source tree.

package sqlx

import (
	"testing"

	"ariga.io/atlas/sql/schema"

	"github.com/stretchr/testify/require"
)

// mockDiffDriver is a DiffDriver that reports no dialect-specific
// changes, except for view comments.
type mockDiffDriver struct{}

func (*mockDiffDriver) RealmObjectDiff(_, _ *schema.Realm) ([]schema.Change, error) {
	return nil, nil
}

func (*mockDiffDriver) SchemaAttrDiff(_, _ *schema.Schema) []schema.Change {
	return nil
}

func (*mockDiffDriver) SchemaObjectDiff(_, _ *schema.Schema, _ *schema.DiffOptions) ([]schema.Change, error) {
	return nil, nil
}

func (*mockDiffDriver) TableAttrDiff(_, _ *schema.Table, _ *schema.DiffOptions) ([]schema.Change, error) {
	return nil, nil
}

func (*mockDiffDriver) ViewAttrChanges(from, to *schema.View) []schema.Change {
	if c := CommentDiff(from.Attrs, to.Attrs); c != nil {
		return []schema.Change{c}
	}
	return nil
}

func (*mockDiffDriver) ColumnChange(_ *schema.Table, _, _ *schema.Column, _ *schema.DiffOptions) (schema.Change, error) {
	return NoChange, nil
}

func (*mockDiffDriver) IndexAttrChanged(_, _ []schema.Attr) bool { return false }

func (*mockDiffDriver) IndexPartAttrChanged(_, _ *schema.Index, _ int) bool { return false }

func (*mockDiffDriver) IsGeneratedIndexName(_ *schema.Table, _ *schema.Index) bool { return false }

func (*mockDiffDriver) ReferenceChanged(_, _ schema.ReferenceOption) bool { return false }

func (*mockDiffDriver) ForeignKeyAttrChanged(_, _ []schema.Attr) bool { return false }

// viewChain returns a schema holding a "base" <- "mid" <- "top" view chain,
// where each view selects from the previous one, along with its views.
func viewChain(name, baseDef string) (*schema.Schema, *schema.View, *schema.View, *schema.View) {
	var (
		s    = schema.New(name)
		base = schema.NewView("base", baseDef).SetSchema(s)
		mid  = schema.NewView("mid", "SELECT * FROM base").SetSchema(s)
		top  = schema.NewView("top", "SELECT * FROM mid").SetSchema(s)
	)
	mid.AddDeps(base)
	top.AddDeps(mid)
	s.AddViews(base, mid, top)
	return s, base, mid, top
}

func TestDiff_ViewRecreateDependents(t *testing.T) {
	d := &Diff{DiffDriver: &mockDiffDriver{}}
	t.Run("base definition changed", func(t *testing.T) {
		from, base1, mid1, top1 := viewChain("public", "SELECT 1")
		to, base2, mid2, top2 := viewChain("public", "SELECT 2")
		changes, err := d.SchemaDiff(from, to)
		require.NoError(t, err)
		// Only the base view was changed, but its dependents
		// cannot survive its drop, hence they are recreated.
		require.Equal(t, []schema.Change{
			&schema.DropView{V: base1}, &schema.AddView{V: base2},
			&schema.DropView{V: mid1}, &schema.AddView{V: mid2},
			&schema.DropView{V: top1}, &schema.AddView{V: top2},
		}, changes)
		require.Equal(t, mid1.Def, mid2.Def, "dependents are recreated as-is")
		require.Equal(t, top1.Def, top2.Def, "dependents are recreated as-is")
		// Drops are planned in the reverse dependency
		// order, and creations in the forward one.
		require.Equal(t, []schema.Change{
			&schema.DropView{V: top1}, &schema.DropView{V: mid1}, &schema.DropView{V: base1},
			&schema.AddView{V: base2}, &schema.AddView{V: mid2}, &schema.AddView{V: top2},
		}, SortChanges(changes, nil))
	})
	t.Run("base and dependent definitions changed", func(t *testing.T) {
		from, base1, mid1, top1 := viewChain("public", "SELECT 1")
		to, base2, mid2, top2 := viewChain("public", "SELECT 2")
		mid1.Def, mid2.Def = "SELECT * FROM base LIMIT 1", "SELECT * FROM base LIMIT 2"
		changes, err := d.SchemaDiff(from, to)
		require.NoError(t, err)
		// Both changed views are reported as pairs and not duplicated.
		require.Equal(t, []schema.Change{
			&schema.DropView{V: base1}, &schema.AddView{V: base2},
			&schema.DropView{V: mid1}, &schema.AddView{V: mid2},
			&schema.DropView{V: top1}, &schema.AddView{V: top2},
		}, changes)
		for _, c := range changes {
			_, ok := c.(*schema.ModifyView)
			require.False(t, ok, "definition changes are never reported as an atomic modification")
		}
		require.Equal(t, []schema.Change{
			&schema.DropView{V: top1}, &schema.DropView{V: mid1}, &schema.DropView{V: base1},
			&schema.AddView{V: base2}, &schema.AddView{V: mid2}, &schema.AddView{V: top2},
		}, SortChanges(changes, nil))
	})
	t.Run("comment-only change is kept in place", func(t *testing.T) {
		from, _, mid1, _ := viewChain("public", "SELECT 1")
		to, _, mid2, _ := viewChain("public", "SELECT 1")
		mid1.SetComment("old")
		mid2.SetComment("new")
		changes, err := d.SchemaDiff(from, to)
		require.NoError(t, err)
		// No view is dropped, hence no dependent is recreated.
		require.Equal(t, []schema.Change{
			&schema.ModifyView{From: mid1, To: mid2, Changes: []schema.Change{
				&schema.ModifyAttr{From: &schema.Comment{Text: "old"}, To: &schema.Comment{Text: "new"}},
			}},
		}, changes)
	})
	t.Run("comment-only change is subsumed by a recreation", func(t *testing.T) {
		from, base1, mid1, top1 := viewChain("public", "SELECT 1")
		to, base2, mid2, top2 := viewChain("public", "SELECT 2")
		mid1.SetComment("old")
		mid2.SetComment("new")
		changes, err := d.SchemaDiff(from, to)
		require.NoError(t, err)
		// The comment change of "mid" is replaced by its recreation, as
		// its comment is set again by the creation of the desired view.
		require.Equal(t, []schema.Change{
			&schema.DropView{V: base1}, &schema.AddView{V: base2},
			&schema.DropView{V: mid1}, &schema.AddView{V: mid2},
			&schema.DropView{V: top1}, &schema.AddView{V: top2},
		}, changes)
	})
	t.Run("deleted view recreates its dependents", func(t *testing.T) {
		from, base1, mid1, top1 := viewChain("public", "SELECT 1")
		to, _, mid2, top2 := viewChain("public", "SELECT 1")
		to.Views = to.Views[1:] // Delete the base view.
		changes, err := d.SchemaDiff(from, to)
		require.NoError(t, err)
		require.Equal(t, []schema.Change{
			&schema.DropView{V: base1},
			&schema.DropView{V: mid1}, &schema.AddView{V: mid2},
			&schema.DropView{V: top1}, &schema.AddView{V: top2},
		}, changes)
	})
	t.Run("dependents are not recreated twice", func(t *testing.T) {
		from, base1, mid1, top1 := viewChain("public", "SELECT 1")
		to, base2, mid2, top2 := viewChain("public", "SELECT 2")
		// "top" depends on both views of the chain.
		top1.AddDeps(base1)
		top2.AddDeps(base2)
		changes, err := d.SchemaDiff(from, to)
		require.NoError(t, err)
		require.Equal(t, []schema.Change{
			&schema.DropView{V: base1}, &schema.AddView{V: base2},
			&schema.DropView{V: mid1}, &schema.AddView{V: mid2},
			&schema.DropView{V: top1}, &schema.AddView{V: top2},
		}, changes)
	})
}

func TestDiff_ViewRecreateDependentsRealm(t *testing.T) {
	d := &Diff{DiffDriver: &mockDiffDriver{}}
	// The dependent view resides in another schema, and is
	// only visible to a realm-level diff.
	newRealm := func(baseDef string) (*schema.Realm, *schema.View, *schema.View) {
		var (
			s1   = schema.New("s1")
			s2   = schema.New("s2")
			base = schema.NewView("base", baseDef).SetSchema(s1)
			dep  = schema.NewView("dep", "SELECT * FROM s1.base").SetSchema(s2)
		)
		dep.AddDeps(base)
		s1.AddViews(base)
		s2.AddViews(dep)
		return schema.NewRealm(s1, s2), base, dep
	}
	from, base1, dep1 := newRealm("SELECT 1")
	to, base2, dep2 := newRealm("SELECT 2")
	changes, err := d.RealmDiff(from, to)
	require.NoError(t, err)
	require.Equal(t, []schema.Change{
		&schema.DropView{V: base1}, &schema.AddView{V: base2},
		&schema.DropView{V: dep1}, &schema.AddView{V: dep2},
	}, changes)
	require.Equal(t, []schema.Change{
		&schema.DropView{V: dep1}, &schema.DropView{V: base1},
		&schema.AddView{V: base2}, &schema.AddView{V: dep2},
	}, SortChanges(changes, nil))
	// A schema-level diff cannot see the dependent that resides
	// in another schema, and therefore does not recreate it.
	changes, err = d.SchemaDiff(from.Schemas[0], to.Schemas[0])
	require.NoError(t, err)
	require.Equal(t, []schema.Change{
		&schema.DropView{V: base1}, &schema.AddView{V: base2},
	}, changes)
}
