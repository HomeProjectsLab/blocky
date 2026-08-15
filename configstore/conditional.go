package configstore

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v2"
)

// GetConditional returns the conditional.mapping out of the RAW stored blob.
// Each stored value is a COMMA-JOINED STRING (config.ConditionalUpstreamMapping
// has a custom UnmarshalYAML that splits on ","), so we split it back into the
// upstream list here. Absent key -> empty map.
func (s *Store) GetConditional() (map[string][]string, error) {
	raw, err := s.RawYAML()
	if err != nil {
		return nil, err
	}

	root := map[string]any{}
	if err := yaml.Unmarshal([]byte(raw), &root); err != nil {
		return nil, fmt.Errorf("can't parse stored config: %w", err)
	}

	out := map[string][]string{}

	mapping := nestedMap(nestedMap(root["conditional"])["mapping"])
	for domain, v := range mapping {
		joined, _ := v.(string)
		for _, part := range strings.Split(joined, ",") {
			if p := strings.TrimSpace(part); p != "" {
				out[domain] = append(out[domain], p)
			}
		}
	}

	return out, nil
}

// SetConditionalMapping replaces conditional.mapping.<domain> with the given
// upstreams (comma-joined, matching the custom UnmarshalYAML), leaving other
// mappings and the rewrite config untouched. Validated through the full
// pipeline (SetRawYAML) before persist; nothing is written on failure.
func (s *Store) SetConditionalMapping(domain string, upstreams []string) error {
	return s.mutateConditional(func(mapping map[any]any) {
		mapping[domain] = strings.Join(upstreams, ",")
	})
}

// DeleteConditionalMapping removes conditional.mapping.<domain>.
func (s *Store) DeleteConditionalMapping(domain string) error {
	return s.mutateConditional(func(mapping map[any]any) {
		delete(mapping, domain)
	})
}

// mutateConditional serializes a read-modify-write of conditional.mapping: each
// section writer rewrites the WHOLE document from its own snapshot, so a
// concurrent writer would otherwise clobber this change (last full write wins).
func (s *Store) mutateConditional(mutate func(mapping map[any]any)) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	raw, err := s.RawYAML()
	if err != nil {
		return err
	}

	root := map[string]any{}
	if err := yaml.Unmarshal([]byte(raw), &root); err != nil {
		return fmt.Errorf("can't parse stored config: %w", err)
	}

	// yaml.v2 decodes nested maps as map[any]any, not map[string]any.
	cond, _ := root["conditional"].(map[any]any)
	if cond == nil {
		cond = map[any]any{}
		root["conditional"] = cond
	}

	mapping, _ := cond["mapping"].(map[any]any)
	if mapping == nil {
		mapping = map[any]any{}
		cond["mapping"] = mapping
	}

	mutate(mapping)

	merged, err := yaml.Marshal(root)
	if err != nil {
		return fmt.Errorf("can't marshal merged config: %w", err)
	}

	return s.setRawYAML(string(merged))
}
