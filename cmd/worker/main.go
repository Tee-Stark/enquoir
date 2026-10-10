// Command worker runs the Enquoir Temporal worker. Wiring added in a later phase.
package main

import (
	"log/slog"
	"os"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	logger.Info("enquoir worker: not yet implemented")
}
