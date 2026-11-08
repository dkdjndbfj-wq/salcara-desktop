package launcher

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"salcara/bridge/internal/config"
)

func TestModelCatalogDistinguishesEmptyFromUnsupportedSuccess(t *testing.T) {
	for _, body := range []string{`{}`, `null`, `{"data":null}`, `{"models":[]}`, `{"error":"unsupported"}`, `{"data":[{}]}`, `{"data":[]}`} {
		t.Run(body, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
			defer s.Close()
			_, err := Models(context.Background(), config.LocalAccount{BaseURL: s.URL, Key: "synthetic-key"})
			if err == nil || errors.Is(err, ErrEmptyModelCatalog) != (body == `{"data":[]}`) {
				t.Fatalf("wrong empty-directory classification: %v", err)
			}
		})
	}
}
