package catalog

import _ "embed"

// snapshotJSON is the build-time models.dev snapshot (generated; see
// snapshot.json). Embedding keeps cost rollups fully offline.
//
//go:embed snapshot.json
var snapshotJSON []byte
