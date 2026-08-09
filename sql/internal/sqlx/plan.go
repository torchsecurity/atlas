// Copyright 2021-present The Atlas Authors. All rights reserved.
// This source code is licensed under the Apache 2.0 license found
// in the LICENSE file in the root directory of this source tree.

package sqlx

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"sort"

	"ariga.io/atlas/sql/migrate"
	"ariga.io/atlas/sql/schema"
)

type (
	execPlanner interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
		PlanChanges(context.Context, string, []schema.Change, ...migrate.PlanOption) (*migrate.Plan, error)
	}
	// ApplyError is an error that exposes an information for getting
	// how any changes were applied before encountering the failure.
	ApplyError struct {
		err     string
		applied int
	}
)

// Applied reports how many changes were applied before getting an error.
// In case the first change was failed, Applied() returns 0.
func (e *ApplyError) Applied() int {
	return e.applied
}

// Error implements the error interface.
func (e *ApplyError) Error() string {
	return e.err
}

// ApplyChanges is a helper used by the different drivers to apply changes.
func ApplyChanges(ctx context.Context, changes []schema.Change, p execPlanner, opts ...migrate.PlanOption) error {
	plan, err := p.PlanChanges(ctx, "apply", changes, opts...)
	if err != nil {
		return err
	}
	for i, c := range plan.Changes {
		if _, err := p.ExecContext(ctx, c.Cmd, c.Args...); err != nil {
			if c.Comment != "" {
				err = fmt.Errorf("%s: %w", c.Comment, err)
			}
			return &ApplyError{err: err.Error(), applied: i}
		}
	}
	return nil
}

// noRows implements the schema.ExecQuerier for migrate.Driver's without connections.
// This can be useful to always return no rows for queries, and block any execution.
type noRows struct{}

// QueryContext implements the sqlx.ExecQuerier interface.
func (*noRows) QueryContext(context.Context, string, ...interface{}) (*sql.Rows, error) {
	return nil, sql.ErrNoRows
}

// ExecContext implements the sqlx.ExecQuerier interface.
func (*noRows) ExecContext(context.Context, string, ...interface{}) (sql.Result, error) {
	return nil, errors.New("cannot execute statements without a database connection. use Open to create a new Driver")
}

// NoRows to be used by differs and planners without a connection.
var NoRows schema.ExecQuerier = (*noRows)(nil)

// SetReversible sets the Reversible field to
// true if all planned changes are reversible.
func SetReversible(p *migrate.Plan) error {
	reversible := true
	for _, c := range p.Changes {
		stmts, err := c.ReverseStmts()
		if err != nil {
			return err
		}
		if len(stmts) == 0 {
			reversible = false
		}
	}
	p.Reversible = reversible
	return nil
}

// DetachCycles takes a list of schema changes, and detaches
// references between changes if there is at least one circular
// reference in the changeset. More explicitly, it postpones fks
// creation, or deletes fks before deletes their tables.
func DetachCycles(changes []schema.Change) ([]schema.Change, error) {
	sorted, err := sortMap(changes)
	if errors.Is(err, errCycle) {
		return detachReferences(changes), nil
	}
	if err != nil {
		return nil, err
	}
	planned := make([]schema.Change, len(changes))
	copy(planned, changes)
	sort.Slice(planned, func(i, j int) bool {
		return sorted[table(planned[i])] < sorted[table(planned[j])]
	})
	return planned, nil
}

