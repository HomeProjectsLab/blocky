package querylog

// Focused SQLite two-writer contention harness, gated behind BLOCKY_STRESS=1.
// This isolates the system's #1 stated risk: the DatabaseWriter (batched flush,
// its own single connection) and the DecoySource (a SEPARATE read-write handle:
// corpus upserts, list samples, cohort/session scans) both hitting one WAL file,
// plus a read-only Reader — all concurrent. It hammers every writer/reader path
// simultaneously and asserts zero "database is locked" / SQLITE_BUSY.
//
//	Run: BLOCKY_STRESS=1 go test ./querylog/ -run TestStressContention -v -timeout 5m

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/0xERR0R/blocky/config"
	"github.com/0xERR0R/blocky/log"
	"github.com/0xERR0R/blocky/model"
)

type qlLockHook struct{ n atomic.Int64 }

func (h *qlLockHook) Levels() []logrus.Level { return logrus.AllLevels }
func (h *qlLockHook) Fire(e *logrus.Entry) error {
	m := e.Message
	if e.Data != nil {
		if err, ok := e.Data[logrus.ErrorKey].(error); ok && err != nil {
			m += " " + err.Error()
		}
	}
	for _, s := range []string{"database is locked", "database table is locked", "SQLITE_BUSY", "database is busy"} {
		if containsFoldQL(m, s) {
			h.n.Add(1)
			break
		}
	}
	return nil
}

func containsFoldQL(s, sub string) bool {
	lower := func(b byte) byte {
		if b >= 'A' && b <= 'Z' {
			return b + 32
		}
		return b
	}
outer:
	for i := 0; i+len(sub) <= len(s); i++ {
		for j := 0; j < len(sub); j++ {
			if lower(s[i+j]) != lower(sub[j]) {
				continue outer
			}
		}
		return true
	}
	return false
}

