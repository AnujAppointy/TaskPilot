package main

import (
	"fmt"
	"os"

	"taskpilot/internal/taskpilot"
)

func main() {
	if err := taskpilot.Run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
