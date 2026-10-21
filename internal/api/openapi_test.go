package api_test

import (
	"net/http"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.yaml.in/yaml/v3"

	"github.com/itsivali/lottery-platform/internal/api"
	"github.com/itsivali/lottery-platform/internal/platform/httpx"
)

// The OpenAPI file is the contract; this keeps the router and the document from drifting apart in either direction.
func TestOpenAPI_DocumentsExactlyTheRoutesTheRouterServes(t *testing.T) {
	raw, err := os.ReadFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	documented := map[string]bool{}
	for path, ops := range doc.Paths {
		for method := range ops {
			switch method {
			case "get", "post", "put", "patch", "delete":
				documented[strings.ToUpper(method)+" "+path] = true
			}
		}
	}

	served := map[string]bool{}
	h := httpx.NewRouterWith(nil, api.Mount(api.Deps{})).(chi.Routes)
	if err := chi.Walk(h, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		served[method+" "+strings.TrimSuffix(route, "/")] = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	for r := range served {
		if !documented[r] {
			t.Errorf("route %s is served but not in api/openapi.yaml", r)
		}
	}
	for r := range documented {
		if !served[r] {
			t.Errorf("route %s is in api/openapi.yaml but not served", r)
		}
	}
	if len(served) < 12 {
		keys := make([]string, 0, len(served))
		for k := range served {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		t.Fatalf("suspiciously few routes served: %v", keys)
	}
}
