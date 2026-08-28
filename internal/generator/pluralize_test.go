package generator

import "testing"

func TestPluralize(t *testing.T) {
	cases := map[string]string{
		"User":     "Users",
		"Class":    "Classes",
		"Box":      "Boxes",
		"Wish":     "Wishes",
		"Watch":    "Watches",
		"Category": "Categories",
		"Key":      "Keys",
		"Toy":      "Toys",
		"Bus":      "Buses",
	}
	for in, want := range cases {
		if got := pluralize(in); got != want {
			t.Errorf("pluralize(%q) = %q, want %q", in, got, want)
		}
	}
}
