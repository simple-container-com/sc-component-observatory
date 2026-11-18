// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package observatory

import "github.com/simple-container-com/api/pkg/api"

// inputWithRaw builds a ResourceInput whose component config is the
// given map, via the ComponentRaw side-channel that SC core uses on the
// compute-process path.
func inputWithRaw(raw map[string]any) api.ResourceInput {
	return api.ResourceInput{
		Descriptor:   &api.ResourceDescriptor{Type: "sc-observatory", Name: "my-observatory"},
		ComponentRaw: raw,
	}
}
