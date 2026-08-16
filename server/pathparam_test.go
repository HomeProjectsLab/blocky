package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
)

// A CIDR client segment identifier must survive the round trip through a chi
// path param: chi routes on the escaped path, so %2F would otherwise be stored
// literally and the subnet profile would silently never match (fail open).
func TestPathParamUnescapesCIDRClient(t *testing.T) {
	var got string

	r := chi.NewRouter()
	r.Put("/api/ui/blocking/segments/{client}", func(w http.ResponseWriter, req *http.Request) {
		got = pathParam(req, "client")
	})

	req := httptest.NewRequest(http.MethodPut, "/api/ui/blocking/segments/192.168.1.0%2F24", nil)
	r.ServeHTTP(httptest.NewRecorder(), req)

	if want := "192.168.1.0/24"; got != want {
		t.Fatalf("pathParam = %q, want %q", got, want)
	}
}
