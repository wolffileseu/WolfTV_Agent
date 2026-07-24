package main

import (
	"reflect"
	"testing"
)

func TestResolutionArgs(t *testing.T) {
	cases := []struct {
		name       string
		preset     string
		existing   []string
		wantAdd    []string
		overridden bool
		unknown    bool
	}{
		{"empty preset leaves args untouched", "", nil, nil, false, false},
		{"1080p", "1080p", nil,
			[]string{"+set", "r_mode", "-1", "+set", "r_customwidth", "1920", "+set", "r_customheight", "1080"}, false, false},
		{"720p", "720p", nil,
			[]string{"+set", "r_mode", "-1", "+set", "r_customwidth", "1280", "+set", "r_customheight", "720"}, false, false},
		{"1440p", "1440p", nil,
			[]string{"+set", "r_mode", "-1", "+set", "r_customwidth", "2560", "+set", "r_customheight", "1440"}, false, false},
		{"2160p", "2160p", nil,
			[]string{"+set", "r_mode", "-1", "+set", "r_customwidth", "3840", "+set", "r_customheight", "2160"}, false, false},
		{"case-insensitive", "1080P", nil,
			[]string{"+set", "r_mode", "-1", "+set", "r_customwidth", "1920", "+set", "r_customheight", "1080"}, false, false},
		{"unknown falls back to 1080p", "4k", nil,
			[]string{"+set", "r_mode", "-1", "+set", "r_customwidth", "1920", "+set", "r_customheight", "1080"}, false, true},
		{"explicit r_customwidth in et_args wins", "2160p",
			[]string{"+set", "r_customwidth", "1234"}, nil, true, false},
		{"explicit r_customheight in et_args wins", "2160p",
			[]string{"+set", "r_customheight", "999"}, nil, true, false},
	}
	for _, c := range cases {
		add, overridden, unknown := resolutionArgs(c.preset, c.existing)
		if !reflect.DeepEqual(add, c.wantAdd) || overridden != c.overridden || unknown != c.unknown {
			t.Errorf("%s: resolutionArgs(%q,%v) = (%v, %v, %v), want (%v, %v, %v)",
				c.name, c.preset, c.existing, add, overridden, unknown, c.wantAdd, c.overridden, c.unknown)
		}
	}
}

func TestWithResolutionAppendsAndDoesNotMutate(t *testing.T) {
	base := []string{"+set", "com_maxfps", "125"}
	got := withResolution(base, "1080p")
	// input untouched
	if !reflect.DeepEqual(base, []string{"+set", "com_maxfps", "125"}) {
		t.Fatalf("withResolution mutated its input: %v", base)
	}
	// preset appended AFTER existing args (later +set wins in ET)
	want := []string{"+set", "com_maxfps", "125", "+set", "r_mode", "-1",
		"+set", "r_customwidth", "1920", "+set", "r_customheight", "1080"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("withResolution = %v, want %v", got, want)
	}
}

func TestWithResolutionOverrideKeepsEtArgs(t *testing.T) {
	base := []string{"+set", "r_mode", "-1", "+set", "r_customwidth", "1600", "+set", "r_customheight", "900"}
	got := withResolution(base, "2160p") // et_args custom res must win
	if !reflect.DeepEqual(got, base) {
		t.Errorf("explicit r_custom* must win; got %v", got)
	}
}
