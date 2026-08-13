package cmd

import (
	"github.com/0xERR0R/blocky/configstore"
	"github.com/0xERR0R/blocky/helpertest"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Import command", func() {
	var (
		tmpDir  *helpertest.TmpFolder
		cfgFile *helpertest.TmpFile
	)

	BeforeEach(func() {
		tmpDir = helpertest.NewTmpFolder("db")
		cfgFile = tmpDir.CreateStringFile("config.yaml",
			"upstreams:",
			"  groups:",
			"    default:",
			"      - 5.6.7.8",
			"ports:",
			"  dns: 5533")
	})

	execute := func(args ...string) error {
		c := NewRootCommand()
		c.SetArgs(args)

		return c.Execute()
	}

	When("importing into a fresh database", func() {
		It("should succeed and LoadConfig should reflect the imported YAML", func() {
			Expect(execute("import", cfgFile.Path, "--db-dir", tmpDir.Path)).Should(Succeed())

			store, err := configstore.Open(tmpDir.Path)
			Expect(err).Should(Succeed())
			DeferCleanup(store.Close)

			cfg, err := store.LoadConfig()
			Expect(err).Should(Succeed())
			Expect(cfg.Upstreams.Groups["default"]).Should(HaveLen(1))
			Expect(cfg.Upstreams.Groups["default"][0].Host).Should(Equal("5.6.7.8"))
			Expect(cfg.Ports.DNS).Should(ContainElement(":5533"))
		})
	})

	When("importing over an already modified database", func() {
		BeforeEach(func() {
			store, err := configstore.Open(tmpDir.Path)
			Expect(err).Should(Succeed())
			Expect(store.SetRawYAML("upstreams:\n  groups:\n    default:\n      - 9.9.9.10\n")).Should(Succeed())
			Expect(store.Close()).Should(Succeed())
		})

		It("should fail without --force", func() {
			err := execute("import", cfgFile.Path, "--db-dir", tmpDir.Path)
			Expect(err).Should(MatchError(ContainSubstring("--force")))
		})

		It("should succeed with --force", func() {
			Expect(execute("import", cfgFile.Path, "--db-dir", tmpDir.Path, "--force")).Should(Succeed())

			store, err := configstore.Open(tmpDir.Path)
			Expect(err).Should(Succeed())
			DeferCleanup(store.Close)

			cfg, err := store.LoadConfig()
			Expect(err).Should(Succeed())
			Expect(cfg.Upstreams.Groups["default"][0].Host).Should(Equal("5.6.7.8"))
		})
	})

	When("importing an invalid config file", func() {
		It("should fail and leave the database untouched", func() {
			broken := tmpDir.CreateStringFile("broken.yaml",
				"upstreams:",
				"  groups:",
				"    default:",
				"      - 1.broken file")

			Expect(execute("import", broken.Path, "--db-dir", tmpDir.Path)).Should(HaveOccurred())

			store, err := configstore.Open(tmpDir.Path)
			Expect(err).Should(Succeed())
			DeferCleanup(store.Close)

			fresh, err := store.IsFresh()
			Expect(err).Should(Succeed())
			Expect(fresh).Should(BeTrue())
		})
	})
})
