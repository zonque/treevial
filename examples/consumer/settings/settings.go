// Package settings holds the baseline struct the two sides share.
//
// This is the part that has to be common: the server walks this type into a
// tree, and the client applies the tree back into the same type. When the two
// sides live in different repositories, a package like this one is what they
// both depend on — alongside treevial itself.
package settings

import "google.golang.org/protobuf/types/known/timestamppb"

// Settings is the value being synchronised. Its field names become the paths
// in the git tree: Owner/Name, Display/Brightness, and so on. Seen is a
// protobuf message, so it is one leaf rather than a subtree.
type Settings struct {
	Owner   Owner
	Display Display
	Seen    *timestamppb.Timestamp
}

// Owner names who the unit belongs to.
type Owner struct {
	Name string
	Team string
}

// Display is nested one level down.
type Display struct {
	Brightness int
	Rotation   int
}
