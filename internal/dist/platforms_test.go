package dist

import "testing"

func TestPlatformsMatch(t *testing.T) {
	cases := []struct {
		name      string
		platforms []string
		goos      string
		goarch    string
		want      bool
	}{
		{"empty applies everywhere", nil, "linux", "amd64", true},
		{"exact GOOS/GOARCH", []string{"darwin/arm64"}, "darwin", "arm64", true},
		{"wrong arch", []string{"darwin/arm64"}, "darwin", "amd64", false},
		{"wrong OS", []string{"darwin/arm64"}, "windows", "amd64", false},
		{"bare GOOS matches any arch", []string{"windows"}, "windows", "arm64", true},
		{"server linux excluded", []string{"darwin/arm64"}, "linux", "amd64", false},
	}
	for _, c := range cases {
		if got := platformsMatch(c.platforms, c.goos, c.goarch); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}
