package configstore

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v2"
	yaml3 "gopkg.in/yaml.v3"
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
	return s.mutateConditional(func(mapping *yaml3.Node) {
		setMapEntry(mapping, domain,
			&yaml3.Node{Kind: yaml3.ScalarNode, Tag: "!!str", Value: strings.Join(upstreams, ",")})
	})
}

// DeleteConditionalMapping removes conditional.mapping.<domain>.
func (s *Store) DeleteConditionalMapping(domain string) error {
	return s.mutateConditional(func(mapping *yaml3.Node) {
		deleteMapEntry(mapping, domain)
	})
}

// mutateConditional serializes a read-modify-write of conditional.mapping: each
// section writer rewrites the WHOLE document from its own snapshot, so a
// concurrent writer would otherwise clobber this change (last full write wins).
// yaml.v3 node surgery (see customdns.go) touches only the conditional key,
// preserving the hand-editable blob's comments, anchors and key order.
func (s *Store) mutateConditional(mutate func(mapping *yaml3.Node)) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	raw, err := s.RawYAML()
	if err != nil {
		return err
	}

	doc, root, err := parseDocNode(raw)
	if err != nil {
		return err
	}

	cond := mapEntry(root, "conditional")
	if cond == nil || cond.Kind != yaml3.MappingNode {
		cond = &yaml3.Node{Kind: yaml3.MappingNode, Tag: "!!map"}
		setMapEntry(root, "conditional", cond)
	}

	mapping := mapEntry(cond, "mapping")
	if mapping == nil || mapping.Kind != yaml3.MappingNode {
		mapping = &yaml3.Node{Kind: yaml3.MappingNode, Tag: "!!map"}
		setMapEntry(cond, "mapping", mapping)
	}

	mutate(mapping)

	merged, err := marshalDocNode(doc)
	if err != nil {
		return err
	}

	return s.setRawYAML(merged)
}
