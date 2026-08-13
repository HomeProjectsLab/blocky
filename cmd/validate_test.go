package cmd

import (
	"github.com/0xERR0R/blocky/helpertest"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Validate command", func() {
	var tmpDir *helpertest.TmpFolder
	BeforeEach(func() {
		tmpDir = helpertest.NewTmpFolder("db")
	})

	When("Validate is called on a fresh database directory", func() {
		It("should seed the starter config and terminate without error", func() {
			c := NewRootCommand()
			c.SetArgs([]string{"validate", "--db-dir", tmpDir.Path})

			Expect(c.Execute()).Should(Succeed())
		})
	})

	When("Validate is called after an import", func() {
		It("should validate the imported configuration", func() {
			cfgFile := tmpDir.CreateStringFile("config.yaml",
				"upstreams:",
				"  groups:",
				"    default:",
				"      - 1.1.1.1")

			c := NewRootCommand()
			c.SetArgs([]string{"import", cfgFile.Path, "--db-dir", tmpDir.Path})
			Expect(c.Execute()).Should(Succeed())

			c = NewRootCommand()
			c.SetArgs([]string{"validate", "--db-dir", tmpDir.Path})
			Expect(c.Execute()).Should(Succeed())
		})
	})
})
