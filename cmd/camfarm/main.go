// Command camfarm (CamFarm) runs a simulated camera fleet with a web dashboard.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	// Restore default signal handling as soon as the first signal arrives, so a
	// second Ctrl-C during a slow shutdown kills the process.
	go func() { <-ctx.Done(); stop() }()
	err := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	if err == nil {
		return
	}
	code := 1
	var ee *exitError
	if errors.As(err, &ee) {
		code = ee.code
		if ee.quiet {
			os.Exit(code)
		}
	}
	fmt.Fprintln(os.Stderr, "camfarm:", err)
	os.Exit(code)
}
