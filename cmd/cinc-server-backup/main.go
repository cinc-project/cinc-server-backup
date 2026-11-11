// Command cinc-server-backup backs up and restores Chef Infra Server and
// Cinc Server installations.
package main

import (
	"os"

	"github.com/cinc-project/cinc-server-backup/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
