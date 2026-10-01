// Command imagecachectl fills a node's image cache once, from a registry or
// from a docker archive. It is what provisioning calls before there is a
// Kubernetes to run the agent in.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/scality/image-cache/agent/internal/cli"
)

func main() {
	// A boot cache image is about a gigabyte, so an operator who changes
	// their mind should not have to wait for the download to finish. The
	// extraction publishes by rename, so an interrupted run leaves the
	// resource absent rather than half filled.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Which signal arrived decides the exit code, so a caller can tell a
	// timeout it set itself from an operator pressing Ctrl-C.
	stopped := make(chan os.Signal, 1)
	go func() {
		sig, ok := <-signals
		if !ok {
			return
		}
		// The next one goes back to the kernel's default action. Cancellation
		// does not reach a read blocked on a half-open connection, and an
		// operator pressing Ctrl-C a second time should not have to find
		// another shell to kill this.
		signal.Stop(signals)
		stopped <- sig
		cancel()
	}()

	code := cli.Run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	if code == cli.ExitInterrupted {
		select {
		case sig := <-stopped:
			code = 128 + int(sig.(syscall.Signal))
		default:
		}
	}
	os.Exit(code)
}
