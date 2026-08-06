package main

// Rotation-time PK3 cleanup.
//
// Every server rotation is a chance for the shared homepath to grow: ET
// auto-downloads the pk3s the new server references, and nothing else ever
// removes them. Left alone the drive fills. rotate_pk3_clear + rotate_pk3_rules
// let the operator specify per-folder what to keep, applied after ET is fully
// shut down and before the next launch.
//
// SAFETY MODEL (non-negotiable):
//   - etmain is NEVER deleted from, regardless of what a rule says. The base
//     paks (pak0.pk3, pak1.pk3, pak2.pk3, mp_bin.pk3) live there and deleting
//     them bricks ET. This is enforced as a hard reject of any rule for the
//     etmain folder, PLUS a defence-in-depth guard inside applyRule.
//   - Only *.pk3 files DIRECTLY INSIDE the configured folder are candidates
//     (no recursion, no other extensions).
//   - The folder path must resolve inside rotate_pk3_homepath. Anything
//     containing ".." or a symlink that escapes the homepath is refused.
//   - Every deletion is logged with the folder, filename and size, so an
//     unwanted delete is auditable after the fact.

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// pk3RuleMode is the per-folder policy.
type pk3RuleMode int

const (
	pk3ModeWhitelist pk3RuleMode = iota // keep listed, delete everything else
	pk3ModeBlacklist                    // delete listed, keep everything else
)

func (m pk3RuleMode) String() string {
	if m == pk3ModeBlacklist {
		return "blacklist"
	}
	return "whitelist"
}

// pk3Rule is one folder's policy after parsing/validation.
type pk3Rule struct {
	Folder string
	Mode   pk3RuleMode
	List   map[string]struct{} // set of basenames the rule refers to
}

// pk3CleanReport is what applyPK3Rules returns per folder for logging and tests.
type pk3CleanReport struct {
	Folder       string
	Deleted      []string
	Kept         int
	BytesFreed   int64
	SkippedNoDir bool // folder does not exist -> no-op (not an error)
	ClearedAll   bool // whitelist mode with an empty list (deletes ALL pk3)
}

