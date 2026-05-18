// Package scaffold ships the default project tree that `stagent
// init` copies into a user's repo. The tree (.stagent.yaml, the
// .stagent/ prompts and templates) is kept in the same directory
// as this file so the canonical paths used in docs
// (notes/architecture.md, decisions.md §15) and the embedded
// content stay in lockstep.
package scaffold

import "embed"

//go:embed .stagent.yaml
//go:embed all:.stagent
var FS embed.FS

// Path is the directory-prefix used for FS lookups. `init` walks
// every entry in FS and writes the target file under
// <project>/<RelPath(entry)>.
const Path = "."
