/*
Copyright 2018 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package typed

import (
	"fmt"
	"reflect"

	"sigs.k8s.io/structured-merge-diff/fieldpath"
	"sigs.k8s.io/structured-merge-diff/schema"
	"sigs.k8s.io/structured-merge-diff/value"
)

// AsTyped accepts a value and a type and returns a TypedValue. 'v' must have
// type 'typeName' in the schema. An error is returned if the v doesn't conform
// to the schema.
func AsTyped(v value.Value, s *schema.Schema, typeRef schema.TypeRef) (*TypedValue, error) {
	tv := &TypedValue{
		value:   v,
		typeRef: typeRef,
		schema:  s,
	}
	if err := tv.Validate(); err != nil {
		return nil, err
	}
	return tv, nil
}

// AsTypeUnvalidated is just like AsTyped, but doesn't validate that the type
// conforms to the schema, for cases where that has already been checked or
// where you're going to call a method that validates as a side-effect (like
// ToFieldSet).
func AsTypedUnvalidated(v value.Value, s *schema.Schema, typeRef schema.TypeRef) *TypedValue {
	tv := &TypedValue{
		value:   v,
		typeRef: typeRef,
		schema:  s,
	}
	return tv
}

// TypedValue is a value of some specific type.
type TypedValue struct {
	value   value.Value
	typeRef schema.TypeRef
	schema  *schema.Schema
}

// AsValue removes the type from the TypedValue and only keeps the value.
func (tv TypedValue) AsValue() *value.Value {
	return &tv.value
}

// Validate returns an error with a list of every spec violation.
func (tv TypedValue) Validate() error {
	if errs := tv.walker().validate(); len(errs) != 0 {
		return errs
	}
	return nil
}

// ToFieldSet creates a set containing every leaf field and item mentioned, or
// validation errors, if any were encountered.
func (tv TypedValue) ToFieldSet() (*fieldpath.Set, error) {
	s := fieldpath.NewSet()
	w := tv.walker()
	w.leafFieldCallback = func(p fieldpath.Path) { s.Insert(p) }
	w.nodeFieldCallback = func(p fieldpath.Path) { s.Insert(p) }
	if errs := w.validate(); len(errs) != 0 {
		return nil, errs
	}
	return s, nil
}

// Merge returns the result of merging tv and pso ("partially specified
// object") together. Of note:
//  * No fields can be removed by this operation.
//  * If both tv and pso specify a given leaf field, the result will keep pso's
//    value.
//  * Container typed elements will have their items ordered:
//    * like tv, if pso doesn't change anything in the container
//    * like pso, if pso does change something in the container.
// tv and pso must both be of the same type (their Schema and TypeRef must
// match), or an error will be returned. Validation errors will be returned if
// the objects don't conform to the schema.
func (tv TypedValue) Merge(pso *TypedValue) (*TypedValue, error) {
	return merge(&tv, pso, ruleKeepRHS, nil)
}

// Compare compares the two objects. See the comments on the `Comparison`
// struct for details on the return value.
//
// tv and rhs must both be of the same type (their Schema and TypeRef must
// match), or an error will be returned. Validation errors will be returned if
// the objects don't conform to the schema.
func (tv TypedValue) Compare(rhs *TypedValue) (c *Comparison, err error) {
	c = &Comparison{
		Removed:  fieldpath.NewSet(),
		Modified: fieldpath.NewSet(),
		Added:    fieldpath.NewSet(),
	}
	c.Merged, err = merge(&tv, rhs, func(w *mergingWalker) {
		if w.lhs == nil {
			c.Added.Insert(w.path)
		} else if w.rhs == nil {
			c.Removed.Insert(w.path)
		} else if !reflect.DeepEqual(w.rhs, w.lhs) {
			// TODO: reflect.DeepEqual is not sufficient for this.
			// Need to implement equality check on the value type.
			c.Modified.Insert(w.path)
		}

		ruleKeepRHS(w)
	}, func(w *mergingWalker) {
		if w.lhs == nil {
			c.Added.Insert(w.path)
		} else if w.rhs == nil {
			c.Removed.Insert(w.path)
		}
	})
	if err != nil {
		return nil, err
	}

	return c, nil
}

// RemoveItems removes each provided list or map item from the value.
func (tv TypedValue) RemoveItems(items *fieldpath.Set) *TypedValue {
	tv.value, _ = value.FromUnstructured(tv.value.ToUnstructured(true))
	removeItemsWithSchema(&tv.value, items, tv.schema, tv.typeRef)
	return &tv
}

// NormalizeUnions takes the new object and normalizes the union:
// - If there is a discriminator and its value has changed, clean all
// fields but the one specified by the discriminator
// - If there is no discriminator, or it hasn't changed, if new has two
// of the fields set, remove the one that was set in old.
// - If there is a discriminator, set it to the value we've kept (if it changed)
//
// This can fail if:
// - Multiple new fields are set,
// - The discriminator is changed, and at least one new field is set.
func (tv TypedValue) NormalizeUnions(new *TypedValue) (*TypedValue, error) {
	var errs ValidationErrors
	var normalizeFn = func(w *mergingWalker) {
		if err := normalizeUnion(w); err != nil {
			errs = append(errs, w.error(err)...)
		}
	}
	out, mergeErrs := merge(&tv, new, func(w *mergingWalker) {
		if w.rhs != nil {
			v := *w.rhs
			w.out = &v
		}
	}, normalizeFn)
	if mergeErrs != nil {
		errs = append(errs, mergeErrs.(ValidationErrors)...)
	}
	if len(errs) > 0 {
		return nil, errs
	}
	return out, nil
}

func merge(lhs, rhs *TypedValue, rule, postRule mergeRule) (*TypedValue, error) {
	if lhs.schema != rhs.schema {
		return nil, errorFormatter{}.
			errorf("expected objects with types from the same schema")
	}
	if !reflect.DeepEqual(lhs.typeRef, rhs.typeRef) {
		return nil, errorFormatter{}.
			errorf("expected objects of the same type, but got %v and %v", lhs.typeRef, rhs.typeRef)
	}

	mw := mergingWalker{
		lhs:          &lhs.value,
		rhs:          &rhs.value,
		schema:       lhs.schema,
		typeRef:      lhs.typeRef,
		rule:         rule,
		postItemHook: postRule,
	}
	errs := mw.merge()
	if len(errs) > 0 {
		return nil, errs
	}

	out := &TypedValue{
		schema:  lhs.schema,
		typeRef: lhs.typeRef,
	}
	if mw.out == nil {
		out.value = value.Value{Null: true}
	} else {
		out.value = *mw.out
	}
	return out, nil
}

// Comparison is the return value of a TypedValue.Compare() operation.
//
// No field will appear in more than one of the three fieldsets. If all of the
// fieldsets are empty, then the objects must have been equal.
type Comparison struct {
	// Merged is the result of merging the two objects, as explained in the
	// comments on TypedValue.Merge().
	Merged *TypedValue

	// Removed contains any fields removed by rhs (the right-hand-side
	// object in the comparison).
	Removed *fieldpath.Set
	// Modified contains fields present in both objects but different.
	Modified *fieldpath.Set
	// Added contains any fields added by rhs.
	Added *fieldpath.Set
}

// IsSame returns true if the comparison returned no changes (the two
// compared objects are similar).
func (c *Comparison) IsSame() bool {
	return c.Removed.Empty() && c.Modified.Empty() && c.Added.Empty()
}

// String returns a human readable version of the comparison.
func (c *Comparison) String() string {
	str := fmt.Sprintf("- Merged Object:\n%v\n", c.Merged.AsValue())
	if !c.Modified.Empty() {
		str += fmt.Sprintf("- Modified Fields:\n%v\n", c.Modified)
	}
	if !c.Added.Empty() {
		str += fmt.Sprintf("- Added Fields:\n%v\n", c.Added)
	}
	if !c.Removed.Empty() {
		str += fmt.Sprintf("- Removed Fields:\n%v\n", c.Removed)
	}
	return str
}
