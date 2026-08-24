// Command browse renders an interactive terminal map, demonstrating the
// tiletea component. Clicking on the map drops a marker dot and shows the
// clicked coordinates in the status line.
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

	clicked    bool
	clickedLat float64
	clickedLng float64
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

	// The click callback ran inside the map's Update; if it recorded a new
	// click, move the marker there and re-render to show the dot.
	if _, isClick := msg.(tea.MouseClickMsg); isClick && a.clicked {
		lat, lng := a.clickedLat, a.clickedLng
		a.m.SetMarker(&lat, &lng)
		a.m.SetStatusExtra(fmt.Sprintf("Clicked: %.6f, %.6f", lat, lng))
		return a, tea.Batch(cmd, a.m.Refresh())
	}
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

	app := &app{incremental: true}
	m := tiletea.New(lat, lng, zoom,
		tiletea.WithLogger(logger),
		tiletea.WithClickCallback(func(clat, clng float64) {
			app.clicked = true
			app.clickedLat = clat
			app.clickedLng = clng
		}),
	)
	app.m = m

	p := tea.NewProgram(app)
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
