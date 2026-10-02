package httpx

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
)

func req(target string) *http.Request { return httptest.NewRequest(http.MethodGet, target, nil) }

func TestParsePage(t *testing.T) {
	p, err := ParsePage(req("/x"), 50, 200)
	if err != nil || p.Limit != 50 || p.After != "" {
		t.Fatalf("defaults: %+v %v", p, err)
	}
	p, err = ParsePage(req("/x?limit=10&cursor=RU1QLTAwMTA"), 50, 200)
	if err != nil || p.Limit != 10 || p.After != "EMP-0010" {
		t.Fatalf("explicit: %+v %v", p, err)
	}
	for _, q := range []string{"limit=0", "limit=201", "limit=abc", "limit=-1", "cursor=!!!"} {
		var apiErr *Error
		if _, err := ParsePage(req("/x?"+q), 50, 200); !errors.As(err, &apiErr) || apiErr.Status != 400 {
			t.Errorf("%s: expected 400, got %v", q, err)
		}
	}
}

func TestPaginate(t *testing.T) {
	p := PageRequest{Limit: 2}
	rows, meta := Paginate(p, []string{"a", "b", "c"}, func(s string) string { return s })
	if len(rows) != 2 || meta.NextCursor == nil {
		t.Fatalf("expected trimmed page with cursor: %v %+v", rows, meta)
	}
	next, err := ParsePage(req("/x?cursor="+*meta.NextCursor), 2, 10)
	if err != nil || next.After != "b" {
		t.Fatalf("cursor round trip: %+v %v", next, err)
	}
	rows, meta = Paginate(p, []string{"a"}, func(s string) string { return s })
	if len(rows) != 1 || meta.NextCursor != nil {
		t.Fatalf("last page must not have a cursor: %+v", meta)
	}
}

func TestSearchPatternEscapesWildcards(t *testing.T) {
	got, err := SearchPattern(req(`/x?q=50%25_off\`))
	if err != nil || *got != `%50\%\_off\\%` {
		t.Fatalf("got %v %v", got, err)
	}
	if got, _ := SearchPattern(req("/x?q=+++")); got != nil {
		t.Fatal("blank q must be ignored")
	}
	long := make([]byte, 101)
	for i := range long {
		long[i] = 'a'
	}
	if _, err := SearchPattern(req("/x?q=" + string(long))); err == nil {
		t.Fatal("overlong q must be rejected")
	}
}

func TestOptionalMatching(t *testing.T) {
	re := regexp.MustCompile(`^[A-Z]+$`)
	if v, err := OptionalMatching(req("/x?d=FIN"), "d", re); err != nil || *v != "FIN" {
		t.Fatalf("valid: %v %v", v, err)
	}
	if _, err := OptionalMatching(req("/x?d=fin%27%20OR%201"), "d", re); err == nil {
		t.Fatal("invalid value accepted")
	}
}
