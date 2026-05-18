module github.com/farcloser/haustorium

go 1.25.6

require (
	// Testing dependencies
	github.com/containerd/nerdctl/mod/tigron v0.0.0-20260121031139-a630881afd01
	github.com/farcloser/agar v0.0.0-20260127201813-e4cfb90faa46
	// Runtime dependencies
	github.com/farcloser/primordium v0.0.0-20260128062542-c661940b809b
	github.com/urfave/cli/v3 v3.9.0
	gonum.org/v1/gonum v0.17.0
)

require (
	github.com/creack/pty v1.1.24 // indirect
	golang.org/x/sync v0.19.0 // indirect
	golang.org/x/sys v0.40.0 // indirect
	golang.org/x/term v0.39.0 // indirect
	golang.org/x/text v0.33.0 // indirect
)
