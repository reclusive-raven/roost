// Command roost keeps every Claude Code and Codex session. See README.md.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/reclusive-raven/roost/internal/roost"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := roost.Main(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
