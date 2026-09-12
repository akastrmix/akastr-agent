package capability

import (
	"errors"
	"fmt"
	"sort"
)

type Descriptor struct {
	Name            string         `json:"name"`
	Version         int            `json:"version"`
	ExclusiveGroups []string       `json:"exclusive_groups,omitempty"`
	Properties      map[string]any `json:"properties,omitempty"`
}

type Registry struct {
	descriptors []Descriptor
}

func New(descriptors ...Descriptor) (*Registry, error) {
	registry := &Registry{descriptors: make([]Descriptor, 0, len(descriptors))}
	for _, descriptor := range descriptors {
		if descriptor.Name == "" {
			return nil, errors.New("capability name is required")
		}
		if descriptor.Version < 1 {
			return nil, fmt.Errorf("capability %s version must be positive", descriptor.Name)
		}
		registry.descriptors = append(registry.descriptors, cloneDescriptor(descriptor))
	}
	sort.Slice(registry.descriptors, func(i, j int) bool { return registry.descriptors[i].Name < registry.descriptors[j].Name })
	for index := 1; index < len(registry.descriptors); index++ {
		if registry.descriptors[index].Name == registry.descriptors[index-1].Name {
			return nil, fmt.Errorf("duplicate capability %s", registry.descriptors[index].Name)
		}
	}
	return registry, nil
}

func (r *Registry) List() []Descriptor {
	result := make([]Descriptor, 0, len(r.descriptors))
	for _, descriptor := range r.descriptors {
		result = append(result, cloneDescriptor(descriptor))
	}
	return result
}

func cloneDescriptor(source Descriptor) Descriptor {
	copy := source
	copy.ExclusiveGroups = append([]string(nil), source.ExclusiveGroups...)
	if source.Properties != nil {
		copy.Properties = make(map[string]any, len(source.Properties))
		for key, value := range source.Properties {
			switch typed := value.(type) {
			case []string:
				copy.Properties[key] = append([]string(nil), typed...)
			default:
				copy.Properties[key] = value
			}
		}
	}
	return copy
}
