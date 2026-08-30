package warrant

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"agentbob/contract"
)

// PlaceAttachments relocates this event's downloaded attachments from their staged
// (scope-agnostic) path into the resolved scope's SPACE, so the user's files live in
// the same space the fs / ocr / vision tools read and persist past the turn (the
// historical-file-reachable goal). It runs at session submit — ONCE per inbound,
// before any agora member fan-out — so a staged file shared across members is moved
// exactly once.
//
// Each downloaded attachment is moved to <space>/inbox/<base>, its Path rewritten
// space-relative, and LocalPath cleared: the raw-absolute-path bypass is retired, so
// every reader goes through the FileChannel. A per-file move failure leaves that
// attachment on its staged path (best-effort — a lost file beats a dropped turn).
//
// home is $BOB_HOME; scope is the resolved session scope (the space name). Pure: no
// started Manager needed (wired as a closure in cmd/bob), and the destination is
// computed with SpaceLocalDir so it matches the FileChannel that serves the space.
func PlaceAttachments(home, scope string, atts []contract.Attachment) []contract.Attachment {
	if len(atts) == 0 {
		return atts
	}
	dir, err := SpaceLocalDir(home, scope)
	if err != nil {
		// F55: nothing can be placed — clear the staged (space-dead) Path on every
		// attachment so a dead handle never reaches the prompt / history (see clearPath).
		slog.Warn("warrant: attachment placement skipped (bad space)", "scope", scope, "err", err)
		return clearUnplaced(atts)
	}
	inbox := filepath.Join(dir, contract.InboxSubdir)
	made := false
	for i := range atts {
		a := &atts[i]
		if a.LocalPath == "" { // not downloaded (over size limit / capture disabled)
			a.Path = "" // F55: no on-disk file → no space-relative path; degrade to "not downloaded"
			continue
		}
		if !made {
			if err := os.MkdirAll(inbox, 0o700); err != nil {
				slog.Warn("warrant: attachment placement mkdir failed (keeping staged paths)", "scope", scope, "err", err)
				return clearUnplaced(atts) // nothing placed yet (mkdir gates the first placement)
			}
			made = true
		}
		// Name the inbox file from the attachment's CLEAN display name, not the staged
		// "<nanos>-<hex>-<name>" base. The prompt shows this name (describeAttachments)
		// and the model passes it straight back to a tool (ch.Read), so display ==
		// on-disk: no per-tool name→path resolver, no leaked staging prefix. Falls back
		// to the unique staged base when there is no usable FileName (e.g. a nameless
		// sticker), keeping that file collision-proof.
		base := inboxBaseName(a.FileName)
		reserved := false
		if base == "" {
			// No usable display name → keep the unique staged base; it is already
			// collision-proof ("<nanos>-<hex>-<name>"), so no reservation needed.
			base = filepath.Base(a.LocalPath)
		} else {
			base = reserveInboxName(inbox, base)
			reserved = true
		}
		dst := filepath.Join(inbox, base)
		if err := moveFile(a.LocalPath, dst); err != nil {
			slog.Warn("warrant: attachment placement move failed", "scope", scope, "file", base, "err", err)
			if reserved {
				_ = os.Remove(dst) // drop the 0-byte reservation placeholder we created
			}
			a.Path = "" // F55: the staged Path is space-dead → clear it (degrade to "not downloaded")
			continue
		}
		// Space-relative path uses forward slashes (the FileChannel's path convention),
		// regardless of host separator.
		a.Path = contract.InboxSubdir + "/" + base
		a.LocalPath = ""
	}
	return atts
}

// clearUnplaced blanks the Path on every attachment that was NOT successfully placed
// into the space (a placed one has a non-empty Path AND a cleared LocalPath). On a
// whole-batch placement abort (bad space / mkdir fail) nothing was placed, so this
// drops the staged, space-dead Path so no unreadable handle reaches the prompt or
// history (F55). A truly-placed attachment (LocalPath already "") is left untouched.
func clearUnplaced(atts []contract.Attachment) []contract.Attachment {
	for i := range atts {
		if atts[i].LocalPath != "" { // never placed → its Path is the space-dead staged one
			atts[i].Path = ""
		}
	}
	return atts
}

