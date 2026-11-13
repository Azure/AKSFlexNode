// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package hostnode

import (
	"context"

	"github.com/Azure/unbounded/pkg/agent/phases"
)

type funcTask struct {
	name string
	do   func(context.Context) error
}

func (t funcTask) Name() string                 { return t.name }
func (t funcTask) Do(ctx context.Context) error { return t.do(ctx) }

// Func adapts a function to a phases.Task named name. The name is what a failed
// task carries into the error and the Machine condition reason is classified
// from.
func Func(name string, do func(context.Context) error) phases.Task {
	return funcTask{name: name, do: do}
}
