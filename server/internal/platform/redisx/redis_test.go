package redisx

import "testing"

func TestBuildKey(t *testing.T) {
	cases := []struct {
		prefix string
		parts  []string
		want   string
	}{
		{"marpokat:", []string{"ch", "t1", "c1"}, "marpokat:ch:t1:c1"},
		{"marpokat:", []string{"presence"}, "marpokat:presence"},
		{"", []string{"a", "b"}, "a:b"},
		{"marpokat:", nil, "marpokat:"},
	}
	for _, c := range cases {
		if got := BuildKey(c.prefix, c.parts...); got != c.want {
			t.Errorf("BuildKey(%q, %v) = %q, want %q", c.prefix, c.parts, got, c.want)
		}
	}
}
