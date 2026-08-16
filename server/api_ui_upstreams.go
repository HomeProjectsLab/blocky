package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/0xERR0R/blocky/config"
	"github.com/0xERR0R/blocky/configstore"

	"github.com/go-chi/chi/v5"
)

// upstreamSwapper replaces a group's upstreams in the running resolver tree.
// *Server implements it.
type upstreamSwapper interface {
	SwapUpstreams(ctx context.Context, group string, upstreams []config.Upstream) error
}

type upstreamEntryJSON struct {
	ID       uint   `json:"id"`
	Address  string `json:"address"`
	Weight   uint   `json:"weight"`
	Enabled  bool   `json:"enabled"`
	Position int    `json:"position"`
}

type upstreamGroupJSON struct {
	Name     string              `json:"name"`
	Strategy string              `json:"strategy"`
	HopMin   string              `json:"hopMin"`
	HopMax   string              `json:"hopMax"`
	Entries  []upstreamEntryJSON `json:"entries"`
}

func (u *uiAPI) getUpstreams(rw http.ResponseWriter, _ *http.Request) {
	if u.storeUnavailable(rw) {
		return
	}

	groups, entries, err := u.store.ListUpstreamGroups()
	if err != nil {
		writeJSON(rw, http.StatusInternalServerError, map[string]string{"error": err.Error()})

		return
	}

	out := make([]upstreamGroupJSON, 0, len(groups))

	for _, g := range groups {
		gj := upstreamGroupJSON{
			Name:     g.Name,
			Strategy: g.Strategy,
			HopMin:   time.Duration(g.HopMin).String(),
			HopMax:   time.Duration(g.HopMax).String(),
			Entries:  make([]upstreamEntryJSON, 0, len(entries[g.Name])),
		}

		for _, e := range entries[g.Name] {
			gj.Entries = append(gj.Entries, upstreamEntryJSON{
				ID:       e.ID,
				Address:  e.Address,
				Weight:   e.Weight,
				Enabled:  e.Enabled,
				Position: e.Position,
			})
		}

		out = append(out, gj)
	}

	writeJSON(rw, http.StatusOK, map[string]any{"groups": out})
}

func (u *uiAPI) putUpstreamGroup(rw http.ResponseWriter, req *http.Request) {
	if u.storeUnavailable(rw) {
		return
	}

	var body struct {
		Strategy string          `json:"strategy"`
		HopMin   config.Duration `json:"hopMin"`
		HopMax   config.Duration `json:"hopMax"`
	}

	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeJSON(rw, http.StatusBadRequest, map[string]string{"error": err.Error()})

		return
	}

	g := configstore.UpstreamGroup{
		Name:     chi.URLParam(req, "name"),
		Strategy: body.Strategy,
		HopMin:   int64(body.HopMin.ToDuration()),
		HopMax:   int64(body.HopMax.ToDuration()),
	}

	if err := u.store.PutUpstreamGroup(g); err != nil {
		writeJSON(rw, http.StatusBadRequest, map[string]string{"error": err.Error()})

		return
	}

	// strategy/group topology changes need a rebuilt resolver tree
	writeJSON(rw, http.StatusOK, map[string]any{"needsApply": true})
}

func (u *uiAPI) deleteUpstreamGroup(rw http.ResponseWriter, req *http.Request) {
	if u.storeUnavailable(rw) {
		return
	}

	if err := u.store.DeleteUpstreamGroup(chi.URLParam(req, "name")); err != nil {
		writeJSON(rw, http.StatusBadRequest, map[string]string{"error": err.Error()})

		return
	}

	// needsApply semantics: removing a branch needs a rebuilt resolver tree
	rw.WriteHeader(http.StatusNoContent)
}

func (u *uiAPI) putUpstreamEntries(rw http.ResponseWriter, req *http.Request) {
	if u.storeUnavailable(rw) {
		return
	}

	group := chi.URLParam(req, "name")

	var body struct {
		Entries []struct {
			Address string `json:"address"`
			Weight  uint   `json:"weight"`
			Enabled *bool  `json:"enabled"` // absent = true
		} `json:"entries"`
	}

	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeJSON(rw, http.StatusBadRequest, map[string]string{"error": err.Error()})

		return
	}

	entries := make([]configstore.UpstreamEntry, 0, len(body.Entries))
	for i, e := range body.Entries {
		entries = append(entries, configstore.UpstreamEntry{
			Address:  e.Address,
			Weight:   e.Weight,
			Enabled:  e.Enabled == nil || *e.Enabled,
			Position: i,
		})
	}

	if err := u.store.SetUpstreamEntries(group, entries); err != nil {
		writeJSON(rw, http.StatusBadRequest, map[string]string{"error": err.Error()})

		return
	}

	// persisted — now try to swap the running tree in place
	if u.swapper == nil {
		writeJSON(rw, http.StatusOK, map[string]any{
			"swapped": false, "needsApply": true, "reason": "no running server to swap",
		})

		return
	}

	upstreams, err := configstore.UpstreamsFromEntries(entries)
	if err != nil {
		// can't happen: SetUpstreamEntries already parsed every address
		writeJSON(rw, http.StatusOK, map[string]any{"swapped": false, "needsApply": true, "reason": err.Error()})

		return
	}

	if err := u.swapper.SwapUpstreams(req.Context(), group, upstreams); err != nil {
		writeJSON(rw, http.StatusOK, map[string]any{"swapped": false, "needsApply": true, "reason": err.Error()})

		return
	}

	writeJSON(rw, http.StatusOK, map[string]any{"swapped": true})
}

func (u *uiAPI) getConditional(rw http.ResponseWriter, _ *http.Request) {
	if u.storeUnavailable(rw) {
		return
	}

	mapping, err := u.store.GetConditional()
	if err != nil {
		internalError(rw, err)

		return
	}

	writeJSON(rw, http.StatusOK, map[string]any{"mapping": mapping})
}

func (u *uiAPI) putConditional(rw http.ResponseWriter, req *http.Request) {
	if u.storeUnavailable(rw) {
		return
	}

	var body struct {
		Domain    string   `json:"domain"`
		Upstreams []string `json:"upstreams"` // absent/empty = delete the mapping
	}

	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		badRequest(rw, err)

		return
	}

	domain := strings.TrimSpace(body.Domain)
	if domain == "" {
		writeJSON(rw, http.StatusBadRequest, map[string]string{"error": "domain is required"})

		return
	}

	var up []string

	for _, s := range body.Upstreams {
		if t := strings.TrimSpace(s); t != "" {
			up = append(up, t)
		}
	}

	var err error
	if len(up) == 0 {
		err = u.store.DeleteConditionalMapping(domain)
	} else {
		err = u.store.SetConditionalMapping(domain, up)
	}

	if err != nil {
		badRequest(rw, err)

		return
	}

	u.store.RequestApply()

	writeJSON(rw, http.StatusOK, map[string]any{"needsApply": true})
}
