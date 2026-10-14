package sample

import _ "embed"

// ProtoSource is the sample.proto schema, embedded so the export tooling can
// ship it inside the tarball and consumers can regenerate a decoder.
//
//go:embed sample.proto
var ProtoSource string
