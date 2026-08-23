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

// app wraps the map model to add a toggle between incremental and full
// rendering, so panning performance can be compared visually.
type app struct {
	m           *tiletea.Map
	incremental bool
}

func (a *app) Init() tea.Cmd { return a.m.Init() }

func (a *app) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if key, ok := msg.(tea.KeyMsg); ok && key.String() == "i" {
		a.incremental = !a.incremental
		a.m.SetIncremental(a.incremental)
		return a, nil
	}
	model, cmd := a.m.Update(msg)
	a.m = model.(*tiletea.Map)
	return a, cmd
}

func (a *app) View() tea.View {
	return a.m.View()
}

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

	p := tea.NewProgram(&app{m: m, incremental: true})
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
