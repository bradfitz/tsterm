// tsterm-gui is tsterm's desktop GUI, for Linux, macOS, and Windows: tabs
// of your sessions on a tstermd, and a terminal view of each, as the web UI
// has. It connects to tstermd over your tailnet, as a browser would.
//
// Usage:
//
//	tsterm-gui [HOST[:PORT]]
//
// With no HOST, it uses the one it last connected to, or asks.
package main

import (
	"flag"
	"fmt"
	"os"

	"fyne.io/fyne/v2/app"

	"github.com/bradfitz/tsterm/internal/gui"
)

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
	gui.Run(app.NewWithID("io.github.bradfitz.tsterm"), flag.Arg(0))
}
