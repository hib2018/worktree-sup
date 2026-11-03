package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/hib2018/worktree-sup/internal/wsup"
)

func main() { os.Exit(run()) }

func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := wsup.Run(ctx, os.Args[1:], os.Stdin, os.Stdout); err != nil {
		if errors.Is(err, wsup.ErrCanceled) || errors.Is(err, context.Canceled) {
			fmt.Fprintln(os.Stderr, "Canceled. Existing changes are preserved. Run wsup status to check the operation.")
			return 130
		}
		fmt.Fprintln(os.Stderr, "wsup:", err)
		return 1
	}
	return 0
}
