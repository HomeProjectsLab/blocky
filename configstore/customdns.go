package configstore

import (
	"errors"
	"fmt"
	"strings"

	"gopkg.in/yaml.v2"
	yaml3 "gopkg.in/yaml.v3"
)

// GetLocalDNSZone returns the customDNS.zone text out of the RAW stored blob.
// It reads the raw YAML (not the parsed struct: ZoneFileDNS has no string form)
// and returns "" if the key is absent.
func (s *Store) GetLocalDNSZone() (string, error) {
	raw, err := s.RawYAML()
	if err != nil {
		return "", err
	}

	root := map[string]any{}
	if err := yaml.Unmarshal([]byte(raw), &root); err != nil {
		return "", fmt.Errorf("can't parse stored config: %w", err)
	}

	sub := nestedMap(root["customDNS"])
	if sub == nil {
		return "", nil
	}

	zone, _ := sub["zone"].(string)

	return zone, nil
}

// SetLocalDNSZone replaces only customDNS.zone in the stored blob, leaving
// customTTL/filterUnmappedTypes/mapping untouched. The candidate is validated
// through the full pipeline (SetRawYAML) before persist; nothing is written on
// failure.
func (s *Store) SetLocalDNSZone(zoneText string) error {
	// Serialize the read-modify-write: each section writer rewrites the WHOLE
	// document from its own snapshot, so a concurrent SetPrivacy would otherwise
	// clobber this change (last full-document write wins).
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

	sub := mapEntry(root, "customDNS")
	if sub == nil || sub.Kind != yaml3.MappingNode {
		sub = &yaml3.Node{Kind: yaml3.MappingNode, Tag: "!!map"}
		setMapEntry(root, "customDNS", sub)
	}

	zone := &yaml3.Node{}
	if zoneText != "" && strings.TrimSpace(zoneText) == "" {
		// A whitespace-only zone can't survive YAML block-scalar chomping
		// (all-newline content collapses on read); store it as an escaped
		// double-quoted scalar so it round-trips verbatim.
		zone.Kind, zone.Tag, zone.Value, zone.Style = yaml3.ScalarNode, "!!str", zoneText, yaml3.DoubleQuotedStyle
	} else if err := zone.Encode(zoneText); err != nil {
		return fmt.Errorf("can't marshal merged config: %w", err)
	}

	setMapEntry(sub, "zone", zone)

	merged, err := marshalDocNode(doc)
	if err != nil {
		return err
	}

	return s.setRawYAML(merged)
}

// GetLocalDNSNXDomains returns the customDNS.nxdomain list (domains answered with
// NXDOMAIN) from the raw stored blob, or nil if absent.
func (s *Store) GetLocalDNSNXDomains() ([]string, error) {
	raw, err := s.RawYAML()
	if err != nil {
		return nil, err
	}

	root := map[string]any{}
	if err := yaml.Unmarshal([]byte(raw), &root); err != nil {
		return nil, fmt.Errorf("can't parse stored config: %w", err)
	}

	sub := nestedMap(root["customDNS"])
	if sub == nil {
		return nil, nil
	}

	raw2, _ := sub["nxdomain"].([]any)

	out := make([]string, 0, len(raw2))
	for _, v := range raw2 {
		if s, ok := v.(string); ok && s != "" {
			out = append(out, s)
		}
	}

	return out, nil
}

// SetLocalDNSNXDomains replaces only customDNS.nxdomain in the stored blob,
// leaving zone/mapping/etc. untouched. Validated through setRawYAML before persist.
func (s *Store) SetLocalDNSNXDomains(domains []string) error {
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

	sub := mapEntry(root, "customDNS")
	if sub == nil || sub.Kind != yaml3.MappingNode {
		sub = &yaml3.Node{Kind: yaml3.MappingNode, Tag: "!!map"}
		setMapEntry(root, "customDNS", sub)
	}

	if len(domains) == 0 {
		deleteMapEntry(sub, "nxdomain")
	} else {
		list := &yaml3.Node{}
		if err := list.Encode(domains); err != nil {
			return fmt.Errorf("can't marshal merged config: %w", err)
		}

		setMapEntry(sub, "nxdomain", list)
	}

	merged, err := marshalDocNode(doc)
	if err != nil {
		return err
	}

	return s.setRawYAML(merged)
}

// --- yaml.v3 node surgery ----------------------------------------------------
// The Settings blob is hand-editable: section writers must touch ONLY their
// key, not flatten comments/anchors/order the way a map round-trip does.

// parseDocNode parses raw into a yaml.v3 document whose root is a mapping,
// synthesizing an empty mapping for an empty blob.
func parseDocNode(raw string) (doc, root *yaml3.Node, err error) {
	doc = &yaml3.Node{}
	if err := yaml3.Unmarshal([]byte(raw), doc); err != nil {
		return nil, nil, fmt.Errorf("can't parse stored config: %w", err)
	}

	if len(doc.Content) == 0 {
		doc.Kind = yaml3.DocumentNode
		doc.Content = []*yaml3.Node{{Kind: yaml3.MappingNode, Tag: "!!map"}}
	}

	root = doc.Content[0]
	if root.Kind != yaml3.MappingNode {
		return nil, nil, errors.New("can't parse stored config: not a YAML mapping")
	}

	return doc, root, nil
}

// mapEntry returns the value node for key in mapping m, or nil.
func mapEntry(m *yaml3.Node, key string) *yaml3.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}

	return nil
}

// setMapEntry replaces key's value in mapping m, appending the pair if absent.
func setMapEntry(m *yaml3.Node, key string, val *yaml3.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1] = val

			return
		}
	}

	m.Content = append(m.Content, &yaml3.Node{Kind: yaml3.ScalarNode, Tag: "!!str", Value: key}, val)
}

// deleteMapEntry removes key (and its value) from mapping m if present.
func deleteMapEntry(m *yaml3.Node, key string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)

			return
		}
	}
}

// marshalDocNode renders doc with the blob's conventional 2-space indent.
func marshalDocNode(doc *yaml3.Node) (string, error) {
	var buf strings.Builder

	enc := yaml3.NewEncoder(&buf)
	enc.SetIndent(2)

	if err := enc.Encode(doc); err != nil {
		return "", fmt.Errorf("can't marshal merged config: %w", err)
	}

	if err := enc.Close(); err != nil {
		return "", fmt.Errorf("can't marshal merged config: %w", err)
	}

	return buf.String(), nil
}

// nestedMap normalizes a yaml.v2 submap (map[any]any) or a map[string]any into
// map[string]any for reads.
func nestedMap(v any) map[string]any {
	switch m := v.(type) {
	case map[string]any:
		return m
	case map[any]any:
		out := make(map[string]any, len(m))
		for k, val := range m {
			if ks, ok := k.(string); ok {
				out[ks] = val
			}
		}

		return out
	}

	return nil
}
