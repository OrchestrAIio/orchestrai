// Command orchestrai (alias ork) is the OrchestrAI CLI. It is the same program
// as the module root; this path gives `go install …/cli/cmd/orchestrai` the
// binary name orchestrai.
package main

import (
	"os"

	"github.com/OrchestrAIio/orchestrai/cli/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args, os.Stdin, os.Stdout, os.Stderr))
}
