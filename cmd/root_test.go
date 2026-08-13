package cmd

import (
	"io"
	"net/http"

	"github.com/0xERR0R/blocky/log"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// Mock implementation of codeWithStatus interface for testing
type mockResponse struct {
	statusCode int
	status     string
}

func (m mockResponse) StatusCode() int {
	return m.statusCode
}

func (m mockResponse) Status() string {
	return m.status
}

var _ = Describe("root command", func() {
	When("Help command is called", func() {
		log.Log().ExitFunc = nil
		It("should execute without error", func() {
			c := NewRootCommand()
			c.SetOut(io.Discard)
			c.SetArgs([]string{"help"})
			err := c.Execute()
			Expect(err).Should(Succeed())
		})
	})

	Describe("apiURL function", func() {
		It("should return correct URL with default values", func() {
			apiHost = defaultHost
			apiPort = defaultPort

			url := apiURL()
			Expect(url).Should(Equal("http://localhost:4000/api"))
		})

		It("should return correct URL with custom values", func() {
			apiHost = "127.0.0.1"
			apiPort = 8080

			url := apiURL()
			Expect(url).Should(Equal("http://127.0.0.1:8080/api"))
		})
	})

	Describe("printOkOrError function", func() {
		It("should return nil for OK status", func() {
			resp := mockResponse{
				statusCode: http.StatusOK,
				status:     "200 OK",
			}

			err := printOkOrError(resp, "")
			Expect(err).ShouldNot(HaveOccurred())
		})

		It("should return error for non-OK status", func() {
			resp := mockResponse{
				statusCode: http.StatusBadRequest,
				status:     "400 Bad Request",
			}

			err := printOkOrError(resp, "Error message")
			Expect(err).Should(HaveOccurred())
			Expect(err.Error()).Should(ContainSubstring("400 Bad Request"))
			Expect(err.Error()).Should(ContainSubstring("Error message"))
		})
	})

	Describe("Command execution", func() {
		BeforeEach(func() {
			// Reset to default values before each test
			apiHost = defaultHost
			apiPort = defaultPort
		})

		It("should create root command with all subcommands", func() {
			cmd := NewRootCommand()

			// Check if all subcommands are added
			subCmdNames := make([]string, 0, len(cmd.Commands()))
			for _, subCmd := range cmd.Commands() {
				subCmdNames = append(subCmdNames, subCmd.Name())
			}

			expectedCmds := []string{
				"refresh", "query", "version", "serve",
				"blocking", "lists", "healthcheck", "cache", "import", "validate",
			}
			for _, expected := range expectedCmds {
				Expect(subCmdNames).Should(ContainElement(expected))
			}
		})

		It("should set flags correctly", func() {
			cmd := NewRootCommand()

			// Test db-dir flag
			dbDirFlag := cmd.PersistentFlags().Lookup("db-dir")
			Expect(dbDirFlag).ShouldNot(BeNil())
			Expect(dbDirFlag.DefValue).Should(Equal("."))

			// Test apiHost flag
			apiHostFlag := cmd.PersistentFlags().Lookup("apiHost")
			Expect(apiHostFlag).ShouldNot(BeNil())
			Expect(apiHostFlag.DefValue).Should(Equal(defaultHost))

			// Test apiPort flag
			apiPortFlag := cmd.PersistentFlags().Lookup("apiPort")
			Expect(apiPortFlag).ShouldNot(BeNil())
			Expect(apiPortFlag.DefValue).Should(Equal("4000"))
		})

		It("should honor the db-dir env var for the flag default", func() {
			GinkgoT().Setenv(dbDirEnvVar, "/some/dir")

			cmd := NewRootCommand()
			Expect(cmd.PersistentFlags().Lookup("db-dir").DefValue).Should(Equal("/some/dir"))
		})
	})
})
