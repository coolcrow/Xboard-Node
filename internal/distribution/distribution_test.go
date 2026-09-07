package distribution

import "testing"

func TestArtifactURLDefault(t *testing.T) {
	SetBase("")
	overrideBase = ""
	got := ArtifactURL("xboard-node-linux-amd64", "")
	want := "https://github.com/coolcrow/Xboard-Node/releases/latest/download/xboard-node-linux-amd64"
	if got != want {
		t.Fatalf("latest: got %s want %s", got, want)
	}
	got = ArtifactURL("xbctl-linux-arm64", "v1.0.1-fork1")
	want = "https://github.com/coolcrow/Xboard-Node/releases/download/v1.0.1-fork1/xbctl-linux-arm64"
	if got != want {
		t.Fatalf("pinned: got %s want %s", got, want)
	}
}

func TestArtifactURLOverride(t *testing.T) {
	SetBase("http://127.0.0.1:8123/mirror")
	defer SetBase("")
	got := ArtifactURL("xboard-node-linux-amd64", "v2")
	want := "http://127.0.0.1:8123/mirror/download/v2/xboard-node-linux-amd64"
	if got != want {
		t.Fatalf("override: got %s want %s", got, want)
	}
}
