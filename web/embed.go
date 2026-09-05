// Package web carries the built React bundle into the binary. `all:` is
// deliberate: it keeps dist/.gitkeep embeddable, which is what lets `go build`
// succeed on a machine that has never run `npm run build`.
package web

import "embed"

//go:embed all:dist
var Dist embed.FS
