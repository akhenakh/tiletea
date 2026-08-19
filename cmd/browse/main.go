// Command browse renders an interactive terminal map, demonstrating the
// tiletea component.
package main

import (
	"fmt"
	"log/slog"
	"os"

	tea "charm.land/bubbletea/v2"
	"github.com/akhenakh/tiletea"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if os.Getenv("DEBUG") != "" {
		f, err := tea.LogToFile("debug.log", "tiletea")
		if err != nil {
			fmt.Fprintln(os.Stderr, "fatal:", err)
			os.Exit(1)
		}
		defer f.Close()
		logger = slog.New(slog.NewTextHandler(f, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}

	lat, lng, zoom := 40.7128, -74.0060, 14

	m := tiletea.New(lat, lng, zoom,
		tiletea.WithMarker(lat, lng),
		tiletea.WithLogger(logger),
	)

	p := tea.NewProgram(m)
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
