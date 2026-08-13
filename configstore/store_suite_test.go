package configstore

import (
	"testing"

	"github.com/0xERR0R/blocky/log"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func init() {
	log.Silence()
}

func TestConfigStore(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "ConfigStore Suite")
}
