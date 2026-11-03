// Command sugar-it is the sugar-it daemon and command-line client.
package main

import (
	"os"

	"sugarintegration/internal/cli"
)

func main() {
	os.Exit(cli.Execute(os.Args[1:], os.Stdout, os.Stderr))
}
