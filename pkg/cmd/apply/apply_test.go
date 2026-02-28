package apply

import (
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/Azure/AKSFlexNode/components/api"
	_ "github.com/Azure/AKSFlexNode/components/linux" // register linux action types
)

func TestIsBinaryProtoContent(t *testing.T) {
	tests := []struct {
		name  string
		input []byte
		want  bool
	}{
		{name: "json object", input: []byte(`{"metadata":{}}`), want: false},
		{name: "json array", input: []byte(`[{"metadata":{}}]`), want: false},
		{name: "json object with leading whitespace", input: []byte("  \t\n{\"metadata\":{}}"), want: false},
		{name: "json array with leading whitespace", input: []byte("\n  [{\"metadata\":{}}]"), want: false},
		{name: "binary proto (non-json first byte)", input: []byte{0x0a, 0x00}, want: true},
		{name: "empty input", input: []byte{}, want: false},
		{name: "whitespace only", input: []byte("   "), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isBinaryProtoContent(tt.input)
			if got != tt.want {
				t.Errorf("isBinaryProtoContent(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestParseActionFromProto(t *testing.T) {
	// Build a binary-encoded Base message that carries the ConfigureBaseOS type URL.
	base := &api.Base{}
	base.SetMetadata(&api.Metadata{})
	base.GetMetadata().SetType("aks.flex.components.linux.ConfigureBaseOS")
	base.GetMetadata().SetName("test-action")

	b, err := proto.Marshal(base)
	if err != nil {
		t.Fatalf("proto.Marshal: %v", err)
	}

	pa, err := parseActionFromProto(b)
	if err != nil {
		t.Fatalf("parseActionFromProto: %v", err)
	}

	if pa.name != "test-action" {
		t.Errorf("name = %q, want %q", pa.name, "test-action")
	}
	if pa.message == nil {
		t.Error("message is nil")
	}
}

func TestParseActionFromProto_FallbackName(t *testing.T) {
	// When no name is set the type URL is used as the display name.
	base := &api.Base{}
	base.SetMetadata(&api.Metadata{})
	base.GetMetadata().SetType("aks.flex.components.linux.ConfigureBaseOS")

	b, err := proto.Marshal(base)
	if err != nil {
		t.Fatalf("proto.Marshal: %v", err)
	}

	pa, err := parseActionFromProto(b)
	if err != nil {
		t.Fatalf("parseActionFromProto: %v", err)
	}

	want := "aks.flex.components.linux.ConfigureBaseOS"
	if pa.name != want {
		t.Errorf("name = %q, want %q", pa.name, want)
	}
}

func TestParseActionFromProto_UnknownType(t *testing.T) {
	base := &api.Base{}
	base.SetMetadata(&api.Metadata{})
	base.GetMetadata().SetType("does.not.Exist")

	b, err := proto.Marshal(base)
	if err != nil {
		t.Fatalf("proto.Marshal: %v", err)
	}

	if _, err := parseActionFromProto(b); err == nil {
		t.Error("expected error for unknown type, got nil")
	}
}

func TestIsBinaryProtoContent_RoundTrip(t *testing.T) {
	// A real serialized proto message must be detected as binary.
	base := &api.Base{}
	base.SetMetadata(&api.Metadata{})
	base.GetMetadata().SetType("aks.flex.components.linux.ConfigureBaseOS")

	b, err := proto.Marshal(base)
	if err != nil {
		t.Fatalf("proto.Marshal: %v", err)
	}

	if !isBinaryProtoContent(b) {
		t.Error("expected isBinaryProtoContent to return true for a marshaled proto message")
	}
}
