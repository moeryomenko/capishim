// Command capishim is the setup container entrypoint for the capishim
// management stack.
package main

import (
	"flag"
	"fmt"
)

func main() {
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println("capishim dev")
	}
}
