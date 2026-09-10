package requestmodel

import (
	"reflect"
	"testing"
)

func TestModelCandidatesOwnStrings(t *testing.T) {
	for _, route := range []string{"", "/v1/live"} {
		body := []byte(`{"model":"top","MODEL":"second","session":{"model":"nested","mo\u0064el":"escaped"}}`)
		got := FromBodyCandidates(route, "application/json", body)
		want := []string{"top", "second"}
		if route != "" {
			want = []string{"nested", "escaped"}
		}
		clear(body)
		if !reflect.DeepEqual(want, got) {
			t.Fatalf("returned candidates borrow request storage: %v", got)
		}
	}
}