// parsePK3Rules validates the raw config rules and returns them in the internal
// form. Refuses any rule that targets etmain (or a path escape) so a bad config
// cannot survive to disk.
func parsePK3Rules(raw []PK3RuleJSON) ([]pk3Rule, error) {
	out := make([]pk3Rule, 0, len(raw))
	for i, r := range raw {
		folder := strings.TrimSpace(r.Folder)
		if folder == "" {
			return nil, fmt.Errorf("rule %d: folder is empty", i)
		}
		// no traversal, no absolute paths -- folder is a bare directory name
		// under the homepath (nq, silent, dlcache...).
		if strings.ContainsAny(folder, `/\`) || folder == "." || folder == ".." {
			return nil, fmt.Errorf("rule %d: folder %q must be a bare directory name (no path separators)", i, folder)
		}
		if strings.EqualFold(folder, "etmain") {
			// Enforced here so a config that misuses etmain is caught at load
			// time, before any deletion runs.
			return nil, fmt.Errorf("rule %d: etmain is protected and cannot be a cleanup target -- "+
				"the base paks (pak0.pk3, pak1.pk3, ...) must never be deleted", i)
		}
		var mode pk3RuleMode
		switch strings.ToLower(strings.TrimSpace(r.Mode)) {
		case "whitelist":
			mode = pk3ModeWhitelist
		case "blacklist":
			mode = pk3ModeBlacklist
		default:
			return nil, fmt.Errorf("rule %d (%s): mode must be \"whitelist\" or \"blacklist\", got %q",
				i, folder, r.Mode)
		}
		set := make(map[string]struct{}, len(r.List))
		for _, name := range r.List {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			// List entries are basenames only -- no directory component.
			if strings.ContainsAny(name, `/\`) {
				return nil, fmt.Errorf("rule %d (%s): list entry %q must be a bare filename", i, folder, name)
			}
			set[name] = struct{}{}
		}
		out = append(out, pk3Rule{Folder: folder, Mode: mode, List: set})
	}
	return out, nil
}

// isProtectedEtmain reports whether a folder is the etmain directory, matched
// case-insensitively. Defence in depth: parsePK3Rules already refuses etmain
// rules, but this second check inside applyRule means even a caller that
// bypasses the parser cannot delete a base pak.
func isProtectedEtmain(folder string) bool {
	return strings.EqualFold(folder, "etmain")
}

// resolveFolder resolves the rule's folder under the homepath and enforces
// containment: the resolved absolute path must be inside homepath, with no
// symlink escape. Returns the resolved path, or an error if the check fails.
func resolveFolder(homepath, folder string) (string, error) {
	absHome, err := filepath.Abs(homepath)
	if err != nil {
		return "", err
	}
	target := filepath.Join(absHome, folder)
	// EvalSymlinks: reject any symlink that points outside the homepath.
	real, err := filepath.EvalSymlinks(target)
	if err != nil {
		// Non-existent target is not an escape; fall back to the raw path so
		// callers can distinguish "missing" from "escape" via os.Stat.
		if os.IsNotExist(err) {
			return target, nil
		}
		return "", err
	}
	realHome, err := filepath.EvalSymlinks(absHome)
	if err != nil {
		realHome = absHome
	}
	// containment check: real must be under realHome (with a trailing sep so
	// "/data-attacker" is not accepted as a child of "/data").
	rel, err := filepath.Rel(realHome, real)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("folder %q resolves outside homepath (%q)", folder, homepath)
	}
	return real, nil
}

// applyRule executes one folder's rule and returns a report. Errors that stop
// the whole cleanup (path escape, permission denied on the readdir) surface;
// per-file delete failures are logged and counted but do not abort the folder.
func applyRule(homepath string, r pk3Rule) (pk3CleanReport, error) {
	rep := pk3CleanReport{Folder: r.Folder}
	if isProtectedEtmain(r.Folder) {
		// Should be unreachable (parsePK3Rules refuses it) but the second gate
		// exists precisely so it stays unreachable in the future.
		return rep, fmt.Errorf("refusing to delete inside etmain (protected)")
	}
	dir, err := resolveFolder(homepath, r.Folder)
	if err != nil {
		return rep, err
	}
	info, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			rep.SkippedNoDir = true
			return rep, nil
		}
		return rep, err
	}
	if !info.IsDir() {
		return rep, fmt.Errorf("target %q is not a directory", dir)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return rep, err
	}
	rep.ClearedAll = r.Mode == pk3ModeWhitelist && len(r.List) == 0

	for _, e := range entries {
		// Only regular files ending in .pk3 (case-insensitive) DIRECTLY inside
		// the folder. Sub-directories and other extensions are ignored.
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(strings.ToLower(name), ".pk3") {
			continue
		}
		_, listed := r.List[name]
		deleteIt := false
		switch r.Mode {
		case pk3ModeWhitelist:
			deleteIt = !listed // keep listed, delete rest
		case pk3ModeBlacklist:
			deleteIt = listed // delete listed, keep rest
		}
		if !deleteIt {
			rep.Kept++
			continue
		}
		path := filepath.Join(dir, name)
		// Extra guard: never cross out of the resolved folder via a per-file
		// symlink (e.g. dlcache/foo.pk3 -> ../../etmain/pak0.pk3).
		if realPath, err := filepath.EvalSymlinks(path); err == nil {
			realDir, _ := filepath.EvalSymlinks(dir)
			relPath, rerr := filepath.Rel(realDir, realPath)
			if rerr != nil || strings.Contains(relPath, "..") {
				log.Printf("pk3clean: SKIP %s/%s (symlink escape: %s)", r.Folder, name, realPath)
				continue
			}
		}
		var size int64
		if fi, err := e.Info(); err == nil {
			size = fi.Size()
		}
		if err := os.Remove(path); err != nil {
			log.Printf("pk3clean: DELETE FAIL %s/%s: %v", r.Folder, name, err)
			continue
		}
		rep.Deleted = append(rep.Deleted, name)
		rep.BytesFreed += size
	}
	return rep, nil
}

// applyPK3Rules runs cleanup across all rules and logs the outcome. Returns the
// reports (for tests) and the first fatal error (path escape / bad rule); a
// per-folder soft error is logged and skipped so one bad rule cannot block the
// rest of the sweep from freeing space.
func applyPK3Rules(homepath string, rules []pk3Rule) ([]pk3CleanReport, error) {
	if homepath == "" {
		return nil, fmt.Errorf("no homepath configured for pk3 cleanup")
	}
	if len(rules) == 0 {
		return nil, nil
	}
	log.Printf("pk3clean: sweep under %s -- %d rule(s)", homepath, len(rules))
	reports := make([]pk3CleanReport, 0, len(rules))
	for _, r := range rules {
		rep, err := applyRule(homepath, r)
		if err != nil {
			log.Printf("pk3clean: rule %q (%s) SKIPPED: %v", r.Folder, r.Mode, err)
			reports = append(reports, rep)
			continue
		}
		reports = append(reports, rep)
		logCleanReport(rep, r)
	}
	return reports, nil
}

// logCleanReport is the audit trail: per folder, count + total size freed +
// filenames. Also emits a loud warning for the empty-whitelist case, which
// intentionally clears an entire folder and should never be a silent surprise.
func logCleanReport(rep pk3CleanReport, r pk3Rule) {
	switch {
	case rep.SkippedNoDir:
		log.Printf("pk3clean: %s -- folder missing, no-op", rep.Folder)
		return
	case rep.ClearedAll && len(rep.Deleted) > 0:
		log.Printf("pk3clean: %s -- WARNING empty whitelist cleared %d pk3(s) (%s freed): %s",
			rep.Folder, len(rep.Deleted), humanBytes(rep.BytesFreed),
			strings.Join(rep.Deleted, ", "))
		return
	case len(rep.Deleted) == 0:
		log.Printf("pk3clean: %s (%s) -- kept %d, deleted 0", rep.Folder, r.Mode, rep.Kept)
		return
	}
	log.Printf("pk3clean: %s (%s) -- kept %d, deleted %d (%s freed): %s",
		rep.Folder, r.Mode, rep.Kept, len(rep.Deleted),
		humanBytes(rep.BytesFreed), strings.Join(rep.Deleted, ", "))
}

// humanBytes formats a byte count as e.g. "42 MB" or "1.2 GB".
func humanBytes(n int64) string {
	const kb, mb, gb = int64(1024), int64(1024 * 1024), int64(1024 * 1024 * 1024)
	switch {
	case n >= gb:
		return fmt.Sprintf("%.1f GB", float64(n)/float64(gb))
	case n >= mb:
		return fmt.Sprintf("%d MB", n/mb)
	case n >= kb:
		return fmt.Sprintf("%d KB", n/kb)
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// rotationPK3Cleanup is the hook deploy() calls when it is about to (re)start
// ET on a different server AND rotate_pk3_clear is enabled. It expects the
// caller to have already killed both instances of ET (so no file is open).
// Returns silently if disabled or no rules; logs and continues on soft errors
// so a broken rule never aborts the server switch itself.
func rotationPK3Cleanup() {
	if !cfg.RotatePK3Clear {
		return
	}
	homepath := cfg.RotatePK3Homepath
	if homepath == "" {
		homepath = cfg.LiveHomepath
	}
	if homepath == "" {
		log.Println("pk3clean: enabled but no homepath resolvable (set rotate_pk3_homepath or live_homepath) -- skipping")
		return
	}
	rules, err := parsePK3Rules(cfg.RotatePK3Rules)
	if err != nil {
		log.Println("pk3clean: config REJECTED, no cleanup this rotation:", err)
		return
	}
	if _, err := applyPK3Rules(homepath, rules); err != nil {
		log.Println("pk3clean: sweep failed:", err)
	}
}
