package bootstrapper

import (
	"github.com/Azure/AKSFlexNode/components/api"
)

// Shared helpers used by task wrappers in wrappers.go.

func ptrWithDefault[T comparable](value T, defaultValue T) *T {
	var zero T
	if value == zero {
		return &defaultValue
	}
	return &value
}

func ptr[T any](value T) *T {
	return &value
}

func componentAction(name string) *api.Metadata {
	return api.Metadata_builder{Name: &name}.Build()
}
