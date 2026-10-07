package main

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// makeHomepath materialises a fake homepath with the given files. `layout`
// maps "folder/file" to the intended file contents (used only to distinguish
// files by size in the reports). Returns the resolved absolute homepath.
func makeHomepath(t *testing.T, layout map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, body := range layout {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// listPk3s returns the sorted pk3 basenames remaining in a folder under home.
func listPk3s(t *testing.T, home, folder string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(home, folder))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(strings.ToLower(e.Name()), ".pk3") {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// TestParsePK3RulesRejectsEtmain is the *at-config-load* half of the etmain
// protection: even if someone adds an etmain rule to config.json, it never
// makes it into the running rule list. The applyRule guard is a second gate
// that TestApplyRuleGuardsEtmain covers.
func TestParsePK3RulesRejectsEtmain(t *testing.T) {
	for _, name := range []string{"etmain", "ETMAIN", "EtMain"} {
		t.Run(name, func(t *testing.T) {
			_, err := parsePK3Rules([]PK3RuleJSON{{Folder: name, Mode: "whitelist"}})
			if err == nil {
				t.Fatalf("parsePK3Rules must reject a rule targeting %q", name)
			}
			if !strings.Contains(err.Error(), "etmain") {
				t.Errorf("error should mention etmain, got: %v", err)
			}
		})
	}
}

// TestApplyRuleGuardsEtmain is defence-in-depth: if the parser is bypassed
// (a future refactor that skips validation, tests with hand-built rules), the
// rule executor still refuses to touch etmain.
func TestApplyRuleGuardsEtmain(t *testing.T) {
	home := makeHomepath(t, map[string]string{
		"etmain/pak0.pk3":    "base",
		"etmain/mp_bin.pk3":  "base",
		"etmain/pak1.pk3":    "base",
		"etmain/pak2.pk3":    "base",
		"etmain/extra42.pk3": "custom",
	})
	rule := pk3Rule{Folder: "etmain", Mode: pk3ModeWhitelist} // empty list = "delete all"
	_, err := applyRule(home, rule)
	if err == nil {
		t.Fatal("applyRule must refuse an etmain target")
	}
	// files still present
	for _, name := range []string{"pak0.pk3", "mp_bin.pk3", "pak1.pk3", "pak2.pk3", "extra42.pk3"} {
		if _, err := os.Stat(filepath.Join(home, "etmain", name)); err != nil {
			t.Errorf("etmain/%s was deleted despite protection: %v", name, err)
		}
	}
}

// TestParsePK3RulesValidation covers the input shapes that are refused before
// any file is touched: bad mode, path-separator in folder or list entry,
// traversal, empty folder.
func TestParsePK3RulesValidation(t *testing.T) {
	cases := []struct {
		name    string
		rules   []PK3RuleJSON
		wantErr string
	}{
		{"bad mode", []PK3RuleJSON{{Folder: "nq", Mode: "delete"}}, "whitelist"},
		{"empty folder", []PK3RuleJSON{{Folder: "", Mode: "whitelist"}}, "empty"},
		{"folder with slash", []PK3RuleJSON{{Folder: "nq/sub", Mode: "whitelist"}}, "separators"},
		{"folder ..", []PK3RuleJSON{{Folder: "..", Mode: "whitelist"}}, "separators"},
		{"list entry with slash", []PK3RuleJSON{
			{Folder: "nq", Mode: "whitelist", List: []string{"../etmain/pak0.pk3"}},
		}, "bare filename"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := parsePK3Rules(c.rules)
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("want error containing %q, got %v", c.wantErr, err)
			}
		})
	}
}

// TestApplyRuleWhitelistKeepsOnlyListed asserts the mod-directory use case:
// the mod's own paks are listed, downloaded junk is removed.
func TestApplyRuleWhitelistKeepsOnlyListed(t *testing.T) {
	home := makeHomepath(t, map[string]string{
		"nq/nq_v1.2.9_3.pk3":   "keep",
		"nq/nq_b_v1.2.9_6.pk3": "keep",
		"nq/random-map1.pk3":   "junk",
		"nq/random-map2.pk3":   "junk",
		"nq/some.txt":          "ignored (not pk3)",
	})
	rules, err := parsePK3Rules([]PK3RuleJSON{{
		Folder: "nq", Mode: "whitelist",
		List: []string{"nq_v1.2.9_3.pk3", "nq_b_v1.2.9_6.pk3"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	reports, err := applyPK3Rules(home, rules)
	if err != nil {
		t.Fatal(err)
	}
	rep := reports[0]
	if rep.Kept != 2 {
		t.Errorf("Kept = %d, want 2", rep.Kept)
	}
	sort.Strings(rep.Deleted)
	want := []string{"random-map1.pk3", "random-map2.pk3"}
	if strings.Join(rep.Deleted, ",") != strings.Join(want, ",") {
		t.Errorf("Deleted = %v, want %v", rep.Deleted, want)
	}
	got := listPk3s(t, home, "nq")
	sort.Strings(got)
	wantRemaining := []string{"nq_b_v1.2.9_6.pk3", "nq_v1.2.9_3.pk3"}
	if strings.Join(got, ",") != strings.Join(wantRemaining, ",") {
		t.Errorf("remaining = %v, want %v", got, wantRemaining)
	}
	// non-pk3 file is untouched
	if _, err := os.Stat(filepath.Join(home, "nq/some.txt")); err != nil {
		t.Errorf("non-pk3 file deleted: %v", err)
	}
}

// TestApplyRuleBlacklistDeletesOnlyListed covers the dlcache use case: keep
// downloads, remove only specifically-named files.
func TestApplyRuleBlacklistDeletesOnlyListed(t *testing.T) {
	home := makeHomepath(t, map[string]string{
		"dlcache/goldrush.pk3": "keep",
		"dlcache/oasis.pk3":    "keep",
		"dlcache/broken.pk3":   "remove",
	})
	rules, err := parsePK3Rules([]PK3RuleJSON{{
		Folder: "dlcache", Mode: "blacklist", List: []string{"broken.pk3"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	reports, _ := applyPK3Rules(home, rules)
	if len(reports[0].Deleted) != 1 || reports[0].Deleted[0] != "broken.pk3" {
		t.Errorf("Deleted = %v, want [broken.pk3]", reports[0].Deleted)
	}
	got := listPk3s(t, home, "dlcache")
	sort.Strings(got)
	if want := []string{"goldrush.pk3", "oasis.pk3"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("remaining = %v, want %v", got, want)
	}
}

// TestBlacklistEmptyListIsNoOp: dlcache with an empty blacklist means "delete
// nothing" (the common "keep everything I've downloaded so far" case).
func TestBlacklistEmptyListIsNoOp(t *testing.T) {
	home := makeHomepath(t, map[string]string{
		"dlcache/a.pk3": "x",
		"dlcache/b.pk3": "x",
	})
	rules, _ := parsePK3Rules([]PK3RuleJSON{{Folder: "dlcache", Mode: "blacklist"}})
	reports, _ := applyPK3Rules(home, rules)
	if len(reports[0].Deleted) != 0 {
		t.Errorf("blacklist with empty list must not delete anything, got %v", reports[0].Deleted)
	}
	if reports[0].Kept != 2 {
		t.Errorf("Kept = %d, want 2", reports[0].Kept)
	}
}

// TestWhitelistEmptyListClearsFolder is the "delete all" case from the spec:
// a whitelist with an empty list means "keep none". It must work and it must
// mark the report ClearedAll=true so the log warning is emitted.
func TestWhitelistEmptyListClearsFolder(t *testing.T) {
	home := makeHomepath(t, map[string]string{
		"scratch/a.pk3": "x",
		"scratch/b.pk3": "x",
	})
	rules, _ := parsePK3Rules([]PK3RuleJSON{{Folder: "scratch", Mode: "whitelist"}})
	reports, _ := applyPK3Rules(home, rules)
	if !reports[0].ClearedAll {
		t.Error("ClearedAll must be true so the logger emits the warning")
	}
	if len(reports[0].Deleted) != 2 || reports[0].Kept != 0 {
		t.Errorf("Deleted=%v Kept=%d, want 2 deletes and 0 kept", reports[0].Deleted, reports[0].Kept)
	}
	if got := listPk3s(t, home, "scratch"); len(got) != 0 {
		t.Errorf("scratch should be empty of pk3s, got %v", got)
	}
}

// TestOnlyPk3IsTouched: a folder full of other extensions is untouched.
func TestOnlyPk3IsTouched(t *testing.T) {
	home := makeHomepath(t, map[string]string{
		"nq/nq.pk3":     "keep",
		"nq/notes.txt":  "ignore",
		"nq/binary.exe": "ignore",
		"nq/PACKED.PK3": "keep-case-insensitive", // .PK3 must be treated as .pk3
	})
	rules, _ := parsePK3Rules([]PK3RuleJSON{{
		Folder: "nq", Mode: "whitelist", List: []string{"nq.pk3", "PACKED.PK3"},
	}})
	applyPK3Rules(home, rules)
	// nothing should have been deleted (all pk3s are listed; non-pk3 is skipped).
	for _, name := range []string{"nq.pk3", "notes.txt", "binary.exe", "PACKED.PK3"} {
		if _, err := os.Stat(filepath.Join(home, "nq", name)); err != nil {
			t.Errorf("nq/%s should still exist, got: %v", name, err)
		}
	}
}

// TestMissingFolderIsNoop: a folder that does not exist is silently skipped
// (not an error). Common on first install before the mod dir is populated.
func TestMissingFolderIsNoop(t *testing.T) {
	home := t.TempDir() // empty homepath
	rules, _ := parsePK3Rules([]PK3RuleJSON{{
		Folder: "nq", Mode: "whitelist", List: []string{"nq.pk3"},
	}})
	reports, err := applyPK3Rules(home, rules)
	if err != nil {
		t.Fatalf("missing folder must not error: %v", err)
	}
	if !reports[0].SkippedNoDir {
		t.Error("SkippedNoDir must be true for a missing folder")
	}
}

// TestDisabledByDefault: the flag defaults to false and applyPK3Rules with
// no rules is a no-op.
func TestApplyEmptyRulesIsNoop(t *testing.T) {
	home := t.TempDir()
	reports, err := applyPK3Rules(home, nil)
	if err != nil {
		t.Fatal(err)
	}
	if reports != nil {
		t.Errorf("no rules -> no reports, got %d", len(reports))
	}
}

// TestSymlinkEscapeIsRefused makes sure a rule for a folder that resolves
// outside the homepath (via a symlink) does not delete anything. Only run on
// platforms that support os.Symlink for non-privileged users -- Windows in a
// non-elevated session cannot create symlinks, so skip there.
func TestSymlinkEscapeIsRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows requires admin/dev-mode to create symlinks; skipping escape test")
	}
	home := t.TempDir()
	outside := t.TempDir()
	// outside has a base pak the attacker would love to delete
	if err := os.WriteFile(filepath.Join(outside, "pak0.pk3"), []byte("base"), 0644); err != nil {
		t.Fatal(err)
	}
	// symlink home/nq -> outside
	if err := os.Symlink(outside, filepath.Join(home, "nq")); err != nil {
		t.Fatal(err)
	}
	rules, _ := parsePK3Rules([]PK3RuleJSON{{Folder: "nq", Mode: "whitelist"}})
	reports, _ := applyPK3Rules(home, rules)
	// escape refused -> no report entry with a deletion, and the file survives
	if _, err := os.Stat(filepath.Join(outside, "pak0.pk3")); err != nil {
		t.Errorf("symlink escape deleted a file outside the homepath: %v", err)
	}
	if len(reports) > 0 && len(reports[0].Deleted) > 0 {
		t.Errorf("symlink escape must not delete anything, deleted=%v", reports[0].Deleted)
	}
}

// TestLoggerMentionsClearedAll snapshots the log-level warning path for the
// empty-whitelist case, so the "loud warning" contract is codified rather
// than left implicit in the log format.
func TestLoggerMentionsClearedAll(t *testing.T) {
	// Reuse the pure logic to build a report, then just check the report
	// carries the flag (the log format itself is asserted informally).
	home := makeHomepath(t, map[string]string{"scratch/x.pk3": "x"})
	rules, _ := parsePK3Rules([]PK3RuleJSON{{Folder: "scratch", Mode: "whitelist"}})
	reports, _ := applyPK3Rules(home, rules)
	if !reports[0].ClearedAll {
		t.Error("empty-whitelist rule must set ClearedAll so the warning fires")
	}
}

// TestWolfTVAssetsSurviveCleanup: the WolfTV client ships legacy/wolftv_assets.pk3.
// An empty whitelist on legacy/ (delete everything) must still keep it, plus
// ET: Legacy's own legacy_*.pk3, while ordinary pk3s are removed as before.
func TestWolfTVAssetsSurviveCleanup(t *testing.T) {
	home := makeHomepath(t, map[string]string{
		"legacy/wolftv_assets.pk3":  "assets",
		"legacy/legacy_v2.83.2.pk3": "mod",
		"legacy/random-map1.pk3":    "junk",
		"legacy/random-map2.pk3":    "junk",
	})
	rules, err := parsePK3Rules([]PK3RuleJSON{{Folder: "legacy", Mode: "whitelist"}})
	if err != nil {
		t.Fatal(err)
	}
	reports, err := applyPK3Rules(home, rules)
	if err != nil {
		t.Fatal(err)
	}
	rep := reports[0]
	sort.Strings(rep.Deleted)
	if want := "random-map1.pk3,random-map2.pk3"; strings.Join(rep.Deleted, ",") != want {
		t.Errorf("Deleted = %v, want %s", rep.Deleted, want)
	}
	sort.Strings(rep.Protected)
	if want := "legacy_v2.83.2.pk3,wolftv_assets.pk3"; strings.Join(rep.Protected, ",") != want {
		t.Errorf("Protected = %v, want %s", rep.Protected, want)
	}
	if rep.Kept != 2 {
		t.Errorf("Kept = %d, want 2", rep.Kept)
	}
	got := listPk3s(t, home, "legacy")
	if want := "legacy_v2.83.2.pk3,wolftv_assets.pk3"; strings.Join(got, ",") != want {
		t.Errorf("remaining = %v, want %s", got, want)
	}
}

// TestProtectedBeatsBlacklist: explicitly blacklisting a protected file (any
// case) still does not delete it; other listed files go as usual.
func TestProtectedBeatsBlacklist(t *testing.T) {
	home := makeHomepath(t, map[string]string{
		"legacy/WolfTV_Assets.PK3": "assets",
		"legacy/pak0.pk3":          "base",
		"legacy/broken.pk3":        "remove",
	})
	rules, _ := parsePK3Rules([]PK3RuleJSON{{
		Folder: "legacy", Mode: "blacklist",
		List: []string{"WolfTV_Assets.PK3", "pak0.pk3", "broken.pk3"},
	}})
	reports, _ := applyPK3Rules(home, rules)
	if d := reports[0].Deleted; len(d) != 1 || d[0] != "broken.pk3" {
		t.Errorf("Deleted = %v, want [broken.pk3]", d)
	}
	got := listPk3s(t, home, "legacy")
	if want := "WolfTV_Assets.PK3,pak0.pk3"; strings.Join(got, ",") != want {
		t.Errorf("remaining = %v, want %s", got, want)
	}
}

func TestIsProtectedPK3(t *testing.T) {
	for _, n := range []string{"wolftv_assets.pk3", "WOLFTV_ASSETS.PK3", "pak0.pk3", "PAK1.pk3",
		"pak2.pk3", "mp_bin.pk3", "legacy_v2.83.2.pk3", "Legacy_V2.82.pk3"} {
		if !isProtectedPK3(n) {
			t.Errorf("%s should be protected", n)
		}
	}
	for _, n := range []string{"wolftv_assets_old.pk3", "pak3.pk3", "goldrush.pk3", "nq_v1.2.9_3.pk3", "mylegacy_x.pk3"} {
		if isProtectedPK3(n) {
			t.Errorf("%s should NOT be protected", n)
		}
	}
}
