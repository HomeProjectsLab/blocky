package server

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/0xERR0R/blocky/configstore"
)

// sqliteMagic is the fixed 16-byte header of every SQLite database file.
const sqliteMagic = "SQLite format 3\x00"

// maxImportBytes caps an uploaded config.db restore. Real backups are KBs; the
// ceiling only exists to stop a hostile/accidental huge upload from OOMing the Pi.
const maxImportBytes = 64 << 20 // 64 MiB

// exportConfig streams a fresh, consistent snapshot of the whole config.db.
// The entire file is sent (not RawYAML) because LoadConfig overlays the
// upstream and blocking tables onto the YAML blob — the YAML alone is partial.
func (u *uiAPI) exportConfig(rw http.ResponseWriter, _ *http.Request) {
	if u.storeUnavailable(rw) {
		return
	}

	tmp, err := os.MkdirTemp("", "blocky-export")
	if err != nil {
		writeJSON(rw, http.StatusInternalServerError, map[string]string{"error": err.Error()})

		return
	}
	defer os.RemoveAll(tmp)

	snap := filepath.Join(tmp, "config.db")
	if err := u.store.SnapshotTo(snap); err != nil {
		writeJSON(rw, http.StatusInternalServerError, map[string]string{"error": err.Error()})

		return
	}

	f, err := os.Open(snap)
	if err != nil {
		writeJSON(rw, http.StatusInternalServerError, map[string]string{"error": err.Error()})

		return
	}
	defer f.Close()

	host, _ := os.Hostname()
	if host == "" {
		host = "blocky"
	}

	name := fmt.Sprintf("jungleblock-backup-%s-%s.db", host, time.Now().Format("2006-01-02"))

	rw.Header().Set(contentTypeHeader, "application/octet-stream")
	rw.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, name))

	if _, err := io.Copy(rw, f); err != nil {
		logger().Error("can't stream config export: ", err)
	}
}

// importConfig restores the whole config.db from an uploaded backup. The upload
// is sniffed for the SQLite magic, spooled to a tmp dir and fully validated
// (opened as an isolated store + LoadConfig) before the live store is touched;
// any validation error is a 400 and leaves the running config untouched.
func (u *uiAPI) importConfig(rw http.ResponseWriter, req *http.Request) {
	if u.storeUnavailable(rw) {
		return
	}

	// bound the upload: a config.db backup is KBs, but an unbounded io.Copy to
	// tmpfs OOMs the 1GB Pi (same failure class as the VACUUM incident).
	req.Body = http.MaxBytesReader(rw, req.Body, maxImportBytes)

	// temp dir on the SAME filesystem as config.db: RestoreDB does an os.Rename
	// of the upload onto the live DB path (USB-SSD), which is EXDEV — and fails —
	// if the upload sits on tmpfs.
	tmp, err := os.MkdirTemp(filepath.Dir(u.store.DBPath()), "blocky-import")
	if err != nil {
		writeJSON(rw, http.StatusInternalServerError, map[string]string{"error": err.Error()})

		return
	}
	defer os.RemoveAll(tmp)

	upload := filepath.Join(tmp, "config.db")

	out, err := os.Create(upload)
	if err != nil {
		writeJSON(rw, http.StatusInternalServerError, map[string]string{"error": err.Error()})

		return
	}

	// reject anything that isn't a SQLite file before spooling the rest.
	magic := make([]byte, len(sqliteMagic))
	if _, err := io.ReadFull(req.Body, magic); err != nil || string(magic) != sqliteMagic {
		_ = out.Close()
		writeJSON(rw, http.StatusBadRequest, map[string]string{"error": "not a SQLite database file"})

		return
	}

	if _, err := out.Write(magic); err != nil {
		_ = out.Close()
		writeJSON(rw, http.StatusInternalServerError, map[string]string{"error": err.Error()})

		return
	}

	if _, err := io.Copy(out, req.Body); err != nil {
		_ = out.Close()
		writeJSON(rw, http.StatusBadRequest, map[string]string{"error": err.Error()})

		return
	}

	if err := out.Close(); err != nil {
		writeJSON(rw, http.StatusInternalServerError, map[string]string{"error": err.Error()})

		return
	}

	// prove it IS a config database before any Open() touches it: Open
	// migrates+seeds any SQLite file, so without this a wrong upload (e.g. a
	// querylog.db) would "validate" and wipe the live config on restore.
	if err := configstore.VerifyConfigDB(upload); err != nil {
		writeJSON(rw, http.StatusBadRequest, map[string]string{"error": err.Error()})

		return
	}

	// validate in isolation: any error means the live store is never swapped.
	if err := validateConfigDB(tmp); err != nil {
		writeJSON(rw, http.StatusBadRequest, map[string]string{"error": err.Error()})

		return
	}

	if err := u.store.RestoreDB(upload); err != nil {
		writeJSON(rw, http.StatusInternalServerError, map[string]string{"error": err.Error()})

		return
	}

	u.store.RequestApply()

	writeJSON(rw, http.StatusAccepted, map[string]string{"status": "restored"})
}

// validateConfigDB opens the config.db in dir as its own store and runs the full
// LoadConfig pipeline (blob + overlay tables), proving the backup is loadable.
func validateConfigDB(dir string) error {
	st, err := configstore.Open(dir)
	if err != nil {
		return err
	}
	defer st.Close()

	if _, err := st.LoadConfig(); err != nil {
		return err
	}

	return nil
}
