// Command akcore is the Agent Kate orchestration core. It supervises agent and
// language-server subprocesses and exposes them to the agentkate UI over a
// local JSON-RPC bus. The UI normally spawns this binary itself.
//
// Invoked as `akcore mcp ...` it instead runs the Cooperation MCP stdio bridge
// (see runMCPBridge); the default invocation runs the core.
package main

import (
	"os"
)

// version is the akcore protocol/build version reported in the handshake.
// The default is overridden at build time via -ldflags "-X main.version=..."
// (see CMakeLists.txt) so it tracks MAJOR.MINOR.<commits-on-main>.
var version = "0.1.0"

func main() {
	// Desktop-launched runs inherit a minimal PATH that often omits user bin
	// dirs like ~/.local/bin where `claude` lives. Augment it before anything
	// else so subprocesses (claude, git, gh, ...) resolve the same way they
	// do under a terminal-launched dev build.
	augmentPath()

	// Subcommand dispatch: `akcore mcp` is the Cooperation MCP stdio bridge.
	if len(os.Args) > 1 && os.Args[1] == "mcp" {
		runMCPBridge(os.Args[2:])
		return
	}
	runCore()
}
