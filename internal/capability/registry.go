package capability

import (
	"errors"
	"fmt"
	"sort"
)

type Descriptor struct {
	Name            string            `json:"name"`
	Version         int               `json:"version"`
	ExclusiveGroups []string          `json:"exclusive_groups,omitempty"`
	Properties      map[string]string `json:"properties,omitempty"`
}

type Registry struct {
	byName map[string]Descriptor
}

func New(descriptors ...Descriptor) (*Registry, error) {
	registry := &Registry{byName: make(map[string]Descriptor, len(descriptors))}
	for _, descriptor := range descriptors {
		if descriptor.Name == "" {
			return nil, errors.New("capability name is required")
		}
		if descriptor.Version < 1 {
			return nil, fmt.Errorf("capability %s version must be positive", descriptor.Name)
		}
		if _, exists := registry.byName[descriptor.Name]; exists {
			return nil, fmt.Errorf("duplicate capability %s", descriptor.Name)
		}
		registry.byName[descriptor.Name] = cloneDescriptor(descriptor)
	}
	return registry, nil
}

func (r *Registry) List() []Descriptor {
	result := make([]Descriptor, 0, len(r.byName))
	for _, descriptor := range r.byName {
		result = append(result, cloneDescriptor(descriptor))
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Name < result[j].Name
	})
	return result
}

func cloneDescriptor(source Descriptor) Descriptor {
	copy := source
	copy.ExclusiveGroups = append([]string(nil), source.ExclusiveGroups...)
	if source.Properties != nil {
		copy.Properties = make(map[string]string, len(source.Properties))
		for key, value := range source.Properties {
			copy.Properties[key] = value
		}
	}
	return copy
}
