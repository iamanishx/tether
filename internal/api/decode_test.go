package api

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDecodeRejectsTrailingJSON(t *testing.T) {
	for _, body := range []string{`{"name":"a"}{"name":"b"}`, `{"name":"a"}garbage`, `{"unexpected":1}`} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/", strings.NewReader(body))
		var v struct {
			Name string `json:"name"`
		}
		if decode(w, r, &v) || w.Code != 400 {
			t.Errorf("accepted %q status=%d", body, w.Code)
		}
	}
	w := httptest.NewRecorder()
	var v struct {
		Name string `json:"name"`
	}
	if !decode(w, httptest.NewRequest("POST", "/", strings.NewReader("{\"name\":\"a\"}\n ")), &v) {
		t.Fatal("valid JSON plus whitespace rejected")
	}
}
