// Command mmcli is a console Mattermost client.
package main

import (
	"os"

	"github.com/esivres/mmcli/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
