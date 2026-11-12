// tsterm-gui is tsterm's desktop GUI, for Linux, macOS, and Windows: tabs
// of your sessions on a tstermd, and a terminal view of each, as the web UI
// has. It connects to tstermd over your tailnet, as a browser would.
//
// Usage:
//
//	tsterm-gui [HOST[:PORT]]
//
// With no HOST, it uses the one it last connected to, or asks.
//
// Run as a plain binary, it has a generic icon on macOS, where the Dock
// shows an app bundle's. To make tsterm.app, with its icon, use Fyne's
// packaging tool (see FyneApp.toml) on a Mac.
package main

import (
	_ "embed"
	"flag"
	"fmt"
	"os"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"

	"github.com/bradfitz/tsterm/internal/gui"
)

// icon is the app's icon: its windows' and taskbar's on Linux and
// Windows, and its bundle's on macOS (see FyneApp.toml).
//
//go:embed Icon.png
var icon []byte

func main() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: tsterm-gui [HOST[:PORT]]\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() > 1 {
		flag.Usage()
		os.Exit(2)
	}
	a := app.NewWithID("io.github.bradfitz.tsterm")
	a.SetIcon(fyne.NewStaticResource("Icon.png", icon))
	gui.Run(a, flag.Arg(0))
}
