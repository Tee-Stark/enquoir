// Command api runs the Enquoir HTTP server. Wiring added in a later phase.
package main

import (
	"log/slog"
	"os"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	logger.Info("enquoir api: not yet implemented")
}
