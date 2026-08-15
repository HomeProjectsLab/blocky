package configstore

import (
	"fmt"

	"gopkg.in/yaml.v2"
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

	root := map[string]any{}
	if err := yaml.Unmarshal([]byte(raw), &root); err != nil {
		return fmt.Errorf("can't parse stored config: %w", err)
	}

	// yaml.v2 decodes nested maps as map[any]any, not map[string]any.
	sub, _ := root["customDNS"].(map[any]any)
	if sub == nil {
		sub = map[any]any{}
		root["customDNS"] = sub
	}

	sub["zone"] = zoneText

	merged, err := yaml.Marshal(root)
	if err != nil {
		return fmt.Errorf("can't marshal merged config: %w", err)
	}

	return s.setRawYAML(string(merged))
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

	root := map[string]any{}
	if err := yaml.Unmarshal([]byte(raw), &root); err != nil {
		return fmt.Errorf("can't parse stored config: %w", err)
	}

	sub, _ := root["customDNS"].(map[any]any)
	if sub == nil {
		sub = map[any]any{}
		root["customDNS"] = sub
	}

	if len(domains) == 0 {
		delete(sub, "nxdomain")
	} else {
		list := make([]any, len(domains))
		for i, d := range domains {
			list[i] = d
		}

		sub["nxdomain"] = list
	}

	merged, err := yaml.Marshal(root)
	if err != nil {
		return fmt.Errorf("can't marshal merged config: %w", err)
	}

	return s.setRawYAML(string(merged))
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
