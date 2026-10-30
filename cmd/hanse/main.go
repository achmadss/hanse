// Command hanse is the Hanse CLI client (docs/architecture.md section 12).
// It stores the Gateway URL and token in the user configuration directory
// and reads every capability, node, schema, and value from the Gateway.
package main

import (
	"context"
	"os"

	"github.com/achmadss/hanse/internal/cli"
)

func main() {
	os.Exit(cli.Run(context.Background(), os.Args[1:], cli.Options{
		Stdin:  os.Stdin,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	}))
}
