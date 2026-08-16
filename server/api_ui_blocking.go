package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/0xERR0R/blocky/configstore"
	"github.com/0xERR0R/blocky/querylog"

	"github.com/go-chi/chi/v5"
)

// maxPasteBytes / maxPasteTokens bound a pasted allow/deny list: every token is
// one fsynced SQLite commit on the USB SSD, so an uncapped 50k-domain paste
// writes for minutes (and an unbounded body OOMs the 1GB Pi).
const (
	maxPasteBytes  = 256 << 10 // 256 KiB body
	maxPasteTokens = 1000      // domains per request
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
		r.Put("/allow/{id}", b.setEntry(true))
		r.Put("/deny/{id}", b.setEntry(false))
		r.Delete("/allow/{id}", b.deleteEntry(true))
		r.Delete("/deny/{id}", b.deleteEntry(false))
		r.Post("/adlists", b.addAdlist)
		r.Put("/adlists/{id}", b.putAdlist)
		r.Delete("/adlists/{id}", b.deleteAdlist)
		r.Get("/groups", b.getGroups)
		r.Put("/groups/{name}", b.putGroup)
		r.Put("/groups/{name}/members", b.putGroupMembers)
		r.Put("/groups/{name}/enabled", b.putGroupEnabled)
		r.Delete("/groups/{name}", b.deleteGroup)
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
	ID      uint   `json:"id"`
	Group   string `json:"group"`
	Domain  string `json:"domain"`
	Enabled bool   `json:"enabled"`
	Comment string `json:"comment"`
}

type adlistJSON struct {
	ID      uint   `json:"id"`
	URL     string `json:"url"`
	Enabled bool   `json:"enabled"`
	Comment string `json:"comment"`
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
		allowJSON = append(allowJSON, blockingEntryJSON{ID: e.ID, Group: e.GroupName, Domain: e.Domain, Enabled: e.Enabled, Comment: e.Comment})
	}

	denyJSON := make([]blockingEntryJSON, 0, len(denies))
	for _, e := range denies {
		denyJSON = append(denyJSON, blockingEntryJSON{ID: e.ID, Group: e.GroupName, Domain: e.Domain, Enabled: e.Enabled, Comment: e.Comment})
	}

	adlists, err := b.store.ListAdlistEntries()
	if err != nil {
		internalError(rw, err)

		return
	}

	adlistJSONs := make([]adlistJSON, 0, len(adlists))
	for _, e := range adlists {
		adlistJSONs = append(adlistJSONs, adlistJSON{ID: e.ID, URL: e.URL, Enabled: e.Enabled, Comment: e.Comment})
	}

	writeJSON(rw, http.StatusOK, map[string]any{
		"categories": catsJSON,
		"segments":   segsJSON,
		"allow":      allowJSON,
		"deny":       denyJSON,
		"adlists":    adlistJSONs,
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

	if err := b.store.SetCategoryEnabled(pathParam(req, "name"), body.Enable); err != nil {
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

	if err := b.store.SetClientSegment(pathParam(req, "client"), body.Categories); err != nil {
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

		// bound the paste: an unbounded body OOMs the 1GB Pi, and each token
		// below is one fsynced SQLite commit on the USB SSD.
		req.Body = http.MaxBytesReader(rw, req.Body, maxPasteBytes)

		var body struct {
			Group   string `json:"group"`
			Domain  string `json:"domain"`
			Comment string `json:"comment"`
		}

		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				writeJSON(rw, http.StatusRequestEntityTooLarge,
					map[string]string{"error": fmt.Sprintf("paste too large (max %d bytes)", maxPasteBytes)})

				return
			}

			badRequest(rw, err)

			return
		}

		add := b.store.AddDenyEntry
		if isAllow {
			add = b.store.AddAllowEntry
		}

		// Accept a whole pasted list, not just one domain: split on any
		// whitespace or comma and strip a trailing URL path, then add each. So
		// the box takes "a.com" or "a.com b.com, c.com/x" equally.
		toks := strings.FieldsFunc(body.Domain, func(r rune) bool {
			return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == ','
		})

		if len(toks) > maxPasteTokens {
			writeJSON(rw, http.StatusRequestEntityTooLarge,
				map[string]string{"error": fmt.Sprintf("too many domains in one paste: %d (max %d)", len(toks), maxPasteTokens)})

			return
		}

		ids := make([]uint, 0)
		skipped := make([]string, 0)

		// ponytail: one commit per token (configstore has no exported bulk/Tx
		// API to call from here). Bounded by maxPasteTokens; batch into a single
		// transaction in configstore if bigger pastes ever matter.
		for _, tok := range toks {
			if i := strings.IndexByte(tok, '/'); i >= 0 {
				tok = tok[:i] // a domain, not a URL
			}

			if tok == "" {
				continue
			}

			id, err := add(body.Group, tok, body.Comment)
			if err != nil {
				skipped = append(skipped, tok)

				continue
			}

			ids = append(ids, id)
		}

		if len(ids) == 0 {
			badRequest(rw, fmt.Errorf("no valid domains in %q", body.Domain))

			return
		}

		writeJSON(rw, http.StatusOK, map[string]any{
			"added": len(ids), "ids": ids, "skipped": skipped, "needsApply": true,
		})
	}
}