// detachReferences detaches all table references.
func detachReferences(changes []schema.Change) []schema.Change {
	var planned, deferred []schema.Change
	for _, change := range changes {
		switch change := change.(type) {
		case *schema.AddTable:
			var (
				ext  []schema.Change
				self []*schema.ForeignKey
			)
			for _, fk := range change.T.ForeignKeys {
				if fk.RefTable == change.T {
					self = append(self, fk)
				} else {
					ext = append(ext, &schema.AddForeignKey{F: fk})
				}
			}
			if len(ext) > 0 {
				deferred = append(deferred, &schema.ModifyTable{T: change.T, Changes: ext})
				t := *change.T
				t.ForeignKeys = self
				change = &schema.AddTable{T: &t, Extra: change.Extra}
			}
			planned = append(planned, change)
		case *schema.DropTable:
			var fks []schema.Change
			for _, fk := range change.T.ForeignKeys {
				if fk.RefTable != change.T {
					fks = append(fks, &schema.DropForeignKey{F: fk})
				}
			}
			if len(fks) > 0 {
				planned = append(planned, &schema.ModifyTable{T: change.T, Changes: fks})
				t := *change.T
				t.ForeignKeys = nil
				change = &schema.DropTable{T: &t, Extra: change.Extra}
			}
			deferred = append(deferred, change)
		case *schema.ModifyTable:
			var fks, rest []schema.Change
			for _, c := range change.Changes {
				switch c := c.(type) {
				case *schema.AddForeignKey:
					fks = append(fks, c)
				default:
					rest = append(rest, c)
				}
			}
			if len(fks) > 0 {
				deferred = append(deferred, &schema.ModifyTable{T: change.T, Changes: fks})
			}
			if len(rest) > 0 {
				planned = append(planned, &schema.ModifyTable{T: change.T, Changes: rest})
			}
		default:
			planned = append(planned, change)
		}
	}
	return append(planned, deferred...)
}

// errCycle is an internal error to indicate a case of a cycle.
var errCycle = errors.New("cycle detected")

// sortMap returns an index-map indicates the position of table in a topological
// sort in reversed order based on its references, and a boolean indicate if there
// is a non-self loop.
func sortMap(changes []schema.Change) (map[string]int, error) {
	var (
		visit     func(string) bool
		sorted    = make(map[string]int)
		progress  = make(map[string]bool)
		deps, err = dependencies(changes)
	)
	if err != nil {
		return nil, err
	}
	visit = func(name string) bool {
		if _, done := sorted[name]; done {
			return false
		}
		if progress[name] {
			return true
		}
		progress[name] = true
		for _, ref := range deps[name] {
			if visit(ref.Name) {
				return true
			}
		}
		delete(progress, name)
		sorted[name] = len(sorted)
		return false
	}
	for _, node := range byKeys(deps) {
		if visit(node.K) {
			return nil, errCycle
		}
	}
	return sorted, nil
}

// dependencies returned an adjacency list of all tables and the tables they depend on.
func dependencies(changes []schema.Change) (map[string][]*schema.Table, error) {
	deps := make(map[string][]*schema.Table)
	for _, change := range changes {
		switch change := change.(type) {
		case *schema.AddTable:
			for _, fk := range change.T.ForeignKeys {
				if err := checkFK(fk); err != nil {
					return nil, err
				}
				if fk.RefTable != change.T {
					deps[change.T.Name] = append(deps[change.T.Name], fk.RefTable)
				}
			}
		case *schema.DropTable:
			for _, fk := range change.T.ForeignKeys {
				if err := checkFK(fk); err != nil {
					return nil, err
				}
				if isDropped(changes, fk.RefTable) {
					deps[fk.RefTable.Name] = append(deps[fk.RefTable.Name], fk.Table)
				}
			}
		case *schema.ModifyTable:
			for _, c := range change.Changes {
				switch c := c.(type) {
				case *schema.AddForeignKey:
					if err := checkFK(c.F); err != nil {
						return nil, err
					}
					if c.F.RefTable != change.T {
						deps[change.T.Name] = append(deps[change.T.Name], c.F.RefTable)
					}
				case *schema.ModifyForeignKey:
					if err := checkFK(c.To); err != nil {
						return nil, err
					}
					if c.To.RefTable != change.T {
						deps[change.T.Name] = append(deps[change.T.Name], c.To.RefTable)
					}
				case *schema.DropForeignKey:
					if err := checkFK(c.F); err != nil {
						return nil, err
					}
					if isDropped(changes, c.F.RefTable) {
						deps[c.F.RefTable.Name] = append(deps[c.F.RefTable.Name], c.F.Table)
					}
				}
			}
		}
	}
	return deps, nil
}

