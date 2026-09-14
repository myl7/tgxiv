// Package split moves dialogs between two archive roots — the engine of the
// `split` command. For each listed dialog it moves the media/<dialog_id>/
// folder across roots FIRST, then one cross-database transaction copies the
// dialog's dialogs/messages/tasks rows into the destination root's database
// and deletes them from the source. That ordering makes an interrupted run
// resumable by re-running the same command: a crash after the media move but
// before the transaction leaves the files at the destination and the rows at
// the source, and the next run recognizes each half and finishes the other.
// The destination root is prepared through store.Open, which applies the v3
// schema when absent and refuses an old v2-layout file with its own
// actionable error; the source is opened as a raw connection so the copy and
// the delete can share one attached-database transaction, which the store API
// cannot express.
package split

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/myl7/tgxiv/internal/store"
)

// dbName and mediaDirName mirror internal/archive's layout constants — the
// database file and media tree every archive root carries. Importing archive
// for two literals would drag the tdl-running package into this offline one,
// so they are mirrored instead (deliberately not importing the tdl-running
// way where it must avoid archive.Open's side effects).
const (
	dbName       = "tgxiv.sqlite"
	mediaDirName = "media"
)

// Run moves the listed dialogs from the archive root src into dst. dst is
// created (and given the v3 schema) when absent; src must be an existing v3
// root. An interrupted run is resumable: call Run again with the same ids and
// the already-moved dialogs are detected and skipped.
func Run(src, dst string, ids []int64) error {
	if len(ids) == 0 {
		return fmt.Errorf("no dialog ids to split")
	}
	src, dst = absPath(src), absPath(dst)
	if src == dst {
		return fmt.Errorf("--to must be a different archive root than the source root given by -d")
	}

	// the source is validated before anything is created at the destination,
	// so a mistyped -d cannot litter the disk with a fresh destination root
	srcDB := filepath.Join(src, dbName)
	if _, err := os.Stat(srcDB); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%s does not exist; point -d at the archive root carrying tgxiv.sqlite", srcDB)
		}
		return fmt.Errorf("stat %s: %w", srcDB, err)
	}

	// destination preparation goes through store.Open: it applies the v3
	// schema when the root is absent or its database empty, and refuses a
	// v2-layout file with its own actionable error — no schema is duplicated
	// here, and no ATTACH-qualified CREATE TABLE syntax traps either
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}
	dstDB := filepath.Join(dst, dbName)
	st, err := store.Open(dstDB)
	if err != nil {
		return err
	}
	if err := st.Close(); err != nil {
		return fmt.Errorf("close %s: %w", dstDB, err)
	}

	db, err := openSrcAttached(srcDB, dstDB)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	return moveDialogs(db, src, dst, ids)
}

// ParseIDs parses the split command's positional dialog-id arguments. Each
// argument may itself be a comma-separated list, so `111 222` and
// `"111,222"` parse to the same id set — ids are bare numbers that never
// contain commas or spaces, so quoting never matters. Ids must be positive
// integers; duplicates collapse in first-seen order.
func ParseIDs(args []string) ([]int64, error) {
	var ids []int64
	seen := map[int64]bool{}
	for _, arg := range args {
		for _, part := range strings.Split(arg, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			id, err := strconv.ParseInt(part, 10, 64)
			if err != nil || id <= 0 {
				return nil, fmt.Errorf("bad dialog id %q (ids are bare numeric Telegram dialog ids, e.g. 111 or 111,222)", part)
			}
			if seen[id] {
				continue
			}
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("no dialog ids given (pass them as arguments, e.g. `tgxiv split --to DIR 111 222`)")
	}
	return ids, nil
}

// openSrcAttached opens the source with the per-connection pragmas riding in
// the DSN (see dsn) and attaches the prepared destination file as "dstx". The
// pool is pinned to a single connection: ATTACH is per-connection state, and
// with database/sql pooling a second connection would see no dstx schema —
// the same trap that forces the pragmas into the DSN. One connection also
// serializes the whole move, which is exactly what a maintenance command
// wants.
func openSrcAttached(srcDB, dstDB string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", dsn(srcDB))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open %s: %w", srcDB, err)
	}
	if _, err := db.Exec("ATTACH DATABASE ? AS dstx", dstDB); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("attach %s: %w", dstDB, err)
	}

	// a source without a dialogs table is not v3: a legacy channel archive
	// must be converted into a dialog of a real root first
	var one int
	err = db.QueryRow(`SELECT 1 FROM sqlite_master WHERE type='table' AND name='dialogs'`).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		_ = db.Close()
		return nil, fmt.Errorf("%s has no dialogs table; not a v3 archive database", srcDB)
	}
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("inspect %s: %w", srcDB, err)
	}
	return db, nil
}