func (b *blockingAPI) setEntry(isAllow bool) http.HandlerFunc {
	return func(rw http.ResponseWriter, req *http.Request) {
		if b.storeUnavailable(rw) {
			return
		}

		id, err := strconv.ParseUint(chi.URLParam(req, "id"), 10, 32)
		if err != nil {
			badRequest(rw, err)

			return
		}

		var body struct {
			Enabled bool   `json:"enabled"`
			Comment string `json:"comment"`
		}

		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			badRequest(rw, err)

			return
		}

		set := b.store.SetDenyEntry
		if isAllow {
			set = b.store.SetAllowEntry
		}

		if err := set(uint(id), body.Enabled, body.Comment); err != nil {
			badRequest(rw, err)

			return
		}

		writeJSON(rw, http.StatusOK, map[string]any{"needsApply": true})
	}
}

func (b *blockingAPI) addAdlist(rw http.ResponseWriter, req *http.Request) {
	if b.storeUnavailable(rw) {
		return
	}

	var body struct {
		URL     string `json:"url"`
		Comment string `json:"comment"`
	}

	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		badRequest(rw, err)

		return
	}

	id, err := b.store.AddAdlistEntry(body.URL, body.Comment)
	if err != nil {
		badRequest(rw, err)

		return
	}

	writeJSON(rw, http.StatusOK, map[string]any{"id": id, "needsApply": true})
}

func (b *blockingAPI) putAdlist(rw http.ResponseWriter, req *http.Request) {
	if b.storeUnavailable(rw) {
		return
	}

	id, err := strconv.ParseUint(chi.URLParam(req, "id"), 10, 32)
	if err != nil {
		badRequest(rw, err)

		return
	}

	// url present → edit url/comment; url absent → toggle enabled only.
	var body struct {
		URL     *string `json:"url"`
		Enabled bool    `json:"enabled"`
		Comment string  `json:"comment"`
	}

	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		badRequest(rw, err)

		return
	}

	if body.URL != nil {
		err = b.store.UpdateAdlistEntry(uint(id), *body.URL, body.Comment)
	} else {
		err = b.store.SetAdlistEnabled(uint(id), body.Enabled)
	}

	if err != nil {
		badRequest(rw, err)

		return
	}

	writeJSON(rw, http.StatusOK, map[string]any{"needsApply": true})
}

func (b *blockingAPI) deleteAdlist(rw http.ResponseWriter, req *http.Request) {
	if b.storeUnavailable(rw) {
		return
	}

	id, err := strconv.ParseUint(chi.URLParam(req, "id"), 10, 32)
	if err != nil {
		badRequest(rw, err)

		return
	}

	if err := b.store.DeleteAdlistEntry(uint(id)); err != nil {
		internalError(rw, err)

		return
	}

	writeJSON(rw, http.StatusOK, map[string]any{"needsApply": true})
}

type groupJSON struct {
	Name       string   `json:"name"`
	Enabled    bool     `json:"enabled"`
	Categories []string `json:"categories"`
	Members    []string `json:"members"`
}

// getGroups returns every household group plus the available category names, so
// the groups page can render its picker from one call.
func (b *blockingAPI) getGroups(rw http.ResponseWriter, _ *http.Request) {
	if b.storeUnavailable(rw) {
		return
	}

	groups, err := b.store.ListGroups()
	if err != nil {
		internalError(rw, err)

		return
	}

	cats, err := b.store.ListBlockingCategories()
	if err != nil {
		internalError(rw, err)

		return
	}

	groupsJSON := make([]groupJSON, 0, len(groups))
	for _, g := range groups {
		groupsJSON = append(groupsJSON, groupJSON{
			Name: g.Name, Enabled: g.Enabled, Categories: g.Categories, Members: g.Members,
		})
	}

	catNames := make([]string, 0, len(cats))
	for _, c := range cats {
		catNames = append(catNames, c.Name)
	}

	writeJSON(rw, http.StatusOK, map[string]any{"groups": groupsJSON, "categories": catNames})
}

func (b *blockingAPI) putGroup(rw http.ResponseWriter, req *http.Request) {
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

	if err := b.store.SaveGroup(pathParam(req, "name"), body.Categories); err != nil {
		badRequest(rw, err)

		return
	}

	writeJSON(rw, http.StatusOK, map[string]any{"needsApply": true})
}

func (b *blockingAPI) putGroupMembers(rw http.ResponseWriter, req *http.Request) {
	if b.storeUnavailable(rw) {
		return
	}

	var body struct {
		Members []string `json:"members"`
	}

	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		badRequest(rw, err)

		return
	}

	if err := b.store.SetGroupMembers(pathParam(req, "name"), body.Members); err != nil {
		badRequest(rw, err)

		return
	}

	writeJSON(rw, http.StatusOK, map[string]any{"needsApply": true})
}

func (b *blockingAPI) putGroupEnabled(rw http.ResponseWriter, req *http.Request) {
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

	if err := b.store.SetGroupEnabled(pathParam(req, "name"), body.Enable); err != nil {
		badRequest(rw, err)

		return
	}

	writeJSON(rw, http.StatusOK, map[string]any{"needsApply": true})
}

func (b *blockingAPI) deleteGroup(rw http.ResponseWriter, req *http.Request) {
	if b.storeUnavailable(rw) {
		return
	}

	if err := b.store.DeleteGroup(pathParam(req, "name")); err != nil {
		internalError(rw, err)

		return
	}

	writeJSON(rw, http.StatusOK, map[string]any{"needsApply": true})
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