// unsafeInboxChar matches any run of characters not allowed in a clean inbox base
// name; they collapse to a single "_". Mirrors the staging sanitiser (files.safeName)
// so the on-disk name stays escape-proof — no path separators, no ".." traversal, no
// control chars — even though it now derives from the user-controlled FileName.
var unsafeInboxChar = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

// inboxExt matches a usable filename extension — "." followed by 1-8 alphanumerics.
// Anything else (multi-dot tails, non-ASCII, separators) is treated as part of the
// stem and sanitized with it, so the extension preserved below can never smuggle an
// unsafe byte onto disk.
var inboxExt = regexp.MustCompile(`^\.[a-zA-Z0-9]{1,8}$`)

// inboxBaseName turns an attachment's display FileName into a safe inbox base. The
// model is SHOWN this name and passes it back to a tool, so it must be both clean
// (display == on-disk) and unable to escape the inbox. Returns "" when FileName has
// no usable characters at all — the caller then keeps the unique staged base.
//
// The extension is split off FIRST and survives both sanitizing and truncating: a
// stem with no safe characters (e.g. an all-CJK name like "发票.jpg") falls back to
// "file"+ext instead of collapsing to a bare extension-less token, which would break
// mime-by-extension on later turns (an image misclassified as a document that ocr /
// vision can never find again).
func inboxBaseName(fileName string) string {
	n := filepath.Base(strings.TrimSpace(fileName))
	ext := filepath.Ext(n)
	if !inboxExt.MatchString(ext) {
		ext = "" // unusable extension — sanitize it along with the stem
	}
	stem := strings.TrimSuffix(n, ext)
	stem = unsafeInboxChar.ReplaceAllString(stem, "_")
	stem = strings.Trim(stem, "._-")
	if stem == "" {
		if ext == "" {
			return ""
		}
		stem = "file" // no safe stem characters — keep the extension (dedup handles clashes)
	}
	const maxLen = 80
	if keep := maxLen - len(ext); len(stem) > keep {
		stem = stem[:keep] // stem is ASCII-only after sanitizing, so a byte cut can't split a rune
	}
	return stem + ext
}

// reserveInboxName atomically claims a free inbox name for base and returns it: each
// candidate is created with O_CREATE|O_EXCL (a 0-byte placeholder that moveFile then
// replaces), so the claim is race-free — two concurrent same-scope placements of the
// same display name get DIFFERENT names instead of both winning an os.Stat and
// overwriting each other (placement runs before the per-scope turn lock, so the race
// is real). On a same-name clash it tries base, then "<stem>-2", "-3", … . A non-EEXIST
// error (e.g. a vanished inbox dir) bails to base so moveFile surfaces the real error;
// exhausting maxInboxDedup same-stem files also returns base (the move overwrites — a
// pathological inbox, bounded and rare).
func reserveInboxName(inbox, base string) string {
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	const maxInboxDedup = 1000
	for i := 1; i < maxInboxDedup; i++ {
		name := base
		if i > 1 {
			name = fmt.Sprintf("%s-%d%s", stem, i, ext)
		}
		f, err := os.OpenFile(filepath.Join(inbox, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_ = f.Close()
			return name
		}
		if !os.IsExist(err) {
			return base // real error (not "already exists") — let moveFile report it
		}
	}
	return base
}

// moveFile relocates src to dst, falling back to copy-then-remove when os.Rename
// fails across filesystems (EXDEV — e.g. a separately-mounted sandbox vs spaces). The
// media stack reads exclusively through the space now, so a failed move would leave the
// attachment unreadable; the copy fallback keeps it working. dst's parent must exist.
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	// in is closed explicitly on every path (no defer) so the close below precedes
	// os.Remove(src) without a double-close.
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		_ = in.Close()
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		_ = in.Close()
		os.Remove(dst)
		return err
	}
	if err := out.Close(); err != nil {
		_ = in.Close()
		os.Remove(dst)
		return err
	}
	in.Close()
	return os.Remove(src)
}
