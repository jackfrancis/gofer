// Command runtime is gofer's agent runtime: the workload an AEI substrate launches
// for each dispatched run. It is a one-line shim — the workload itself belongs to the
// backend that launches it (internal/runtime/aei), so gofer's domain packages stay
// substrate-agnostic and this binary carries no logic of its own.
package main

import "github.com/jackfrancis/gofer/internal/runtime/aei"

func main() { aei.RunWorkload() }
