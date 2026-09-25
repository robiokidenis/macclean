// renderui dumps a TUI screen's View() as raw ANSI for screenshot
// generation without a pty. Development helper, not part of the shipped
// CLI: go run ./cmd/renderui <screen>
package main

import (
	"fmt"
	"os"

	"macclean/internal/tui"
)

func main() {
	name := "menu"
	if len(os.Args) > 1 {
		name = os.Args[1]
	}
	out := tui.ScreenshotView(name, 120, 40)
	if out == "" {
		fmt.Fprintln(os.Stderr, "unknown screen:", name)
		os.Exit(1)
	}
	os.Stdout.WriteString(out)
}
