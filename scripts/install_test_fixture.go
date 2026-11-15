//go:build ignore

// Native executable fixture built explicitly by install_test.sh.
package main

import (
	"fmt"
	"os"
	"time"
)

var version string

func main() {
	switch {
	case len(os.Args) == 3 && os.Args[1] == "print":
		if _, err := fmt.Printf("%s:%s\n", version, os.Args[2]); err != nil {
			os.Exit(1)
		}
	case len(os.Args) == 2 && os.Args[1] == "wait":
		if _, err := fmt.Printf("%s:ready\n", version); err != nil {
			os.Exit(1)
		}
		time.Sleep(time.Minute)
	default:
		os.Exit(2)
	}
}
