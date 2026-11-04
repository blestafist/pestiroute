package ui

import (
	"net/http/httptest"
	"testing"
)

func TestGoOnlyBuildReportsUIUnavailable(t *testing.T) {
	response := httptest.NewRecorder()
	Handler().ServeHTTP(response, httptest.NewRequest("GET", "/admin/", nil))
	if response.Code != 503 || response.Body.String() == "" {
		t.Fatalf("status=%d body=%q; want explicit unavailable response", response.Code, response.Body.String())
	}
}
