// Command hanse is the Hanse CLI client (docs/architecture.md section 12).
// It stores the Gateway URL and token in the user configuration directory
// and reads every capability, node, schema, and value from the Gateway.
package main

import (
	"context"
	"os"
	"os/signal"

	"github.com/achmadss/hanse/internal/cli"
)

func main() {
	// Ctrl+C ends a wait or a log stream without cancelling the operation
	// (docs/architecture.md section 12). The handler turns the cancellation
	// into a message and prints the operation ID.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	os.Exit(cli.Run(ctx, os.Args[1:], cli.Options{
		Stdin:  os.Stdin,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	}))
}
