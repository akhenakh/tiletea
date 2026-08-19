// Command geom renders an interactive terminal map fitted to a geometry
// overlay, demonstrating the tiletea.GeomView component.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"

	tea "charm.land/bubbletea/v2"
	"github.com/akhenakh/tiletea"
)

func main() {
	geojson := flag.String("geojson", "", "GeoJSON file to display")
	wkt := flag.String("wkt", "", "WKT geometry to display")
	flag.Parse()

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

	var gv *tiletea.GeomView
	var err error

	switch {
	case *geojson != "":
		data, rerr := os.ReadFile(*geojson)
		if rerr != nil {
			fmt.Fprintln(os.Stderr, "fatal:", rerr)
			os.Exit(1)
		}
		gv, err = tiletea.NewGeomViewFromGeoJSON(data, tiletea.WithLogger(logger))
	case *wkt != "":
		gv, err = tiletea.NewGeomViewFromWKT(*wkt, tiletea.WithLogger(logger))
	default:
		gv, err = tiletea.NewGeomViewFromWKT(
			"POLYGON((-74.02 40.70, -74.00 40.70, -74.00 40.72, -74.02 40.72, -74.02 40.70))",
			tiletea.WithLogger(logger),
		)
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}

	p := tea.NewProgram(gv)
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
