package server

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/0xERR0R/blocky/configstore"
	"github.com/0xERR0R/blocky/querylog"

	"github.com/go-chi/chi/v5"
)

// blocklistStatser supplies per-category domain counts from the query-log
// database. *querylog.DecoySource implements it; nil = counts stay 0.
type blocklistStatser interface {
	BlocklistCategories() ([]querylog.BlocklistStat, error)
}

// registerBlockingUIEndpoints mounts the ad-blocker management API under
// /api/ui/blocking. store may be nil (503, like the other config endpoints).
func registerBlockingUIEndpoints(router *chi.Mux, store *configstore.Store, stats blocklistStatser) {
	b := &blockingAPI{uiAPI: &uiAPI{store: store}, stats: stats}

	router.Route("/api/ui/blocking", func(r chi.Router) {
		r.Get("/", b.get)
		r.Put("/categories/{name}", b.putCategory)
		r.Put("/segments/{client}", b.putSegment)
		r.Post("/allow", b.addEntry(true))
		r.Post("/deny", b.addEntry(false))
		r.Delete("/allow/{id}", b.deleteEntry(true))
		r.Delete("/deny/{id}", b.deleteEntry(false))
	})
}

type blockingAPI struct {
	*uiAPI

	stats blocklistStatser
}

type blockingCategoryJSON struct {
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
	Default bool   `json:"default"`
	Domains int64  `json:"domains"`
}

type blockingSegmentJSON struct {
	Client     string   `json:"client"`
	Categories []string `json:"categories"`
}

type blockingEntryJSON struct {
	ID     uint   `json:"id"`
	Group  string `json:"group"`
	Domain string `json:"domain"`
}

func (b *blockingAPI) get(rw http.ResponseWriter, _ *http.Request) {
	if b.storeUnavailable(rw) {
		return
	}

	cats, err := b.store.ListBlockingCategories()
	if err != nil {
		internalError(rw, err)

		return
	}

	counts := map[string]int64{}

	if b.stats != nil {
		if stats, err := b.stats.BlocklistCategories(); err == nil {
			for _, st := range stats {
				counts[st.Name] = st.Count
			}
		}
	}

	catsJSON := make([]blockingCategoryJSON, 0, len(cats))
	for _, c := range cats {
		catsJSON = append(catsJSON, blockingCategoryJSON{
			Name: c.Name, Enabled: c.Enabled, Default: c.IsDefault, Domains: counts[c.Name],
		})
	}

	segs, err := b.store.GetClientSegments()
	if err != nil {
		internalError(rw, err)

		return
	}

	segsJSON := make([]blockingSegmentJSON, 0, len(segs))
	for client, categories := range segs {
		segsJSON = append(segsJSON, blockingSegmentJSON{Client: client, Categories: categories})
	}

	allows, err := b.store.ListAllowEntries()
	if err != nil {
		internalError(rw, err)

		return
	}

	denies, err := b.store.ListDenyEntries()
	if err != nil {
		internalError(rw, err)

		return
	}

	allowJSON := make([]blockingEntryJSON, 0, len(allows))
	for _, e := range allows {
		allowJSON = append(allowJSON, blockingEntryJSON{ID: e.ID, Group: e.GroupName, Domain: e.Domain})
	}

	denyJSON := make([]blockingEntryJSON, 0, len(denies))
	for _, e := range denies {
		denyJSON = append(denyJSON, blockingEntryJSON{ID: e.ID, Group: e.GroupName, Domain: e.Domain})
	}

	writeJSON(rw, http.StatusOK, map[string]any{
		"categories": catsJSON,
		"segments":   segsJSON,
		"allow":      allowJSON,
		"deny":       denyJSON,
	})
}

func (b *blockingAPI) putCategory(rw http.ResponseWriter, req *http.Request) {
	if b.storeUnavailable(rw) {
		return
	}

	var body struct {
		Enable bool `json:"enable"`
	}

	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		badRequest(rw, err)

		return
	}

	if err := b.store.SetCategoryEnabled(chi.URLParam(req, "name"), body.Enable); err != nil {
		badRequest(rw, err)

		return
	}

	// list sources changed: the resolver's list caches are rebuilt on apply
	writeJSON(rw, http.StatusOK, map[string]any{"needsApply": true})
}

func (b *blockingAPI) putSegment(rw http.ResponseWriter, req *http.Request) {
	if b.storeUnavailable(rw) {
		return
	}

	var body struct {
		Categories []string `json:"categories"`
	}

	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		badRequest(rw, err)

		return
	}

	if err := b.store.SetClientSegment(chi.URLParam(req, "client"), body.Categories); err != nil {
		badRequest(rw, err)

		return
	}

	writeJSON(rw, http.StatusOK, map[string]any{"needsApply": true})
}

func (b *blockingAPI) addEntry(isAllow bool) http.HandlerFunc {
	return func(rw http.ResponseWriter, req *http.Request) {
		if b.storeUnavailable(rw) {
			return
		}

		var body struct {
			Group  string `json:"group"`
			Domain string `json:"domain"`
		}

		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			badRequest(rw, err)

			return
		}

		add := b.store.AddDenyEntry
		if isAllow {
			add = b.store.AddAllowEntry
		}

		id, err := add(body.Group, body.Domain)
		if err != nil {
			badRequest(rw, err)

			return
		}

		writeJSON(rw, http.StatusOK, map[string]any{"id": id, "needsApply": true})
	}
}

func (b *blockingAPI) deleteEntry(isAllow bool) http.HandlerFunc {
	return func(rw http.ResponseWriter, req *http.Request) {
		if b.storeUnavailable(rw) {
			return
		}

		id, err := strconv.ParseUint(chi.URLParam(req, "id"), 10, 32)
		if err != nil {
			badRequest(rw, err)

			return
		}

		del := b.store.DeleteDenyEntry
		if isAllow {
			del = b.store.DeleteAllowEntry
		}

		if err := del(uint(id)); err != nil {
			internalError(rw, err)

			return
		}

		writeJSON(rw, http.StatusOK, map[string]any{"needsApply": true})
	}
}
