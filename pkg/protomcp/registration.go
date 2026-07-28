package protomcp

import (
	"errors"
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
// key first and refuse it if already taken, naming both the primitive and
// the key. Each comes in two forms, following this package's
// MustParseSchema convention:
//
//   - AddTool, AddResource, AddResourceTemplate return an error. Prefer
//     these; a caller that can report or degrade should not be forced to
//     recover a panic.
//   - MustAddTool, MustAddResource, MustAddResourceTemplate panic with
//     that same error. Generated registrars use these because their
//     signatures have no error return, and registration runs at process
//     start where a duplicate key is a wiring bug rather than a runtime
//     condition.
//
// A duplicate is reported as *DuplicateRegistrationError, so callers that
// do recover from a Must call can errors.As it instead of matching on
// message text.
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
// Key is the tool name, resource URI, or URI template.
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

// AddTool registers a tool and its handler, claiming the tool's name.
// Apart from the name claim it is a straight pass-through to mcp.AddTool,
// including that function's schema inference and input validation.
// Returns *DuplicateRegistrationError if the name is already registered.
func AddTool[In, Out any](s *Server, tool *mcp.Tool, handler mcp.ToolHandlerFor[In, Out]) error {
	if tool == nil {
		return errors.New("protomcp: AddTool received a nil tool")
	}
	if err := s.claim(&s.toolNames, "tool", tool.Name); err != nil {
		return err
	}
	mcp.AddTool(s.sdk, tool, handler)
	return nil
}

// MustAddTool is AddTool, panicking with its error. Generated registrars
// use this form because they have no error return.
func MustAddTool[In, Out any](s *Server, tool *mcp.Tool, handler mcp.ToolHandlerFor[In, Out]) {
	if err := AddTool(s, tool, handler); err != nil {
		panic(err)
	}
}

// AddTool registers a tool with a plain (non-generic) handler, claiming
// the tool's name. It is the counterpart of mcp.Server.AddTool, for
// hand-written tools that do their own argument decoding; generated code
// uses the package-level AddTool, which cannot be a method because Go
// methods take no type parameters.
func (s *Server) AddTool(tool *mcp.Tool, handler mcp.ToolHandler) error {
	if tool == nil {
		return errors.New("protomcp: AddTool received a nil tool")
	}
	if err := s.claim(&s.toolNames, "tool", tool.Name); err != nil {
		return err
	}
	s.sdk.AddTool(tool, handler)
	return nil
}

// MustAddTool is Server.AddTool, panicking with its error.
func (s *Server) MustAddTool(tool *mcp.Tool, handler mcp.ToolHandler) {
	if err := s.AddTool(tool, handler); err != nil {
		panic(err)
	}
}

// AddResource registers a static resource and its handler, claiming the
// resource's URI.
func (s *Server) AddResource(resource *mcp.Resource, handler mcp.ResourceHandler) error {
	if resource == nil {
		return errors.New("protomcp: AddResource received a nil resource")
	}
	if err := s.claim(&s.resourceURIs, "resource", resource.URI); err != nil {
		return err
	}
	s.sdk.AddResource(resource, handler)
	return nil
}

// MustAddResource is AddResource, panicking with its error.
func (s *Server) MustAddResource(resource *mcp.Resource, handler mcp.ResourceHandler) {
	if err := s.AddResource(resource, handler); err != nil {
		panic(err)
	}
}

// AddResourceTemplate registers a resource template and its handler,
// claiming the URI template.
func (s *Server) AddResourceTemplate(template *mcp.ResourceTemplate, handler mcp.ResourceHandler) error {
	if template == nil {
		return errors.New("protomcp: AddResourceTemplate received a nil resource template")
	}
	if err := s.claim(&s.resourceTemplates, "resource template", template.URITemplate); err != nil {
		return err
	}
	s.sdk.AddResourceTemplate(template, handler)
	return nil
}

// MustAddResourceTemplate is AddResourceTemplate, panicking with its
// error. Generated resource registrars use this form.
func (s *Server) MustAddResourceTemplate(template *mcp.ResourceTemplate, handler mcp.ResourceHandler) {
	if err := s.AddResourceTemplate(template, handler); err != nil {
		panic(err)
	}
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

// claim records key in the registry, refusing a key already present.
// kind names the primitive in the error.
func (s *Server) claim(registry *map[string]bool, kind, key string) error {
	if key == "" {
		return fmt.Errorf("protomcp: %s registered with an empty key", kind)
	}
	s.registrationMu.Lock()
	defer s.registrationMu.Unlock()
	if *registry == nil {
		*registry = map[string]bool{}
	}
	if (*registry)[key] {
		return &DuplicateRegistrationError{Kind: kind, Key: key}
	}
	(*registry)[key] = true
	return nil
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
