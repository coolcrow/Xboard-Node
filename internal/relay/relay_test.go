package relay

import (
	"strings"
	"testing"
)

func TestBuckets(t *testing.T) {
	cases := []struct {
		in    string
		d, c, u []int
		err   bool
	}{
		{"443,18443", []int{443, 18443}, nil, nil, false},
		{"443/udp, 18443/tcp", nil, []int{18443}, []int{443}, false},
		{" 443 , 53/udp ", []int{443}, nil, []int{53}, false},
		{"", nil, nil, nil, true},
		{"abc", nil, nil, nil, true},
		{"443/sctp", nil, nil, nil, true},
		{"0", nil, nil, nil, true},
		{"70000", nil, nil, nil, true},
	}
	for _, tc := range cases {
		d, c, u, err := buckets(tc.in)
		if tc.err {
			if err == nil {
				t.Errorf("buckets(%q): want error, got nil", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("buckets(%q): %v", tc.in, err)
			continue
		}
		if !eq(d, tc.d) || !eq(c, tc.c) || !eq(u, tc.u) {
			t.Errorf("buckets(%q) = %v/%v/%v, want %v/%v/%v", tc.in, d, c, u, tc.d, tc.c, tc.u)
		}
	}
}

func eq(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestFilesMatchesScriptFormat(t *testing.T) {
	// 与 relay-setup.sh gen_conf 输出对齐的黄金样例
	wantDual := `[network]
no_tcp = false
use_udp = true

# managed by xboard-node agent (sync.relay)
[[endpoints]]
listen = "0.0.0.0:443"
remote = "198.44.54.109:443"
[[endpoints]]
listen = "0.0.0.0:18443"
remote = "198.44.54.109:18443"
`
	fs, err := files(Spec{Enabled: true, LandingHost: "198.44.54.109", Ports: "443,18443"})
	if err != nil {
		t.Fatal(err)
	}
	if got := fs["dual"]; got != wantDual {
		t.Errorf("dual toml mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, wantDual)
	}
	if _, has := fs["tcp"]; has {
		t.Error("empty tcp bucket should be absent")
	}
	if _, has := fs["udp"]; has {
		t.Error("empty udp bucket should be absent")
	}
}

func TestFilesProtocolSplit(t *testing.T) {
	fs, err := files(Spec{Enabled: true, LandingHost: "1.2.3.4", Ports: "443/tcp, 53/udp"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fs["tcp"], "no_tcp = false\nuse_udp = false") {
		t.Errorf("tcp bucket network flags wrong:\n%s", fs["tcp"])
	}
	if !strings.Contains(fs["udp"], "no_tcp = true\nuse_udp = true") {
		t.Errorf("udp bucket network flags wrong:\n%s", fs["udp"])
	}
	if _, has := fs["dual"]; has {
		t.Error("no dual bucket expected")
	}
}
