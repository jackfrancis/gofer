module github.com/jackfrancis/gofer

go 1.26.0

require (
	github.com/jackfrancis/agent-execution-interface/aeiruntime v0.0.0-00010101000000-000000000000
	github.com/jackfrancis/agent-execution-interface/sdks/go v0.0.0-00010101000000-000000000000
	github.com/microcosm-cc/bluemonday v1.0.27
	github.com/prometheus/client_golang v1.24.0
	github.com/yuin/goldmark v1.8.4
	golang.org/x/oauth2 v0.36.0
)

require (
	cloud.google.com/go/compute/metadata v0.3.0 // indirect
	github.com/aymerick/douceur v0.2.0 // indirect
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/gorilla/css v1.0.1 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/prometheus/client_model v0.6.2 // indirect
	github.com/prometheus/common v0.70.0 // indirect
	github.com/prometheus/procfs v0.21.1 // indirect
	golang.org/x/net v0.56.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
)

// The agent-runtime backend (internal/runtime/aei) is built on the Agent Execution
// Interface, consumed as a local sibling checkout until it is published. gofer imports
// only AEI's stdlib-only SDK halves: the app SDK (sdks/go/aeiapp) to dispatch runs to
// the pre-installed control plane over HTTP/JSON, and the runtime SDK (aeiruntime) for
// the agent binary. It imports no provider and no client-go.
replace github.com/jackfrancis/agent-execution-interface/aeiruntime => ../agent-execution-interface/aeiruntime

replace github.com/jackfrancis/agent-execution-interface/sdks/go => ../agent-execution-interface/sdks/go