func TestStressContention(t *testing.T) {
	if os.Getenv("BLOCKY_STRESS") != "1" {
		t.Skip("set BLOCKY_STRESS=1 to run the SQLite contention harness")
	}

	hook := &qlLockHook{}
	log.Log().AddHook(hook)
	log.Log().SetLevel(logrus.WarnLevel)

	dbPath := t.TempDir() + "/querylog.db"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// writer: 50ms flush => maximally frequent write transactions.
	writer, err := NewDatabaseWriter(ctx, config.QueryLogTypeSqlite, dbPath, 7, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("writer: %v", err)
	}

	src, err := NewDecoySource(dbPath)
	if err != nil {
		t.Fatalf("decoy source: %v", err)
	}
	defer src.Close()

	// seed a small decoy list + blocklist so the samplers hit real rows.
	seedLines := ""
	for i := 0; i < 2000; i++ {
		seedLines += fmt.Sprintf("d%d.example.com\n", i)
	}
	if _, err := src.SeedIfEmpty(strings.NewReader(seedLines)); err != nil {
		t.Fatalf("seed decoy: %v", err)
	}
	blLines := ""
	for i := 0; i < 2000; i++ {
		blLines += fmt.Sprintf("blocked%d.ads.example\n", i)
	}
	if _, err := src.SeedBlocklistIfEmpty("ads", strings.NewReader(blLines)); err != nil {
		t.Fatalf("seed blocklist: %v", err)
	}

	// prime some real log rows so cohort/session/replay samplers return data.
	for i := 0; i < 3000; i++ {
		writer.Write(realEntry(i))
	}
	if err := writer.Flush(); err != nil {
		t.Fatalf("prime flush: %v", err)
	}

	var (
		wg        sync.WaitGroup
		writes    atomic.Int64
		corpusW   atomic.Int64
		samples   atomic.Int64
		reads     atomic.Int64
		opErrLock atomic.Int64 // operation-level "locked/busy" errors
		otherErr  atomic.Int64
	)
	run := int32(1)
	stop := func() bool { return atomic.LoadInt32(&run) == 0 }

	var sampleErrMu sync.Mutex
	var sampleErrs []string

	classify := func(err error) {
		if err == nil {
			return
		}
		if containsFoldQL(err.Error(), "database is locked") || containsFoldQL(err.Error(), "SQLITE_BUSY") ||
			containsFoldQL(err.Error(), "database is busy") || containsFoldQL(err.Error(), "table is locked") {
			opErrLock.Add(1)
		} else {
			otherErr.Add(1)
			sampleErrMu.Lock()
			if len(sampleErrs) < 5 {
				sampleErrs = append(sampleErrs, err.Error())
			}
			sampleErrMu.Unlock()
		}
	}

	// 1) writer ingest: constant Write() so every 50ms flush carries a big batch
	//    (raw insert + aggregates + noise_corpus upsert + prune, one tx each).
	for w := 0; w < 6; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			i := id * 1_000_000
			for !stop() {
				writer.Write(realEntry(i))
				i++
				writes.Add(1)
			}
		}(w)
	}

	// 2) DecoySource WRITE path: AddToCorpus on its own connection, concurrent
	//    with the writer's noise_corpus upsert — the exact two-writer scenario.
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			rnd := rand.New(rand.NewSource(int64(id) + 7))
			for !stop() {
				err := src.AddToCorpus(fmt.Sprintf("corpus%d.example.net", rnd.Intn(5000)))
				classify(err)
				corpusW.Add(1)
			}
		}(w)
	}

	// 3) DecoySource READ/sample path: the decoy engine's hot sampling.
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			rnd := rand.New(rand.NewSource(int64(id) + 99))
			for !stop() {
				switch rnd.Intn(7) {
				case 0:
					_, err := src.SampleList()
					classify(err)
				case 1:
					_, err := src.SampleCorpus()
					classify(err)
				case 2:
					_, err := src.SampleRecentReal(8)
					classify(err)
				case 3:
					_, err := src.SampleCohort()
					classify(err)
				case 4:
					_, err := src.NextInSession("example0.com")
					classify(err)
				case 5:
					_, err := src.SampleBlocklist()
					classify(err)
				case 6:
					_, err := src.SampleRealFingerprint()
					classify(err)
				}
				samples.Add(1)
			}
		}(w)
	}

	// 4) read-only Reader (the dashboards), opened once, queried hot.
	reader, err := NewReader(dbPath)
	if err != nil {
		t.Fatalf("reader: %v", err)
	}
	defer reader.Close()
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			to := time.Now()
			from := to.Add(-24 * time.Hour)
			for !stop() {
				if _, err := reader.Overview(from, to); err != nil {
					classify(err)
				}
				reads.Add(1)
			}
		}()
	}

	dur := 20 * time.Second
	if v := os.Getenv("BLOCKY_STRESS_DURATION"); v != "" {
		if d, e := time.ParseDuration(v); e == nil {
			dur = d
		}
	}
	t.Logf("contention running %s: 6 ingest + 4 corpus-writers + 8 samplers + 4 readers on %s", dur, dbPath)
	time.Sleep(dur)
	atomic.StoreInt32(&run, 0)
	wg.Wait()
	cancel()
	_ = writer.Flush()

	fmt.Printf("\n=========== SQLITE CONTENTION REPORT ===========\n")
	fmt.Printf("writer.Write calls   : %d\n", writes.Load())
	fmt.Printf("AddToCorpus calls    : %d\n", corpusW.Load())
	fmt.Printf("decoy sample calls   : %d\n", samples.Load())
	fmt.Printf("reader queries       : %d\n", reads.Load())
	fmt.Printf("op-level lock/busy   : %d\n", opErrLock.Load())
	fmt.Printf("op-level other errs  : %d\n", otherErr.Load())
	fmt.Printf("log lock/busy events : %d\n", hook.n.Load())
	for _, e := range sampleErrs {
		fmt.Printf("  sample other-err   : %s\n", e)
	}
	fmt.Printf("================================================\n\n")

	if opErrLock.Load() > 0 || hook.n.Load() > 0 {
		t.Errorf("SQLite lock/busy contention: op=%d log=%d (expected 0)", opErrLock.Load(), hook.n.Load())
	}
	if otherErr.Load() > 0 {
		t.Errorf("unexpected non-lock errors: %d", otherErr.Load())
	}
}

func realEntry(i int) *LogEntry {
	return &LogEntry{
		Start:          time.Now().Add(-time.Duration(i%3600) * time.Second),
		ClientIP:       fmt.Sprintf("10.0.%d.%d", (i/250)%250, i%250),
		ClientNames:    []string{fmt.Sprintf("client%d", i%20)},
		DurationMs:     int64(i % 200),
		ResponseReason: "resolved",
		ResponseType:   "RESOLVED",
		ResponseCode:   "NOERROR",
		QuestionType:   "A",
		QuestionName:   fmt.Sprintf("real%d.site%d.com", i%500, i%50),
		Answer:         "93.184.216.34",
		SocketProtocol: model.RequestProtocolUDP,
		Decoy:          false,
	}
}
