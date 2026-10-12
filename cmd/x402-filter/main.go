// Command x402-filter is the Envoy ext_proc filter that enforces x402 payments in the
// data plane. The gRPC server arrives in Phase 1 (Gate Zero: a no-op filter that logs
// both phases); until then the binary only reports its version.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/amoghjay/x402_gateway/internal/buildinfo"
)

func main() {
	version := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *version {
		fmt.Println(buildinfo.Version)
		return
	}
	fmt.Fprintln(os.Stderr, "x402-filter: the ext_proc server is not built yet (ROADMAP Phase 1)")
	os.Exit(1)
}
