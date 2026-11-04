// Command ledger is the agency's money database: the service on agency-de and the CLI of
// agents and the owner's Mac.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/goslingmanagment/ledger/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Main(ctx, os.Args[1:], os.Stderr)
	stop()
	os.Exit(code)
}
