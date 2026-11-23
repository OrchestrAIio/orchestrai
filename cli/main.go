// Command orchestrai (alias ork) is the OrchestrAI CLI.
package main

import (
	"os"

	"github.com/OrchestrAIio/orchestrai/cli/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args, os.Stdin, os.Stdout, os.Stderr))
}
