package main

import (
	"log/slog"
	"os"

	"github.com/Saumya40-codes/virutal-lab/pkg/cli"
)

func main() {
	if err := cli.Run(os.Args[1:]); err != nil {
		slog.New(slog.NewJSONHandler(os.Stderr, nil)).Error("command_failed", "error", err)
		os.Exit(1)
	}
}
