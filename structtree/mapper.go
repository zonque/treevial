package structtree

import (
	"fmt"
	"reflect"
)

// TagKey is the struct tag treevial reads, and TagLeaf the value that marks a
// field as a leaf:
//
//	type Config struct {
//		Installed time.Time `treevial:"leaf"`
//	}
//
// Tagging is the primary way to say what a leaf is. It is honoured before any
// rule a [Mapper] carries, so a tag cannot be overruled, and it puts the
// decision in the type itself where the fields are — which is the only place a
// reader of the struct will look for it.
//
// Any other value in the tag is ignored, leaving the structural rule to decide.
const (
	TagKey  = "treevial"
	TagLeaf = "leaf"
)

// IsTaggedLeaf reports whether field carries the tag that marks it as a leaf.
func IsTaggedLeaf(field reflect.StructField) bool {
	return field.Tag.Get(TagKey) == TagLeaf
}

// Mapper carries the three decisions that turn a Go value into a tree and back:
// which fields are leaves, how a leaf's value becomes bytes, and how those bytes
// become a value again.
//
// They travel together because they have to agree. A rule that keeps some
// struct whole — a time.Time, a type of your own — only works if the encoding
// knows what to do with it, and a value written by one encoding can only be
// read by its counterpart. Passing them separately at each call site would
// invite pairing them up wrongly.
//
// The zero Mapper uses the defaults and behaves exactly like the package-level
// [Walk], [Build], [Apply] and [ApplySince], which are shorthand for it.
type Mapper struct {
	// IsLeaf reports whether a field is stored as one blob rather than
	// descended into. Nil means [DefaultIsLeaf].
	//
	// It answers only for fields the tag says nothing about: a field
	// tagged `treevial:"leaf"` is a leaf whatever this returns.
	IsLeaf func(reflect.StructField) bool
	// Encode turns a leaf's value into the bytes of its blob. Nil means
	// [DefaultEncoder].
	Encode Encoder
	// Decode fills a leaf's value from the bytes of its blob. Nil means
	// [DefaultDecoder].
	Decode Decoder
}

func (m Mapper) isLeaf(field reflect.StructField) bool {
	// The tag has the first word, whatever rule is in force.
	if IsTaggedLeaf(field) {
		return true
	}

	if m.IsLeaf != nil {
		return m.IsLeaf(field)
	}

	return DefaultIsLeaf(field)
}

func (m Mapper) encode(v reflect.Value) ([]byte, error) {
	if m.Encode != nil {
		return m.Encode(v)
	}

	return DefaultEncoder(v)
}

func (m Mapper) decode(data []byte, v reflect.Value) error {
	if m.Decode != nil {
		return m.Decode(data, v)
	}

	return DefaultDecoder(data, v)
}

// DefaultIsLeaf reports whether a field is stored as one blob rather than
// descended into: anything tagged `treevial:"leaf"`, everything that is not a
// struct, and the structs that are protobuf messages.
//
// Tagging is the primary way to mark a leaf, and needs nothing from a caller.
// A rule is for what a tag cannot say — a type that should always be a leaf,
// wherever it appears, including in types you do not own:
//
//	m := structtree.Mapper{
//		IsLeaf: func(f reflect.StructField) bool {
//			return f.Type == reflect.TypeFor[time.Time]() ||
//				structtree.DefaultIsLeaf(f)
//		},
//	}
//
// DefaultIsLeaf is exported so such a rule can build on it rather than restate
// it.
func DefaultIsLeaf(field reflect.StructField) bool {
	return IsTaggedLeaf(field) || isLeafType(field.Type)
}

// unusableMap reports an error if field is a map that cannot be addressed by
// path and was not deliberately made a leaf.
//
// A map keyed by anything but a string has no path elements to offer, so it
// cannot become a subtree. Saying so is better than storing it whole and
// leaving someone to wonder why it has no paths under it — unless a tag or a
// rule of your own asked for exactly that, which is how you say you want it in
// one blob.
func (m Mapper) unusableMap(field reflect.StructField) error {
	t := field.Type
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}

	if t.Kind() != reflect.Map || t.Key().Kind() == reflect.String {
		return nil
	}

	if IsTaggedLeaf(field) || (m.IsLeaf != nil && m.IsLeaf(field)) {
		return nil
	}

	return fmt.Errorf("%s cannot be addressed by path: its keys are %s, not strings; tag it `%s:\"%s\"` to store it whole",
		t, t.Key(), TagKey, TagLeaf)
}

// unreadableLeaf reports an error if a leaf could be written but not read back.
//
// An interface says nothing about what it holds, so a protobuf message in one
// is encoded as a message and then met, on the way back, by a field that gives
// the decoder no message to unmarshal into. Anything else in an interface goes
// as JSON both ways and is fine.
func unreadableLeaf(field reflect.StructField, value reflect.Value) error {
	if field.Type.Kind() != reflect.Interface {
		return nil
	}

	if !isProtoMessage(value.Type()) {
		return nil
	}

	return fmt.Errorf("%s holds %s, which cannot be read back: an interface does not say which message to expect, so declare the field as %s",
		field.Type, value.Type(), reflect.PointerTo(value.Type()))
}

// unreadableBlob reports an error if a leaf would be stored whole by the
// default encoding while holding a protobuf message somewhere inside it.
//
// JSON writes such a message as the generated struct it is, oneof wrapper and
// all, and then has nothing to unmarshal that wrapper into. A message of its
// own goes as proto and is fine; a message inside something stored whole is
// not, and saying so is better than writing a blob nobody can read.
//
// An encoding of your own is presumed to know its business, so this only
// applies to the default one.
func (m Mapper) unreadableBlob(field reflect.StructField, t reflect.Type) error {
	if m.Encode != nil || isProtoMessage(t) {
		return nil
	}

	if !containsMessage(t, map[reflect.Type]bool{}) {
		return nil
	}

	return fmt.Errorf("%s holds a protobuf message, which cannot be stored whole by the default encoding: leave it untagged so each message gets a blob of its own, or give the Mapper an Encode and Decode that handle it",
		field.Type)
}

// containsMessage reports whether t holds a protobuf message anywhere inside.
// An interface is not followed, since what it holds is not known from the type.
func containsMessage(t reflect.Type, seen map[reflect.Type]bool) bool {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}

	if seen[t] {
		return false
	}
	seen[t] = true

	if isProtoMessage(t) {
		return true
	}

	switch t.Kind() {
	case reflect.Struct:
		for i := range t.NumField() {
			if containsMessage(t.Field(i).Type, seen) {
				return true
			}
		}

	case reflect.Map:
		return containsMessage(t.Key(), seen) || containsMessage(t.Elem(), seen)

	case reflect.Slice, reflect.Array:
		return containsMessage(t.Elem(), seen)
	}

	return false
}
