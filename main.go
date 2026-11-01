package main

import (
	"fmt"
	"os"

	"github.com/rustfs/migration/cmd"
	"github.com/rustfs/migration/internal/safeoutput"
)

func main() {
	if err := cmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", safeoutput.Escape(err.Error()))
		os.Exit(1)
	}
}