// moveDialogs classifies each id against both databases, moves media, then
// runs the single cross-database transaction. The classification is what
// makes an interrupted run resumable by re-running the same command: ids
// already split (in dst, gone from src) are skipped with a note, while the
// in-both and in-neither states can never arise from this command's own
// atomic transaction and so always mean manual intervention (in-both) or a
// typo (in-neither).
func moveDialogs(db *sql.DB, src, dst string, ids []int64) error {
	srcMissing, err := missingDialogs(db, "main", ids)
	if err != nil {
		return err
	}
	dstMissing, err := missingDialogs(db, "dstx", ids)
	if err != nil {
		return err
	}
	inSrc := make(map[int64]bool, len(ids))
	inDst := make(map[int64]bool, len(ids))
	for _, id := range ids {
		inSrc[id], inDst[id] = true, true
	}
	for _, id := range srcMissing {
		delete(inSrc, id)
	}
	for _, id := range dstMissing {
		delete(inDst, id)
	}

	var move, done, neither, both []int64
	for _, id := range ids {
		switch {
		case inSrc[id] && inDst[id]:
			both = append(both, id)
		case !inSrc[id] && !inDst[id]:
			neither = append(neither, id)
		case !inSrc[id] && inDst[id]:
			done = append(done, id) // already split by an earlier run
		default:
			move = append(move, id)
		}
	}
	if len(both) > 0 {
		return fmt.Errorf("in BOTH databases (split never leaves that state — resolve manually): %v", collidingIDs(both))
	}
	if len(neither) > 0 {
		return fmt.Errorf("in neither database (typo?): %v", neither)
	}
	for _, id := range done {
		fmt.Printf("[split] dialog %d already split earlier; skipping\n", id)
	}
	if len(move) == 0 {
		return printRemainder(db)
	}
	ids = move

	// media moves first: a crash afterwards is resumable (rows still in src,
	// folders already at dst), while the reverse order could strand media
	for _, id := range ids {
		if err := moveMediaDir(
			filepath.Join(src, mediaDirName, strconv.FormatInt(id, 10)),
			filepath.Join(dst, mediaDirName, strconv.FormatInt(id, 10)),
		); err != nil {
			return fmt.Errorf("media for dialog %d: %w", id, err)
		}
	}

	// one transaction spans both attached databases: the copy into dstx and
	// the deletes from main commit together or not at all
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if tx != nil {
			_ = tx.Rollback()
		}
	}()

	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	where := "dialog_id IN (" + placeholders(len(ids)) + ")"

	nDlg, err := execTx(tx, "INSERT INTO dstx.dialogs (dialog_id, username, title, kind, last_msg_id, updated_at) "+
		"SELECT dialog_id, username, title, kind, last_msg_id, updated_at FROM main.dialogs WHERE "+where, args)
	if err != nil {
		return fmt.Errorf("copy dialogs: %w", err)
	}
	nMsg, err := execTx(tx, "INSERT INTO dstx.messages (dialog_id, msg_id, type, date, text, file, raw) "+
		"SELECT dialog_id, msg_id, type, date, text, file, raw FROM main.messages WHERE "+where, args)
	if err != nil {
		return fmt.Errorf("copy messages: %w", err)
	}
	nTask, err := execTx(tx, "INSERT INTO dstx.tasks (dialog_id, msg_id, file_name, size, media_type, status, attempts, actual_size, path, error, updated_at) "+
		"SELECT dialog_id, msg_id, file_name, size, media_type, status, attempts, actual_size, path, error, updated_at FROM main.tasks WHERE "+where, args)
	if err != nil {
		return fmt.Errorf("copy tasks: %w", err)
	}
	// deletes run child-table-first; task paths are root-relative and name the
	// same media/<id>/ layout in either root, so no rewriting is needed
	if _, err = execTx(tx, "DELETE FROM main.tasks WHERE "+where, args); err != nil {
		return fmt.Errorf("delete tasks: %w", err)
	}
	if _, err = execTx(tx, "DELETE FROM main.messages WHERE "+where, args); err != nil {
		return fmt.Errorf("delete messages: %w", err)
	}
	if _, err = execTx(tx, "DELETE FROM main.dialogs WHERE "+where, args); err != nil {
		return fmt.Errorf("delete dialogs: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	tx = nil

	fmt.Printf("[split] moved dialogs %v: %d dialog(s), %d message(s), %d task(s)\n", ids, nDlg, nMsg, nTask)
	return printRemainder(db)
}

// missingDialogs returns which of ids have no row in the named database.
func missingDialogs(db *sql.DB, schema string, ids []int64) ([]int64, error) {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := db.Query("SELECT dialog_id FROM "+schema+".dialogs WHERE dialog_id IN ("+
		placeholders(len(ids))+")", args...)
	if err != nil {
		return nil, err
	}
	has := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		has[id] = true
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()

	var missing []int64
	for _, id := range ids {
		if !has[id] {
			missing = append(missing, id)
		}
	}
	return missing, nil
}

// collidingIDs sorts ascending for stable error messages (map and input
// order vary; error text should be reproducible).
func collidingIDs(ids []int64) []int64 {
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// placeholders builds an "?,?,…" list of n marks for IN clauses.
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func execTx(tx *sql.Tx, query string, args []any) (int64, error) {
	res, err := tx.Exec(query, args...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// moveMediaDir moves one dialog's media folder (dialog.txt included) between
// roots. Rename first; a cross-device rename falls back to a recursive copy
// verified by size, deleting the source tree only after every file verified.
// A source folder that is already gone with the destination present counts as
// "moved by an earlier interrupted run" and is skipped, which is what makes
// the whole command resumable.
func moveMediaDir(srcDir, dstDir string) error {
	_, srcErr := os.Stat(srcDir)
	_, dstErr := os.Stat(dstDir)
	srcOK, dstOK := srcErr == nil, dstErr == nil
	switch {
	case srcOK && dstOK:
		return fmt.Errorf("both %s and %s exist; resolve manually (a crashed copy?) and re-run", srcDir, dstDir)
	case !srcOK && dstOK:
		fmt.Printf("[split] media already at %s\n", dstDir)
		return nil
	case !srcOK && !dstOK:
		fmt.Printf("[split] no media folder for this dialog (none ever downloaded)\n")
		return nil
	}

	// the parent must exist for a same-volume rename to succeed at all, and
	// creating it up front keeps the common case a single atomic rename; ANY
	// remaining rename failure (cross-device is the expected one — EXDEV on
	// unix, ERROR_NOT_SAME_DRIVE on Windows) falls back to a copy verified by
	// size, so no errno sniffing is worth the portability
	if err := os.MkdirAll(filepath.Dir(dstDir), 0o755); err != nil {
		return err
	}
	if err := os.Rename(srcDir, dstDir); err == nil {
		fmt.Printf("[split] media moved %s -> %s\n", srcDir, dstDir)
		return nil
	}

	if err := copyTree(srcDir, dstDir); err != nil {
		return fmt.Errorf("copy fallback: %w", err)
	}
	if err := os.RemoveAll(srcDir); err != nil {
		return err
	}
	fmt.Printf("[split] media copied+verified %s -> %s\n", srcDir, dstDir)
	return nil
}

// copyTree recursively copies srcDir into a fresh dstDir, every regular file
// size-verified as it lands.
func copyTree(srcDir, dstDir string) error {
	return filepath.WalkDir(srcDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcDir, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dstDir, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		return copyVerify(p, target)
	})
}

// copyVerify copies src's contents into dst, created fresh, and fails unless
// the landed file carries exactly the source's byte size.
func copyVerify(src, dst string) error {
	si, err := os.Stat(src)
	if err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err = io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err = out.Close(); err != nil {
		return err
	}
	di, err := os.Stat(dst)
	if err != nil {
		return err
	}
	if si.Size() != di.Size() {
		return fmt.Errorf("%s copied %d bytes, want %d", dst, di.Size(), si.Size())
	}
	return nil
}

// printRemainder shows what each root now holds, source first, so the
// split's outcome is visible without opening a separate sqlite shell.
func printRemainder(db *sql.DB) error {
	for _, r := range []struct{ name, schema string }{{"source", "main"}, {"destination", "dstx"}} {
		rows, err := db.Query("SELECT dialog_id FROM " + r.schema + ".dialogs ORDER BY dialog_id")
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return err
			}
			ids = append(ids, strconv.FormatInt(id, 10))
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		_ = rows.Close()
		if len(ids) == 0 {
			fmt.Printf("[split] %s now holds no dialogs\n", r.name)
		} else {
			fmt.Printf("[split] %s now holds: %s\n", r.name, strings.Join(ids, ", "))
		}
	}
	return nil
}

// dsn builds the source DSN with every per-connection pragma riding in it,
// mirroring internal/store's openDB: PRAGMA foreign_keys (and busy_timeout)
// are per-connection state, and database/sql pools connections, so an Exec'd
// pragma would only cover whichever single pooled connection served it — the
// DSN form makes the driver apply them on every connection it opens. WAL lets
// reads proceed during writes; busy_timeout absorbs lock waits.
func dsn(path string) string {
	return "file:" + escapeDSNPath(path) +
		"?_pragma=foreign_keys(1)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)"
}

// escapeDSNPath percent-encodes the bytes sqlite's URI parser treats
// specially in a path segment, so an archive directory containing them still
// resolves. Duplicated from internal/store rather than exported there: one
// offline caller does not justify widening the store package's surface.
func escapeDSNPath(path string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(path); i++ {
		switch c := path[i]; c {
		case '%', '?', '#':
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0xF])
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// absPath canonicalizes to an absolute, cleaned path so a relative --to and
// an absolute -d naming the same root still compare equal. A path that cannot
// be made absolute is used verbatim; the failure surfaces at the first file
// operation that needs it.
func absPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return abs
}