func checkFK(fk *schema.ForeignKey) error {
	var cause []string
	if fk.Table == nil {
		cause = append(cause, "child table")
	}
	if len(fk.Columns) == 0 {
		cause = append(cause, "child columns")
	}
	if fk.RefTable == nil {
		cause = append(cause, "parent table")
	}
	if len(fk.RefColumns) == 0 {
		cause = append(cause, "parent columns")
	}
	if len(cause) != 0 {
		return fmt.Errorf("missing %q for foreign key: %q", cause, fk.Symbol)
	}
	return nil
}

// table extracts a table from the given change.
func table(change schema.Change) (t string) {
	switch change := change.(type) {
	case *schema.AddTable:
		t = change.T.Name
	case *schema.DropTable:
		t = change.T.Name
	case *schema.ModifyTable:
		t = change.T.Name
	}
	return
}

// isDropped checks if the given table is marked as a deleted in the changeset.
func isDropped(changes []schema.Change, t *schema.Table) bool {
	for _, c := range changes {
		if c, ok := c.(*schema.DropTable); ok && c.T.Name == t.Name {
			return true
		}
	}
	return false
}

// CheckChangesScope checks that changes can be applied
// on a schema scope (connection).
func CheckChangesScope(opts migrate.PlanOptions, changes []schema.Change) error {
	names := make(map[string]struct{})
	for _, c := range changes {
		var t *schema.Table
		switch c := c.(type) {
		case *schema.ModifySchema:
			switch scope := V(opts.SchemaQualifier); {
			case !opts.Mode.Is(migrate.PlanModeInPlace):
				// The migration plan is generated for deferred execution.
				return fmt.Errorf("%T is not allowed when migration plan is scoped to one schema", c)
			case scope != "" && scope != c.S.Name:
				// Other schemas can not be modified when the migration plan is scoped to one schema.
				return fmt.Errorf("modify schema %s is not allowed when migration plan is scoped to schema %s", c.S.Name, scope)
			default:
				names[c.S.Name] = struct{}{}
				continue
			}
		case *schema.AddSchema, *schema.DropSchema:
			return fmt.Errorf("%T is not allowed when migration plan is scoped to one schema", c)
		case *schema.AddTable:
			t = c.T
		case *schema.ModifyTable:
			t = c.T
		case *schema.DropTable:
			t = c.T
		default:
			continue
		}
		if t.Schema != nil && t.Schema.Name != "" {
			names[t.Schema.Name] = struct{}{}
		}
		for _, c := range t.Columns {
			e, ok := c.Type.Type.(*schema.EnumType)
			if ok && e.Schema != nil && e.Schema.Name != "" {
				names[t.Schema.Name] = struct{}{}
			}
		}
	}
	if len(names) > 1 {
		ks := make([]string, 0, len(names))
		for k := range names {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		return fmt.Errorf("found %d schemas when migration plan is scoped to one: %q", len(names), ks)
	}
	return nil
}

// byKeys sorts a map by keys.
func byKeys[T any](m map[string]T) []struct {
	K string
	V T
} {
	vs := make([]struct {
		K string
		V T
	}, 0, len(m))
	for k, v := range m {
		vs = append(vs, struct {
			K string
			V T
		}{k, v})
	}
	sort.Slice(vs, func(i, j int) bool {
		return vs[i].K < vs[j].K
	})
	return vs
}

// SortOptions allows drivers to customize the behavior of the SortChanges function.
type SortOptions struct {
	// DefaultSchema defines the default schema (also known as "search_path") that
	// is used by the database to search for objects if no qualifier is provided.
	DefaultSchema string
}

// SortChanges is a helper function to sort to level changes based on their priority.
func SortChanges(changes []schema.Change, opts *SortOptions) []schema.Change {
	var views, drop, other []schema.Change
	for _, c := range changes {
		switch c.(type) {
		case *schema.AddView, *schema.DropView, *schema.ModifyView:
			views = append(views, c)
		case *schema.DropSchema, *schema.DropTable, *schema.DropObject:
			drop = append(drop, c)
		default:
			other = append(other, c)
		}
	}
	if planned, err := sortViewChanges(views); err == nil { // no cycles.
		views = planned
	}
	// To keep backwards compatibility with previous sorting and also in case we miss any dependency between changes
	// (see, dependsOn function) we push views and drop changes to the end, unless there is a dependency requirement.
	changes = append(other, append(views, drop...)...)
	var (
		hasE  = make(map[struct{ e1, e2 schema.Change }]bool)
		edges = make(map[schema.Change][]schema.Change)
	)
	for _, c1 := range changes {
		for _, c2 := range changes {
			// Skip checking dependencies between the same change. Also, if the inverse
			// dependency is already added, skip it, as circular dependencies are not expected.
			if c1 != c2 && !hasE[struct{ e1, e2 schema.Change }{c2, c1}] && dependsOn(c1, c2, V(opts)) {
				edges[c1] = append(edges[c1], c2)
				hasE[struct{ e1, e2 schema.Change }{c1, c2}] = true
			}
		}
	}
	var (
		add     func(schema.Change)
		added   = make(map[schema.Change]bool)
		planned = make([]schema.Change, 0, len(changes))
	)
	add = func(c schema.Change) {
		if added[c] {
			return
		}
		added[c] = true
		for _, d := range edges[c] {
			if !added[d] {
				add(d)
			}
		}
		planned = append(planned, c)
	}
	for _, c := range changes {
		if !added[c] {
			add(c)
		}
	}
	return planned
}

// sortViewChanges sorts the given view changes topologically by their dependencies:
// a view is created (or modified) only after the views it depends on, and dropped
// before them - i.e. drops are emitted in the reverse dependency order. Changes that
// do not depend on each other keep their input order, keeping the result deterministic.
// An error is returned in case the dependencies form a cycle.
func sortViewChanges(changes []schema.Change) ([]schema.Change, error) {
	const (
		unvisited = iota
		visiting
		visited
	)
	var (
		state   = make([]int, len(changes))
		planned = make([]schema.Change, 0, len(changes))
		visit   func(int) error
	)
	visit = func(i int) error {
		switch state[i] {
		case visited:
			return nil
		case visiting:
			return fmt.Errorf("sqlx: cyclic dependency between view changes")
		}
		state[i] = visiting
		for j := range changes {
			if i == j || state[j] == visited || !viewChangeDepends(changes[i], changes[j]) {
				continue
			}
			if err := visit(j); err != nil {
				return err
			}
		}
		state[i] = visited
		planned = append(planned, changes[i])
		return nil
	}
	for i := range changes {
		if state[i] != unvisited {
			continue
		}
		if err := visit(i); err != nil {
			return nil, err
		}
	}
	return planned, nil
}

// viewChangeDepends reports if the view change c1 must be planned after c2.
func viewChangeDepends(c1, c2 schema.Change) bool {
	switch c1 := c1.(type) {
	case *schema.AddView:
		return addViewDepends(c1.V, c2)
	case *schema.ModifyView:
		return addViewDepends(c1.To, c2)
	case *schema.DropView:
		// A view is dropped only after the views that depend on it were dropped.
		c2, ok := c2.(*schema.DropView)
		return ok && viewDependsOn(c2.V, c1.V)
	}
	return false
}

// addViewDepends reports if the creation (or modification) of
// the given view must be planned after the given change.
func addViewDepends(v *schema.View, c schema.Change) bool {
	switch c := c.(type) {
	case *schema.AddView:
		return viewDependsOn(v, c.V)
	case *schema.ModifyView:
		return viewDependsOn(v, c.To)
	case *schema.DropView:
		// Recreating a view (e.g., switching it to materialized and vice versa)
		// occurs after its drop, and so does the recreation of a view that
		// dropped views depend on.
		return SameView(v, c.V) || viewDependsOn(c.V, v)
	}
	return false
}

// viewDependsOn reports if v1 depends on v2.
func viewDependsOn(v1, v2 *schema.View) bool {
	return slices.ContainsFunc(v1.Deps, func(o schema.Object) bool {
		d, ok := o.(*schema.View)
		return ok && SameView(d, v2)
	})
}

type (
	// Depender can be implemented by an object to determine if a change to it
	// depends on other change, or if other change depends on it. For example:
	// A table creation depends on type creation, and a type deletion depends on
	// table deletion.
	Depender interface {
		DependsOn(change, other schema.Change) bool
		DependencyOf(change, other schema.Change) bool
	}
	// RowTyper can be implemented by a type to determine if its source
	// is a regular table (e.g., row types).
	RowTyper interface {
		RowTypeT() *schema.Table
	}
)

// dependOnOf checks if the given change depends on the other change or
// vice versa based on their underlying object implementation.
func dependOnOf(change, other schema.Change) bool {
	switch change := change.(type) {
	case *schema.AddObject:
		if d, ok := change.O.(Depender); ok && d.DependsOn(change, other) {
			return true
		}
	case *schema.ModifyObject:
		if d, ok := change.To.(Depender); ok && d.DependsOn(change, other) {
			return true
		}
	case *schema.DropObject:
		if d, ok := change.O.(Depender); ok && d.DependsOn(change, other) {
			return true
		}
	}
	switch other := other.(type) {
	case *schema.AddObject:
		if d, ok := other.O.(Depender); ok && d.DependencyOf(other, change) {
			return true
		}
	case *schema.ModifyObject:
		if d, ok := other.To.(Depender); ok && d.DependencyOf(other, change) {
			return true
		}
	case *schema.DropObject:
		if d, ok := other.O.(Depender); ok && d.DependencyOf(other, change) {
			return true
		}
	}
	return false
}

// depOfDrops checks if the given object is a dependency of the given change.
func depOfDrop(o schema.Object, c schema.Change) bool {
	var deps []schema.Object
	switch c := c.(type) {
	case *schema.DropTable:
		deps = c.T.Deps
	case *schema.DropView:
		deps = c.V.Deps
	}
	return slices.Contains(deps, o)
}

// depOfAdd checks if the given change is a creation of a resource exists in the given list.
func depOfAdd(refs []schema.Object, c schema.Change) bool {
	var o schema.Object
	switch c := c.(type) {
	case *schema.AddTable:
		return slices.ContainsFunc(refs, func(o schema.Object) bool {
			t, ok := o.(*schema.Table)
			return ok && SameTable(c.T, t)
		})
	case *schema.ModifyTable:
		return slices.ContainsFunc(refs, func(o schema.Object) bool {
			t, ok := o.(*schema.Table)
			return ok && SameTable(c.T, t)
		})
	case *schema.AddView:
		return slices.ContainsFunc(refs, func(o schema.Object) bool {
			v, ok := o.(*schema.View)
			return ok && SameView(c.V, v)
		})
	case *schema.ModifyView:
		return slices.ContainsFunc(refs, func(o schema.Object) bool {
			v, ok := o.(*schema.View)
			return ok && SameView(c.To, v)
		})
	case *schema.AddObject:
		o = c.O
	default:
		return false
	}
	return slices.Contains(refs, o)
}

// refTo reports if the given foreign keys reference the given table.
func refTo(fks []*schema.ForeignKey, to *schema.Table) bool {
	return slices.ContainsFunc(fks, func(fk *schema.ForeignKey) bool {
		return SameTable(fk.RefTable, to)
	})
}

// typeDependsOnT reports if the declaration of type "t" depends on the table.
func typeDependsOnT(t schema.Type, tt *schema.Table) bool {
	rt, ok := schema.UnderlyingType(t).(RowTyper)
	if !ok {
		return false
	}
	rowT := rt.RowTypeT()
	return rowT != nil && SameTable(rowT, tt)
}

// SameView reports if the two objects represent the same view.
func SameView(v1, v2 *schema.View) bool {
	if v1 == nil || v2 == nil {
		return v1 == v2
	}
	return v1.Name == v2.Name && SameSchema(v1.Schema, v2.Schema)
}

// SameTable reports if the two objects represent the same table.
func SameTable(t1, t2 *schema.Table) bool {
	if t1 == nil || t2 == nil {
		return t1 == t2
	}
	return t1.Name == t2.Name && SameSchema(t1.Schema, t2.Schema)
}

// SameSchema reports if the given schemas are the same.
// Objects can be different as they might reside in two
// different states (current and desired).
func SameSchema(s1, s2 *schema.Schema) bool {
	if s1 == nil || s2 == nil {
		return s1 == s2
	}
	return s1.Name == s2.Name
}
