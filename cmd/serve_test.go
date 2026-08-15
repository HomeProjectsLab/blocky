package cmd

import (
	"net"
	"strings"
	"syscall"
	"time"

	"github.com/0xERR0R/blocky/configstore"
	"github.com/0xERR0R/blocky/helpertest"
	"github.com/0xERR0R/blocky/log"

	"github.com/sirupsen/logrus/hooks/test"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// freeTCPPort asks the kernel for a free port; avoids collisions when
// multiple package test binaries run in parallel (fixed bases collided).
func freeTCPPort() string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	Expect(err).Should(Succeed())
	defer ln.Close()

	_, port, err := net.SplitHostPort(ln.Addr().String())
	Expect(err).Should(Succeed())

	return port
}

func minimalYAML(dnsPort string, extraLines ...string) string {
	lines := []string{
		"upstreams:",
		"  groups:",
		"    default:",
		"      - 1.1.1.1",
		"ports:",
		"  dns: " + dnsPort,
	}

	return strings.Join(append(lines, extraLines...), "\n") + "\n"
}

func dialable(port string) func(g Gomega) {
	return func(g Gomega) {
		conn, err := net.DialTimeout("tcp", "127.0.0.1:"+port, 200*time.Millisecond)
		g.Expect(err).Should(Succeed())
		defer conn.Close()
	}
}

var _ = Describe("Serve command", func() {
	var (
		tmpDir *helpertest.TmpFolder
		store  *configstore.Store
		port   string
	)

	BeforeEach(func() {
		port = freeTCPPort()
		tmpDir = helpertest.NewTmpFolder("db")

		var err error
		store, err = configstore.Open(tmpDir.Path)
		Expect(err).Should(Succeed())
		DeferCleanup(store.Close)

		Expect(store.SetRawYAML(minimalYAML(port))).Should(Succeed())
	})

	When("startServer is called with a prepared config database", func() {
		It("should start without error and terminate with signal", func() {
			dbDir = tmpDir.Path

			errChan := make(chan error)
			go func() {
				// blocking function, call async
				errChan <- startServer(newServeCommand(), []string{})
			}()

			By("check DNS port is open", func() {
				Eventually(dialable(port), "5s").Should(Succeed())
			})

			By("terminate with signal", func() {
				signals <- syscall.SIGINT

				Eventually(errChan, "15s").Should(Receive(BeNil()))
			})
		})
	})

	When("a config apply is requested", func() {
		It("should restart the server with the new config", func() {
			errChan := make(chan error)
			go func() {
				errChan <- runSupervisor(store)
			}()

			Eventually(dialable(port), "5s").Should(Succeed())

			newPort := freeTCPPort()

			By("apply new config with different DNS port", func() {
				Expect(store.SetRawYAML(minimalYAML(newPort))).Should(Succeed())
				store.RequestApply()
			})

			By("server serves the new port and released the old one", func() {
				Eventually(dialable(newPort), "5s").Should(Succeed())
				Eventually(dialable(port), "5s").ShouldNot(Succeed())
			})

			By("terminate with signal", func() {
				signals <- syscall.SIGINT

				Eventually(errChan, "15s").Should(Receive(BeNil()))
			})
		})

		It("should hot-swap a listener-compatible change without restarting", func() {
			loggerHook := test.NewGlobal()
			log.Log().AddHook(loggerHook)
			DeferCleanup(loggerHook.Reset)

			errChan := make(chan error)
			go func() {
				errChan <- runSupervisor(store)
			}()

			Eventually(dialable(port), "5s").Should(Succeed())

			By("apply a resolver-only change on the same ports (adds a denylist)", func() {
				Expect(store.SetRawYAML(minimalYAML(port,
					"blocking:",
					"  denylists:",
					"    ads:",
					"      - |",
					"        example.com",
					"  clientGroupsBlock:",
					"    default:",
					"      - ads"))).Should(Succeed())
				store.RequestApply()
			})

			By("config is applied via hot-swap, keeping the same listener", func() {
				Eventually(func() []string {
					msgs := make([]string, 0, len(loggerHook.AllEntries()))
					for _, entry := range loggerHook.AllEntries() {
						msgs = append(msgs, entry.Message)
					}

					return msgs
				}, "10s").Should(ContainElement(ContainSubstring("without dropping listeners")))

				// the port never dropped, and no full-restart message was logged
				Consistently(dialable(port), "300ms", "100ms").Should(Succeed())
				for _, entry := range loggerHook.AllEntries() {
					Expect(entry.Message).ShouldNot(ContainSubstring("full restart"))
				}
			})

			By("terminate with signal", func() {
				signals <- syscall.SIGINT

				Eventually(errChan, "15s").Should(Receive(BeNil()))
			})
		})

		It("should roll back to the last applied config if the rebuild fails", func() {
			loggerHook := test.NewGlobal()
			log.Log().AddHook(loggerHook)
			DeferCleanup(loggerHook.Reset)

			errChan := make(chan error)
			go func() {
				errChan <- runSupervisor(store)
			}()

			Eventually(dialable(port), "5s").Should(Succeed())

			blockedPort := freeTCPPort()

			By("occupy the HTTP port of the new config", func() {
				ln, err := net.Listen("tcp", "127.0.0.1:"+blockedPort)
				Expect(err).Should(Succeed())
				DeferCleanup(ln.Close)
			})

			By("apply config whose HTTP listener can't be created", func() {
				Expect(store.SetRawYAML(minimalYAML(port,
					"  http: 127.0.0.1:"+blockedPort))).Should(Succeed())
				store.RequestApply()
			})

			By("supervisor rolls back and keeps serving the old config", func() {
				Eventually(func() []string {
					msgs := make([]string, 0, len(loggerHook.AllEntries()))
					for _, entry := range loggerHook.AllEntries() {
						msgs = append(msgs, entry.Message)
					}

					return msgs
				}, "10s").Should(ContainElement(ContainSubstring("rolling back")))

				Eventually(dialable(port), "5s").Should(Succeed())
				Consistently(errChan, "200ms").ShouldNot(Receive())
			})

			By("terminate with signal", func() {
				signals <- syscall.SIGINT

				Eventually(errChan, "15s").Should(Receive(BeNil()))
			})
		})
	})

	When("the DNS port is already in use", func() {
		It("should fail to start and report the error", func() {
			By("occupy the DNS port", func() {
				ln, err := net.Listen("tcp", ":"+port)
				Expect(err).Should(Succeed())
				DeferCleanup(ln.Close)
			})

			errChan := make(chan error)
			go func() {
				errChan <- runSupervisor(store)
			}()

			var startError error
			Eventually(errChan, "10s").Should(Receive(&startError))
			Expect(startError).Should(MatchError(ContainSubstring("address already in use")))
		})
	})
})
