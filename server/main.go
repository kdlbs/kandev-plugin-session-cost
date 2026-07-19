// Command server is the backend for the kandev-session-cost plugin: it maps
// the chat composer's kandev session id to its agent/ACP transcript id via the
// Host data API, then runs the tokscale CLI to report the cost of the current
// session. It imports only the public pkg/pluginsdk surface — exactly what a
// third-party plugin author would.
package main

import "github.com/kandev/kandev/pkg/pluginsdk"

func main() {
	pluginsdk.Serve(newPlugin())
}
