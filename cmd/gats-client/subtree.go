package main

import (
	"fmt"
	"reflect"
	"strings"
)

// changedSubtree returns the deepest path that contains every changed leaf: a
// lone change resolves to the struct it sits in, siblings to the struct they
// share, and unrelated changes to the root. It is what the client shows
// instead of the whole value.
func changedSubtree(paths []string) string {
	if len(paths) == 0 {
		return ""
	}

	// The leaf name itself is not part of the subtree holding it.
	shared := parentOf(paths[0])

	for _, path := range paths[1:] {
		shared = commonPrefix(shared, parentOf(path))
		if len(shared) == 0 {
			return ""
		}
	}

	return strings.Join(shared, "/")
}

func parentOf(path string) []string {
	elements := strings.Split(path, "/")

	return elements[:len(elements)-1]
}

func commonPrefix(a, b []string) []string {
	n := min(len(a), len(b))

	for i := range n {
		if a[i] != b[i] {
			return a[:i]
		}
	}

	return a[:n]
}

// valueAt returns the value at a slash-separated path of field names, which is
// the same addressing the tree uses. An empty path is the whole value.
func valueAt(root any, path string) (any, error) {
	v, err := concrete(reflect.ValueOf(root))
	if err != nil {
		return nil, err
	}

	if path == "" {
		return v.Interface(), nil
	}

	for _, name := range strings.Split(path, "/") {
		if v.Kind() != reflect.Struct {
			return nil, fmt.Errorf("%s is not a struct", v.Kind())
		}

		field := v.FieldByName(name)
		if !field.IsValid() || !field.CanInterface() {
			return nil, fmt.Errorf("no field %q", name)
		}

		if v, err = concrete(field); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
	}

	return v.Interface(), nil
}

// concrete follows pointers to the value underneath.
func concrete(v reflect.Value) (reflect.Value, error) {
	for v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return reflect.Value{}, fmt.Errorf("unset")
		}
		v = v.Elem()
	}

	if !v.IsValid() {
		return reflect.Value{}, fmt.Errorf("unset")
	}

	return v, nil
}
