package configstore

import (
	"fmt"

	"github.com/0xERR0R/blocky/config"
	yaml3 "gopkg.in/yaml.v3"
)

// GetPrivacy returns the privacy section of the stored config, with defaults
// applied (via the full load pipeline).
func (s *Store) GetPrivacy() (config.PrivacyConfig, error) {
	cfg, err := s.LoadConfig()
	if err != nil {
		return config.PrivacyConfig{}, err
	}

	return cfg.Privacy, nil
}

// SetPrivacy replaces just the `privacy:` section of the stored config blob and
// persists it. The whole candidate config is validated through the full
// pipeline (SetRawYAML -> ValidateRaw -> LoadFromYAML) before persist; nothing
// is written if validation fails.
func (s *Store) SetPrivacy(p config.PrivacyConfig) error {
	// Serialize the read-modify-write: each section writer rewrites the WHOLE
	// document from its own snapshot, so a concurrent SetLocalDNSZone would
	// otherwise clobber this change (last full-document write wins).
	s.mu.Lock()
	defer s.mu.Unlock()

	raw, err := s.RawYAML()
	if err != nil {
		return err
	}

	// node surgery (see customdns.go helpers): replace only `privacy:`, keep the
	// hand-editable blob's comments/anchors/order intact.
	doc, root, err := parseDocNode(raw)
	if err != nil {
		return err
	}

	node := &yaml3.Node{}
	if err := node.Encode(p); err != nil {
		return fmt.Errorf("can't marshal merged config: %w", err)
	}

	setMapEntry(root, "privacy", node)

	merged, err := marshalDocNode(doc)
	if err != nil {
		return err
	}

	return s.setRawYAML(merged)
}
