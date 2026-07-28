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
// key first and panic if it is already taken, so a collision fails at
// startup with both call sites' key named instead of resolving silently.
// Panicking (rather than returning an error) is deliberate — generated
// registrars return nothing, registration happens during process start,
// and it matches RegisterResourceLister's second-call behavior.
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

// AddTool registers a tool and its handler, claiming the tool's name.
// It panics if the name is already registered. Apart from the name
// claim it is a straight pass-through to mcp.AddTool, including that
// function's schema inference and input validation.
func AddTool[In, Out any](s *Server, tool *mcp.Tool, handler mcp.ToolHandlerFor[In, Out]) {
	if tool == nil {
		panic("protomcp: AddTool received a nil tool")
	}
	s.claim(&s.toolNames, "tool", tool.Name)
	mcp.AddTool(s.sdk, tool, handler)
}

// AddResource registers a static resource and its handler, claiming the
// resource's URI. It panics if the URI is already registered.
func (s *Server) AddResource(resource *mcp.Resource, handler mcp.ResourceHandler) {
	if resource == nil {
		panic("protomcp: AddResource received a nil resource")
	}
	s.claim(&s.resourceURIs, "resource", resource.URI)
	s.sdk.AddResource(resource, handler)
}

// AddResourceTemplate registers a resource template and its handler,
// claiming the URI template. It panics if the template is already
// registered.
func (s *Server) AddResourceTemplate(template *mcp.ResourceTemplate, handler mcp.ResourceHandler) {
	if template == nil {
		panic("protomcp: AddResourceTemplate received a nil resource template")
	}
	s.claim(&s.resourceTemplates, "resource template", template.URITemplate)
	s.sdk.AddResourceTemplate(template, handler)
}

// RegisteredToolNames returns the tool names registered through AddTool,
// sorted. Tools attached directly via SDK() are not included.
func (s *Server) RegisteredToolNames() []string {
	return s.registered(&s.toolNames)
}

// RegisteredResourceURIs returns the resource URIs registered through
// AddResource, sorted. Resources attached directly via SDK() are not
// included.
func (s *Server) RegisteredResourceURIs() []string {
	return s.registered(&s.resourceURIs)
}

// RegisteredResourceTemplates returns the URI templates registered
// through AddResourceTemplate, sorted. Templates attached directly via
// SDK() are not included.
func (s *Server) RegisteredResourceTemplates() []string {
	return s.registered(&s.resourceTemplates)
}

// claim records key in the registry, panicking if it is already present.
// kind names the primitive in the panic message.
func (s *Server) claim(registry *map[string]bool, kind, key string) {
	if key == "" {
		panic(fmt.Sprintf("protomcp: %s registered with an empty key", kind))
	}
	s.registrationMu.Lock()
	defer s.registrationMu.Unlock()
	if *registry == nil {
		*registry = map[string]bool{}
	}
	if (*registry)[key] {
		panic(fmt.Sprintf(
			"protomcp: %s %q is already registered; the SDK would silently replace it, "+
				"leaving the earlier registration's definition bound to this handler — "+
				"register each %s exactly once",
			kind, key, kind))
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
