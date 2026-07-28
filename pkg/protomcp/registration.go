package protomcp

import (
	"fmt"
	"slices"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Registration bookkeeping.
//
// The SDK's registries are maps keyed by tool name, resource URI, and URI
// template: registering a key that already exists REPLACES the previous
// entry — handler included — and returns nothing. Nothing observes the
// collision, and the wire-visible catalog is unchanged, so a server can
// serve one registrar's definition with another registrar's handler and
// look completely healthy.
//
// That is not hypothetical for generated code. protoc-gen-mcp derives a
// tool's name, description, and schemas from the proto, so two calls to
// the same generated registrar produce byte-identical definitions; the
// only difference is the client captured in the handler closure. Calling
// one registrar twice — a plausible wiring mistake when a service is
// exposed on more than one surface — silently rebinds every one of its
// tools to the second client.
//
// The functions here are the write side of the registry: they claim the
// key first and panic if it is already taken. They carry the Must prefix
// (as MustParseSchema does) because the panic is the contract, not a
// surprise: generated registrars have no error return to use, and with
// proto-derived keys a duplicate is a wiring bug that fails
// deterministically at startup — the same binary always fails, so it
// cannot reach production or depend on a request. Registration happens
// before a server serves traffic.
//
// The panic value is *DuplicateRegistrationError, so a caller that wraps
// registration in a recover (to turn a duplicate into a startup error of
// its own) can errors.As it rather than match on message text.
//
// One consequence to respect: this contract assumes keys are static. A key
// derived from runtime data — one resource per tenant, a tool name built
// from an ID — makes a duplicate data-dependent, so it can pass tests and
// then crash the process on a particular row. Derive keys from code, not
// data; use a URI template for the parameterized case.
//
// Callers that need to know what a batch of registrations added — for
// example to bind primitives to an authorization scope — snapshot
// RegisteredToolNames and friends around it. Because keys can only be
// added, never replaced, the difference of two snapshots is exactly what
// the batch registered.
//
// SDK() remains an escape hatch: primitives added straight to the
// underlying mcp.Server bypass these registries (as they already bypass
// every middleware), stay absent from the snapshots, and get no
// collision protection.

// DuplicateRegistrationError reports a key already claimed on the server.
// Kind names the primitive ("tool", "resource", "resource template") and
// Key is the tool name, resource URI, or URI template. It is the value the
// MustAdd functions panic with.
type DuplicateRegistrationError struct {
	Kind string
	Key  string
}

func (e *DuplicateRegistrationError) Error() string {
	return fmt.Sprintf(
		"protomcp: %s %q is already registered; the SDK would silently replace it, "+
			"leaving the earlier registration's definition bound to the new handler — "+
			"register each %s exactly once",
		e.Kind, e.Key, e.Kind)
}

// MustAddTool registers a tool and its handler, claiming the tool's name
// and panicking if it is already registered. Apart from the name claim it
// is a straight pass-through to mcp.AddTool, including that function's
// schema inference and input validation. Generated registrars use this.
func MustAddTool[In, Out any](s *Server, tool *mcp.Tool, handler mcp.ToolHandlerFor[In, Out]) {
	if tool == nil {
		panic("protomcp: MustAddTool received a nil tool")
	}
	s.mustClaim(&s.toolNames, "tool", tool.Name)
	mcp.AddTool(s.sdk, tool, handler)
}

// MustAddTool registers a tool with a plain (non-generic) handler,
// claiming the tool's name and panicking if it is already registered. It
// is the counterpart of mcp.Server.AddTool, for hand-written tools that
// do their own argument decoding; generated code uses the package-level
// MustAddTool, which cannot be a method because Go methods take no type
// parameters.
func (s *Server) MustAddTool(tool *mcp.Tool, handler mcp.ToolHandler) {
	if tool == nil {
		panic("protomcp: MustAddTool received a nil tool")
	}
	s.mustClaim(&s.toolNames, "tool", tool.Name)
	s.sdk.AddTool(tool, handler)
}

// MustAddResource registers a static resource and its handler, claiming
// the resource's URI and panicking if it is already registered.
func (s *Server) MustAddResource(resource *mcp.Resource, handler mcp.ResourceHandler) {
	if resource == nil {
		panic("protomcp: MustAddResource received a nil resource")
	}
	s.mustClaim(&s.resourceURIs, "resource", resource.URI)
	s.sdk.AddResource(resource, handler)
}

// MustAddResourceTemplate registers a resource template and its handler,
// claiming the URI template and panicking if it is already registered.
// Generated resource registrars use this.
func (s *Server) MustAddResourceTemplate(template *mcp.ResourceTemplate, handler mcp.ResourceHandler) {
	if template == nil {
		panic("protomcp: MustAddResourceTemplate received a nil resource template")
	}
	s.mustClaim(&s.resourceTemplates, "resource template", template.URITemplate)
	s.sdk.AddResourceTemplate(template, handler)
}

// RegisteredToolNames returns the tool names registered through
// MustAddTool, sorted. Tools attached directly via SDK() are not included.
func (s *Server) RegisteredToolNames() []string {
	return s.registered(&s.toolNames)
}

// RegisteredResourceURIs returns the resource URIs registered through
// MustAddResource, sorted. Resources attached directly via SDK() are not
// included.
func (s *Server) RegisteredResourceURIs() []string {
	return s.registered(&s.resourceURIs)
}

// RegisteredResourceTemplates returns the URI templates registered
// through MustAddResourceTemplate, sorted. Templates attached directly via
// SDK() are not included.
func (s *Server) RegisteredResourceTemplates() []string {
	return s.registered(&s.resourceTemplates)
}

// mustClaim records key in the registry, panicking with a
// *DuplicateRegistrationError if it is already present. kind names the
// primitive in the message.
func (s *Server) mustClaim(registry *map[string]bool, kind, key string) {
	if key == "" {
		panic(fmt.Sprintf("protomcp: %s registered with an empty key", kind))
	}
	s.registrationMu.Lock()
	defer s.registrationMu.Unlock()
	if *registry == nil {
		*registry = map[string]bool{}
	}
	if (*registry)[key] {
		panic(&DuplicateRegistrationError{Kind: kind, Key: key})
	}
	(*registry)[key] = true
}

func (s *Server) registered(registry *map[string]bool) []string {
	s.registrationMu.Lock()
	defer s.registrationMu.Unlock()
	keys := make([]string, 0, len(*registry))
	for key := range *registry {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}
