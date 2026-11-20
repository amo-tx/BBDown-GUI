package bilibili

import "testing"

func TestHttpsCover(t *testing.T) {
	cases := map[string]string{
		"http://i0.hdslb.com/bfs/archive/a.jpg":  "https://i0.hdslb.com/bfs/archive/a.jpg",
		"https://i0.hdslb.com/bfs/archive/a.jpg": "https://i0.hdslb.com/bfs/archive/a.jpg",
		"":                                       "",
	}
	for in, want := range cases {
		if got := httpsCover(in); got != want {
			t.Errorf("httpsCover(%q) = %q, want %q", in, got, want)
		}
	}
}
