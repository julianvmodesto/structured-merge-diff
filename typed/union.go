/*
Copyright 2019 The Kubernetes Authors.

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

	"sigs.k8s.io/structured-merge-diff/schema"
	"sigs.k8s.io/structured-merge-diff/value"
)

func normalizeUnion(w *mergingWalker) error {
	atom, found := w.schema.Resolve(w.typeRef)
	if !found {
		panic(fmt.Sprintf("Unable to resolve schema in normalize union: %v/%v", w.schema, w.typeRef))
	}
	// Unions can only be in structures, and the struct must not have been removed
	if atom.Struct == nil || atom.Struct.Union == nil || w.out == nil {
		return nil
	}

	old := &value.Map{}
	if w.lhs != nil {
		old = w.lhs.MapValue
	}
	return newUnion(atom.Struct.Union).Normalize(old, w.rhs.MapValue, w.out.MapValue)
}

type discriminated string
type field string

type discriminatedNames struct {
	f2d map[field]discriminated
	d2f map[discriminated]field
}

func newDiscriminatedName(f2d map[field]discriminated) discriminatedNames {
	d2f := map[discriminated]field{}
	for key, value := range f2d {
		d2f[value] = key
	}
	return discriminatedNames{
		f2d: f2d,
		d2f: d2f,
	}
}

func (dn discriminatedNames) toField(d discriminated) field {
	if f, ok := dn.d2f[d]; ok {
		return f
	}
	return field(d)
}

func (dn discriminatedNames) toDiscriminated(f field) discriminated {
	if d, ok := dn.f2d[f]; ok {
		return d
	}
	return discriminated(f)
}

type discriminator struct {
	name string
}

func (d *discriminator) Set(m *value.Map, v discriminated) {
	if d == nil {
		return
	}
	m.Set(d.name, value.StringValue(string(v)))
}

func (d *discriminator) Get(m *value.Map) discriminated {
	if d == nil || m == nil {
		return ""
	}
	f, ok := m.Get(d.name)
	if !ok {
		return ""
	}
	if f.Value.StringValue == nil {
		return ""
	}
	return discriminated(*f.Value.StringValue)
}

type fieldsSet map[field]struct{}

// newFieldsSet returns a map of the fields that are part of the union and are set
// in the given map.
func newFieldsSet(m *value.Map, fields []field) fieldsSet {
	if m == nil {
		return nil
	}
	set := fieldsSet{}
	for _, f := range fields {
		if _, ok := m.Get(string(f)); ok {
			set.Add(f)
		}
	}
	return set
}

func (fs fieldsSet) Add(f field) {
	if fs == nil {
		fs = map[field]struct{}{}
	}
	fs[f] = struct{}{}
}

func (fs fieldsSet) One() *field {
	for f := range fs {
		return &f
	}
	return nil
}

func (fs fieldsSet) Has(f field) bool {
	_, ok := fs[f]
	return ok
}

func (fs fieldsSet) List() []field {
	fields := []field{}
	for f := range fs {
		fields = append(fields, f)
	}
	return fields
}

func (fs fieldsSet) Difference(o fieldsSet) fieldsSet {
	n := fieldsSet{}
	for f := range fs {
		if !o.Has(f) {
			n.Add(f)
		}
	}
	return n
}

type union struct {
	d  *discriminator
	dn discriminatedNames
	f  []field
}

func newUnion(su *schema.Union) *union {
	u := &union{}
	if su.Discriminator != nil {
		u.d = &discriminator{name: *su.Discriminator}
	}
	f2d := map[field]discriminated{}
	for _, f := range su.Fields {
		u.f = append(u.f, field(f.FieldName))
		f2d[field(f.FieldName)] = discriminated(f.DiscriminatedBy)
	}
	u.dn = newDiscriminatedName(f2d)
	return u
}

// clear removes all the fields in map that are part of the union, but
// the one we decided to keep.
func (u *union) clear(m *value.Map, f field) {
	for _, fieldName := range u.f {
		if field(fieldName) != f {
			m.Delete(string(fieldName))
		}
	}
}

func (u *union) Normalize(old, new, out *value.Map) error {
	os := newFieldsSet(old, u.f)
	ns := newFieldsSet(new, u.f)
	diff := ns.Difference(os)

	if len(ns) > 1 && len(diff) != 1 {
		return fmt.Errorf("unable to guess new discriminator: %v", diff)
	}

	discriminator := field("")
	if len(ns) == 1 {
		discriminator = *ns.One()
	} else if len(diff) == 1 {
		discriminator = *diff.One()
	}

	if u.d.Get(old) != u.d.Get(new) && u.d.Get(new) != "" {
		if len(diff) == 1 && u.d.Get(new) != u.dn.toDiscriminated(discriminator) {
			return fmt.Errorf("discriminator and field changed: %v/%v", discriminator, u.d.Get(new))
		}
		u.clear(out, u.dn.toField(u.d.Get(new)))
		return nil
	}

	if discriminator != "" {
		u.clear(out, discriminator)
		u.d.Set(out, u.dn.toDiscriminated(discriminator))
	}

	return nil
}
