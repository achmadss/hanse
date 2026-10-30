// Command hanse-gateway is the Hanse Gateway server. This is a skeleton
// entry point; it only reports its version and usage.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/achmadss/hanse/internal/version"
)

func main() {
	var showVersion bool
	flag.BoolVar(&showVersion, "version", false, "print the version and exit")
	flag.Usage = usage
	flag.Parse()

	if showVersion {
		fmt.Printf("hanse-gateway %s\n", version.Version)
		return
	}
	usage()
	os.Exit(2)
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: hanse-gateway [--version]")
}
