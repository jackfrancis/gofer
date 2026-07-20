module github.com/jackfrancis/gofer

go 1.26.0

require (
	github.com/jackfrancis/agent-execution-interface v0.0.0-00010101000000-000000000000
	github.com/jackfrancis/agent-execution-interface/aeiruntime v0.0.0-00010101000000-000000000000
	github.com/jackfrancis/agent-execution-interface/sdks/go v0.0.0-00010101000000-000000000000
	github.com/microcosm-cc/bluemonday v1.0.27
	github.com/yuin/goldmark v1.8.4
	golang.org/x/oauth2 v0.36.0
)

require (
	cloud.google.com/go/compute/metadata v0.3.0 // indirect
	github.com/aymerick/douceur v0.2.0 // indirect
	github.com/gorilla/css v1.0.1 // indirect
	golang.org/x/net v0.49.0 // indirect
)

// AEI is consumed as a local sibling checkout until it is published. gofer imports
// only the stdlib-only pieces of AEI: the core (aei) for the run-spec types, the
// runtime SDK (aeiruntime) for the agent binary, and the app SDK (sdks/go/aeiapp)
// to dispatch runs to the pre-installed control plane over HTTP/JSON. It imports
// no provider and no client-go (docs/adr/0001): dispatch is an app-to-controller
// call, not an embedded launcher.
replace github.com/jackfrancis/agent-execution-interface => ../agent-execution-interface

replace github.com/jackfrancis/agent-execution-interface/aeiruntime => ../agent-execution-interface/aeiruntime

replace github.com/jackfrancis/agent-execution-interface/sdks/go => ../agent-execution-interface/sdks/go
