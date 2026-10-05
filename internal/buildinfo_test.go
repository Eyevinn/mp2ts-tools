package internal

import (
	"runtime/debug"
	"testing"
)

func TestVersionString(t *testing.T) {
	built := func(version, vcsTime string) *debug.BuildInfo {
		info := &debug.BuildInfo{Main: debug.Module{Path: "github.com/Eyevinn/mp2ts-tools", Version: version}}
		if vcsTime != "" {
			info.Settings = []debug.BuildSetting{
				{Key: "vcs.revision", Value: "a997d3d1c2e34f5a6b7c8d9e0f1a2b3c4d5e6f70"},
				{Key: "vcs.time", Value: vcsTime},
			}
		}
		return info
	}
	cases := []struct {
		desc string
		info *debug.BuildInfo
		want string
	}{
		{"tag build", built("v0.4.0", "2026-10-05T12:31:39Z"), "v0.4.0, date: 2026-10-05"},
		{"build after the tag, with local changes",
			built("v0.3.1-0.20261005120000-a997d3d1c2e3+dirty", "2026-10-05T12:00:00Z"),
			"v0.3.1-0.20261005120000-a997d3d1c2e3+dirty, date: 2026-10-05"},
		{"go install of a release has no vcs time", built("v0.4.0", ""), "v0.4.0"},
		{"the date is in UTC", built("v0.4.0", "2026-10-05T23:30:00-02:00"), "v0.4.0, date: 2026-10-06"},
		{"workspace build", built("(devel)", ""), "(devel)"},
		{"build of files, not a package", built("", ""), "(devel)"},
		{"no build information", nil, "(devel)"},
	}
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			if got := versionString(c.info); got != c.want {
				t.Errorf("versionString() = %q, want %q", got, c.want)
			}
		})
	}
}

// TestVersion checks that a test binary reports what the toolchain embedded
// in it, without asserting a value: that depends on how the test was built.
func TestVersion(t *testing.T) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		t.Fatal("no build information in the test binary")
	}
	if got, want := Version(), versionString(info); got != want {
		t.Errorf("Version() = %q, want %q", got, want)
	}
}
