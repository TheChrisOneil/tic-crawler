package robots

import "testing"

const centene = `User-agent: *
Disallow: /page/
Disallow: /?
Disallow: /pay stubs
Noindex: /investor-day.html
Allow: /

Sitemap: https://www.centene.com/.sitemap.xml
`
const ua = "EverBetter-TiC-Crawler/1.0 (+x@y.com)"

func TestProductToken(t *testing.T) {
	if got := ProductToken(ua); got != "everbetter-tic-crawler" {
		t.Fatal(got)
	}
}

func TestCenteneQueryRuleDoesNotBlockEverything(t *testing.T) {
	r := Parse(centene)
	cases := map[string]bool{
		"https://www.centene.com/price-transparency-files.html": true,
		"https://www.centene.com/?q=1":                          false,
		"https://www.centene.com/page/x":                        false,
		"https://www.centene.com/pay%20stubs/a":                 false,
	}
	for u, want := range cases {
		if got := r.CanFetch(ua, u); got != want {
			t.Errorf("%s: got %v want %v", u, got, want)
		}
	}
}

func TestMatching(t *testing.T) {
	cases := []struct {
		text, url string
		want      bool
	}{
		{"User-agent: *\nDisallow: /a\nAllow: /a/b\n", "https://h/a/b/c", true},
		{"User-agent: *\nAllow: /a\nDisallow: /a/b\n", "https://h/a/b/c", false},
		{"User-agent: *\nDisallow: /x\nAllow: /x\n", "https://h/x", true},
		{"User-agent: *\nDisallow: /*.json$\n", "https://h/f.json", false},
		{"User-agent: *\nDisallow: /*.json$\n", "https://h/f.json.gz", true},
		{"User-agent: *\nDisallow: /tmp*/x\n", "https://h/tmp123/x", false},
		{"User-agent: *\nDisallow:\n", "https://h/anything", true},
		{"User-agent: *\nDisallow: /\n", "https://h/robots.txt", true},
		{"User-agent: other\nDisallow: /\n", "https://h/a", true},
		{"User-agent: everbetter-tic-crawler\nDisallow: /\nUser-agent: *\nAllow: /\n", "https://h/a", false},
		{"User-agent: a\nUser-agent: *\nDisallow: /p\n", "https://h/p", false},
		{"Disallow: /\n", "https://h/a", true},
		{"User-agent: *\nDisallow: /%7Euser\n", "https://h/~user/x", false},
	}
	for _, c := range cases {
		if got := Parse(c.text).CanFetch(ua, c.url); got != c.want {
			t.Errorf("%q %s: got %v want %v", c.text, c.url, got, c.want)
		}
	}
}

func TestAllowAll(t *testing.T) {
	if !(&Rules{AllowAll: true}).CanFetch(ua, "https://h/x") {
		t.Fatal("allow all")
	}
}
