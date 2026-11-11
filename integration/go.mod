// This is a separate Go module so the cinc-server-ng test dependency never
// reaches consumers of the root module (cinc-server-ng itself imports
// format/, which would otherwise make the two modules depend on each other).
module github.com/cinc-project/cinc-server-backup/integration

go 1.27.1

require (
	github.com/cinc-project/cinc-api v0.19.0
	github.com/cinc-project/cinc-server-backup v0.0.0
	github.com/cinc-project/cinc-server-ng v0.18.0
)

require (
	github.com/dustin/go-humanize v1.1.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/ncruces/go-strftime v1.1.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/spf13/cobra v1.10.2 // indirect
	github.com/spf13/pflag v1.0.9 // indirect
	golang.org/x/sys v0.49.0 // indirect
	modernc.org/libc v1.79.0 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.12.1 // indirect
	modernc.org/sqlite v1.61.0 // indirect
)

replace github.com/cinc-project/cinc-server-backup => ../
