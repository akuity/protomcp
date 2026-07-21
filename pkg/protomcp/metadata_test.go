package protomcp

import (
	"context"
	"reflect"
	"testing"

	"google.golang.org/grpc/metadata"
)

func TestOutgoingContext_NoExistingMetadata(t *testing.T) {
	md := metadata.Pairs("x-a", "1")
	ctx := OutgoingContext(context.Background(), md)
	got, ok := metadata.FromOutgoingContext(ctx)
	if !ok {
		t.Fatal("no outgoing metadata on context")
	}
	if want := []string{"1"}; !reflect.DeepEqual(got.Get("x-a"), want) {
		t.Errorf("x-a = %v, want %v", got.Get("x-a"), want)
	}
}

func TestOutgoingContext_MergesWithExisting(t *testing.T) {
	// Ambient metadata set by an outer layer (consumer HTTP middleware,
	// a client wrapper) must survive the generated handler applying
	// GRPCData.Metadata; metadata.NewOutgoingContext would drop it.
	ctx := metadata.NewOutgoingContext(context.Background(),
		metadata.Pairs("x-ambient", "outer"))
	ctx = OutgoingContext(ctx, metadata.Pairs("x-handler", "inner"))

	got, ok := metadata.FromOutgoingContext(ctx)
	if !ok {
		t.Fatal("no outgoing metadata on context")
	}
	if want := []string{"outer"}; !reflect.DeepEqual(got.Get("x-ambient"), want) {
		t.Errorf("x-ambient = %v, want %v (existing metadata was replaced)", got.Get("x-ambient"), want)
	}
	if want := []string{"inner"}; !reflect.DeepEqual(got.Get("x-handler"), want) {
		t.Errorf("x-handler = %v, want %v", got.Get("x-handler"), want)
	}
}

func TestOutgoingContext_DuplicateKeysKeepBothValues(t *testing.T) {
	// metadata.Join semantics: duplicates accumulate, existing values
	// first. This ordering is part of the helper's contract.
	ctx := metadata.NewOutgoingContext(context.Background(),
		metadata.Pairs("x-dup", "existing"))
	ctx = OutgoingContext(ctx, metadata.Pairs("x-dup", "merged"))

	got, _ := metadata.FromOutgoingContext(ctx)
	if want := []string{"existing", "merged"}; !reflect.DeepEqual(got.Get("x-dup"), want) {
		t.Errorf("x-dup = %v, want %v", got.Get("x-dup"), want)
	}
}

func TestSanitizeMetadataValue(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"empty", "", ""},
		{"clean ascii", "foo-bar_123", "foo-bar_123"},
		{"printable with tab", "a\tb", "a\tb"},
		{"strip CR", "foo\rbar", "foobar"},
		{"strip LF", "foo\nbar", "foobar"},
		{"strip CRLF", "foo\r\nx-admin: 1", "foox-admin: 1"},
		{"strip NUL", "foo\x00bar", "foobar"},
		{"strip all control except tab", "a\x01\x02\x03\tb", "a\tb"},
		{"utf-8 preserved", "héllo", "héllo"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SanitizeMetadataValue(tc.in)
			if got != tc.want {
				t.Errorf("SanitizeMetadataValue(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
