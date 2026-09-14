package migrate

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/myl7/tgxiv/internal/archive"
	"github.com/myl7/tgxiv/internal/store"
)

// dbMigrateOptions carries the `migrate db` flags.
type dbMigrateOptions struct {
	ChatID   int64
	Username string
	Title    string
	Kind     string
	Force    bool
}

// newDBCmd builds the `migrate db` subcommand: convert one old v2 channel
// directory into a dialog of the archive root the shared --dir flag points
// at. Like the rest of this package it stays offline — no tdl call — so the
// dialog's username/title metadata is not fetched from Telegram here; a later
// sync refreshes it, and this command only takes manual optional flags.
func newDBCmd(cfg func() (archive.Config, error)) *cobra.Command {
	var opts dbMigrateOptions
	cmd := &cobra.Command{
		Use:   "db <old-channel-dir>",
		Short: "Convert one old channel archive dir (archive.db + media/) into a dialog of this archive root (offline, no tdl call)",
		Long: `Convert one old v2 channel archive directory — archive.db plus flat
media/<chat>_<msg>_<name> files, export/ snapshots, logs/ — into a dialog of
the archive root given by the shared --dir flag. Runs once per old directory;
offline, no tdl call.

All DB state is written before any file moves, so a run that dies mid-move is
safely re-runnable with --force: upserts are idempotent, already-moved files
are detected and kept, and task progress made between runs is never rewound.
The old directory is never deleted or rewritten beyond its media files being
renamed out; archive.db stays behind as the safety backup.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := cfg()
			if err != nil {
				return err
			}
			return migrateDB(c, args[0], opts)
		},
	}
	f := cmd.Flags()
	f.Int64Var(&opts.ChatID, "chat-id", 0, "bare Telegram dialog id the old archive belongs to (default: derived from the archive)")
	f.StringVar(&opts.Username, "username", "", "dialog username, stored without a leading '@' (optional)")
	f.StringVar(&opts.Title, "title", "", "dialog title (optional)")
	f.StringVar(&opts.Kind, "kind", "", "dialog kind: channel | group | private (stored leniently; optional)")
	f.BoolVar(&opts.Force, "force", false, "allow re-running when the dialog already exists in this archive")
	return cmd
}

// oldMessage is one row of a v2 archive's messages table.
type oldMessage struct {
	MsgID int
	Type  string
	Date  int
	Text  string
	File  string
	Raw   string
}

// oldDownload is one row of a v2 archive's downloads manifest.
type oldDownload struct {
	MsgID      int
	DialogID   int64
	FileName   string
	Size       int64
	MediaType  string
	Date       int
	Status     string
	Attempts   int
	ActualSize int64
	Path       string
	Error      string
}

// openOldDB opens the v2 archive.db read-only (the conversion must never
// write its source) and guards the layouts it cannot read: a messages table
// still carrying a dialog_id column is the ancient v1 layout, which only the
// previous tgxiv release knows how to upgrade (it does so on open).
func openOldDB(dbPath string) (*sql.DB, error) {
	// read-only like the webui's viewer: busy_timeout absorbs any contention
	// with a still-running old binary, mode=ro protects the source
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro&_pragma=busy_timeout(3000)")
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open %s: %w", dbPath, err)
	}

	cols, err := dbTableColumns(db, "messages")
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("inspect %s: %w", dbPath, err)
	}
	if len(cols) == 0 {
		_ = db.Close()
		return nil, fmt.Errorf("%s has no messages table; not an old v2 channel archive", dbPath)
	}
	if cols["dialog_id"] {
		_ = db.Close()
		return nil, fmt.Errorf("%s still uses the ancient v1 layout (messages carries a dialog_id column); "+
			"run the previous tgxiv release once on the old directory to upgrade it to v2 first, then re-run", dbPath)
	}
	return db, nil
}

// readOldMessages reads every v2 content row verbatim.
func readOldMessages(db *sql.DB) ([]oldMessage, error) {
	rows, err := db.Query(`SELECT msg_id, type, date, text, file, raw FROM messages ORDER BY msg_id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []oldMessage
	for rows.Next() {
		var m oldMessage
		if err := rows.Scan(&m.MsgID, &m.Type, &m.Date, &m.Text, &m.File, &m.Raw); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// readOldDownloads reads every v2 manifest row verbatim, progress included.
func readOldDownloads(db *sql.DB) ([]oldDownload, error) {
	rows, err := db.Query(`
SELECT msg_id, dialog_id, file_name, size, media_type, date, status, attempts, actual_size, path, error
FROM downloads ORDER BY msg_id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []oldDownload
	for rows.Next() {
		var d oldDownload
		if err := rows.Scan(&d.MsgID, &d.DialogID, &d.FileName, &d.Size, &d.MediaType, &d.Date,
			&d.Status, &d.Attempts, &d.ActualSize, &d.Path, &d.Error); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// oldWatermark reads meta.last_msg_id. A missing meta table or key means the
// old archive never advanced one, so 0 is correct, not an error.
func oldWatermark(db *sql.DB) (int, error) {
	has, err := dbTableExists(db, "meta")
	if err != nil || !has {
		return 0, err
	}
	var v string
	err = db.QueryRow(`SELECT value FROM meta WHERE key = 'last_msg_id'`).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("parse meta last_msg_id %q: %w", v, err)
	}
	return n, nil
}

// oldChannelID reads meta.channel_id the same way oldWatermark reads the
// watermark. A missing meta table or key, or a value that does not parse as
// a positive id, reports "not found" rather than an error: the caller's
// remedy is an explicit --chat-id either way.
func oldChannelID(db *sql.DB) (int64, bool, error) {
	has, err := dbTableExists(db, "meta")
	if err != nil || !has {
		return 0, false, err
	}
	var v string
	err = db.QueryRow(`SELECT value FROM meta WHERE key = 'channel_id'`).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	id, err := strconv.ParseInt(v, 10, 64)
	if err != nil || id <= 0 {
		return 0, false, nil
	}
	return id, true, nil
}

// deriveChatID resolves which dialog an old archive belongs to when
// --chat-id is omitted. The downloads manifest is the authority — its
// distinct dialog ids must be exactly one — with meta.channel_id (the same
// table the watermark lives in) as the fallback for an empty manifest,
// which carries no id of its own. When neither can tell, only an explicit
// --chat-id can.
func deriveChatID(db *sql.DB, dls []oldDownload) (int64, error) {
	ids := distinctDialogIDs(dls)
	if len(ids) > 1 {
		return 0, fmt.Errorf("this archive mixes dialog id(s) %v; migrate db converts exactly one dialog per directory", ids)
	}
	if len(ids) == 1 && ids[0] > 0 {
		return ids[0], nil
	}
	id, ok, err := oldChannelID(db)
	if err != nil {
		return 0, fmt.Errorf("read meta channel_id: %w", err)
	}
	if ok {
		return id, nil
	}
	return 0, fmt.Errorf("cannot tell which dialog this archive belongs to (empty downloads manifest, no usable meta channel_id); pass --chat-id explicitly")
}

// dbTableExists reports whether db carries a table by that name.
func dbTableExists(db *sql.DB, table string) (bool, error) {
	var name string
	err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name = ?`, table).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// dbTableColumns lists the column names of table.
func dbTableColumns(db *sql.DB, table string) (map[string]bool, error) {
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`) // table is a literal we control
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := map[string]bool{}
	for rows.Next() {
		var cid, notNull, pk int
		var name, typ string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			return nil, err
		}
		out[name] = true
	}
	return out, rows.Err()
}

// distinctDialogIDs returns the ascending distinct dialog ids among the old
// downloads.
func distinctDialogIDs(dls []oldDownload) []int64 {
	seen := map[int64]bool{}
	var ids []int64
	for _, d := range dls {
		if !seen[d.DialogID] {
			seen[d.DialogID] = true
			ids = append(ids, d.DialogID)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// mismatchedDialogIDs returns the distinct dialog ids among the old downloads
// that differ from chatID, ascending — empty when the directory belongs to it.
func mismatchedDialogIDs(dls []oldDownload, chatID int64) []int64 {
	var bad []int64
	for _, id := range distinctDialogIDs(dls) {
		if id != chatID {
			bad = append(bad, id)
		}
	}
	return bad
}

// mediaPlan is one media file of the old flat media/ dir, matched to its
// message and renamed to the v3 per-dialog layout.
type mediaPlan struct {
	msgID   int
	newBase string // the v3 file name, "<msgID>_<name>"
	oldPath string
	size    int64
}

// oldMediaRestRe matches the part of an old media file name after the
// "<chatID>_" prefix: "<msgID>_<name>".
var oldMediaRestRe = regexp.MustCompile(`^(\d+)_(.*)$`)

// scanOldMedia lists the <old>/media entries named "<chatID>_<msgID>_<name>".
// Stale "*.tmp" partials and other dialogs' files (a mixed dir is a hard error
// before this point, but defense is cheap) are left untouched.
func scanOldMedia(oldDir string, chatID int64) ([]mediaPlan, error) {
	entries, err := os.ReadDir(filepath.Join(oldDir, "media"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	prefix := strconv.FormatInt(chatID, 10) + "_"
	var out []mediaPlan
	for _, e := range entries {
		if e.IsDir() || strings.HasSuffix(e.Name(), ".tmp") || !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		m := oldMediaRestRe.FindStringSubmatch(strings.TrimPrefix(e.Name(), prefix))
		if m == nil {
			continue
		}
		msgID, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			return nil, err
		}
		out = append(out, mediaPlan{
			msgID:   msgID,
			newBase: m[1] + "_" + m[2],
			oldPath: filepath.Join(oldDir, "media", e.Name()),
			size:    fi.Size(),
		})
	}
	return out, nil
}

// expectedBase derives the v3 file name ("<msgID>_<name>") a row's media
// should live under when the old file itself is gone. The old row's recorded
// path is the authority — the v2 downloader located files by prefix and the
// name on disk may be sanitized away from the manifest's file_name — with
// file_name as the fallback when no path was recorded.
func expectedBase(d oldDownload, chat string) string {
	msg := strconv.Itoa(d.MsgID)
	if d.Path != "" {
		if base := path.Base(d.Path); strings.HasPrefix(base, chat+"_"+msg+"_") {
			return msg + "_" + strings.TrimPrefix(base, chat+"_"+msg+"_")
		}
	}
	return msg + "_" + d.FileName
}

// targetHasFile reports whether a previous run (or a later pipeline download)
// already put the row's file at its v3 location, size-verified against the
// manifest size — the same check the downloader's verify applies.
func targetHasFile(root, chat string, d oldDownload) bool {
	fi, err := os.Stat(filepath.Join(root, "media", chat, expectedBase(d, chat)))
	return err == nil && !fi.IsDir() && fi.Size() == d.Size
}

// moveFile moves src to dst. os.Rename is preferred (atomic, instant); when
// it fails for ANY reason — a cross-device link (EXDEV, on Windows surfaced
// as ERROR_NOT_SAME_DRIVE) is the expected case, and sniffing errno variants
// across platforms buys nothing — it falls back to a copy verified by size,
// deleting the source only after the copy verified. A crash mid-copy leaves
// both files behind, which a --force re-run heals via the already-present
// check.
func moveFile(src, dst string) error {
	renameErr := os.Rename(src, dst)
	if renameErr == nil {
		return nil
	}
	if err := copyFile(src, dst); err != nil {
		return fmt.Errorf("rename failed (%v); copy fallback: %w", renameErr, err)
	}
	si, err := os.Stat(src)
	if err != nil {
		return err
	}
	di, err := os.Stat(dst)
	if err != nil {
		return err
	}
	if si.Size() != di.Size() {
		return fmt.Errorf("copy %s -> %s verified %d bytes, want %d", src, dst, di.Size(), si.Size())
	}
	return os.Remove(src)
}

// copyFile copies src's contents into dst, created fresh.
func copyFile(src, dst string) (err error) {
	r, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()

	w, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := w.Close(); err == nil && cerr != nil {
			err = cerr
		}
	}()
	_, err = io.Copy(w, r)
	return err
}

// logsNameRe matches the old pipeline's log files; only the extension is
// pinned so future log kinds ride along.
var logsNameRe = regexp.MustCompile(`\.txt$`)

// copyMatching copies the regular files of src whose names match re into dst,
// skipping names that already exist there (a --force re-run must not clobber
// or duplicate). Copy, not move: the old directory must stay complete as the
// safety backup, except for its media files, which are renamed out.
func copyMatching(src, dst string, re *regexp.Regexp) (int, error) {
	entries, err := os.ReadDir(src)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return 0, fmt.Errorf("create %s: %w", dst, err)
	}

	n := 0
	for _, e := range entries {
		if e.IsDir() || !re.MatchString(e.Name()) {
			continue
		}
		d := filepath.Join(dst, e.Name())
		if _, err := os.Stat(d); err == nil {
			continue
		}
		if err := copyFile(filepath.Join(src, e.Name()), d); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// dialogTaskCounts returns the dialog's per-status task counts.
func dialogTaskCounts(st *store.Store, dialogID int64) (done, pending, failed int, err error) {
	per, err := st.CountsByDialog()
	if err != nil {
		return 0, 0, 0, err
	}
	for _, dc := range per {
		if dc.Dialog.DialogID == dialogID {
			return dc.Done, dc.Pending, dc.Failed, nil
		}
	}
	return 0, 0, 0, nil
}

// writeDialogMarker reads the dialog row back and drops the human-readable
// pointer file into the target root's media/<id>/, which may not exist yet on
// a first run (WriteDialogMarker builds it). Reading the row back is what
// makes a --force re-run without --username/--title keep the first run's
// marker: the upsert's non-clobber merge, not this run's flags, decides the
// content. Strictly best-effort — the marker must never fail the conversion.
func writeDialogMarker(st *store.Store, root string, dialogID int64) {
	d, ok, err := st.GetDialog(dialogID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[migrate] dialog marker: %v\n", err)
		return
	}
	if !ok {
		fmt.Fprintf(os.Stderr, "[migrate] dialog marker: dialog %d missing right after upsert\n", dialogID)
		return
	}
	if err := archive.WriteDialogMarker(root, d); err != nil {
		fmt.Fprintf(os.Stderr, "[migrate] dialog marker: %v\n", err)
	}
}

// migrateDB converts one old v2 channel directory into a dialog of the root.
// The run is DB-first — dialog row, content (with synthesized placeholders
// for orphan downloads), tasks with their planned paths, watermark — then the
// media files move, then a reconciliation pass fails the command on any
// count mismatch. That ordering makes a run that dies mid-move re-runnable:
// with --force every step heals (upserts are idempotent, ImportTasks never
// rewinds progress, already-moved files are detected and kept).
func migrateDB(c archive.Config, oldDir string, opts dbMigrateOptions) error {
	// 0 means "derive from the archive"; only an explicitly negative value
	// is a user mistake worth rejecting up front
	if opts.ChatID < 0 {
		return fmt.Errorf("--chat-id must be a positive Telegram dialog id, or omitted to derive it from the archive (got %d)", opts.ChatID)
	}

	oldDBPath := filepath.Join(oldDir, "archive.db")
	if _, err := os.Stat(oldDBPath); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%s does not exist; <old-channel-dir> must be a v2 channel archive directory", oldDBPath)
		}
		return err
	}

	odb, err := openOldDB(oldDBPath)
	if err != nil {
		return err
	}
	defer func() { _ = odb.Close() }()

	msgs, err := readOldMessages(odb)
	if err != nil {
		return fmt.Errorf("read old messages: %w", err)
	}
	dls, err := readOldDownloads(odb)
	if err != nil {
		return fmt.Errorf("read old downloads: %w", err)
	}
	wm, err := oldWatermark(odb)
	if err != nil {
		return err
	}

	// --chat-id omitted: let the archive say which dialog it is
	if opts.ChatID == 0 {
		opts.ChatID, err = deriveChatID(odb, dls)
		if err != nil {
			return err
		}
	}

	// the directory must belong to the declared dialog: a wrong --chat-id
	// would otherwise split one channel's archive across two dialogs
	if bad := mismatchedDialogIDs(dls, opts.ChatID); len(bad) > 0 {
		return fmt.Errorf("archive.db holds downloads of dialog id(s) %v, not %d; pass the id the old directory itself belongs to", bad, opts.ChatID)
	}

	// the filesystem drives the media plan; scanning before the target opens
	// keeps all old-side reads in one place
	plans, err := scanOldMedia(oldDir, opts.ChatID)
	if err != nil {
		return fmt.Errorf("scan %s: %w", filepath.Join(oldDir, "media"), err)
	}
	fmt.Printf("[migrate] %s: %d message(s), %d download task(s), %d media file(s) to move\n", oldDir, len(msgs), len(dls), len(plans))

	// prepare the target root by hand: archive.Open's extra side effects
	// (tdl runner setup) are exactly what an offline conversion must avoid
	root := c.Dir
	for _, d := range []string{filepath.Join(root, "media"), filepath.Join(root, "export"), filepath.Join(root, "logs")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", d, err)
		}
	}
	st, err := store.Open(filepath.Join(root, "tgxiv.sqlite"))
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	_, existed, err := st.GetDialog(opts.ChatID)
	if err != nil {
		return err
	}
	if existed && !opts.Force {
		return fmt.Errorf("dialog %d already in this archive (use --force to re-run)", opts.ChatID)
	}

	username := strings.TrimPrefix(opts.Username, "@")
	if err := st.UpsertDialog(store.Dialog{
		DialogID: opts.ChatID,
		Username: username,
		Title:    opts.Title,
		Kind:     opts.Kind,
	}); err != nil {
		return err
	}
	writeDialogMarker(st, root, opts.ChatID)

	// content rows verbatim, plus a synthesized placeholder for every orphan
	// download (a v1-upgrade legacy: a manifest row whose message row was
	// lost) so the tasks FK has something to point at
	recs := make([]store.ContentRecord, 0, len(msgs)+len(dls))
	for _, m := range msgs {
		recs = append(recs, store.ContentRecord{MsgID: m.MsgID, Type: m.Type, Date: m.Date, Text: m.Text, File: m.File, Raw: m.Raw})
	}
	hasMsg := make(map[int]bool, len(msgs))
	for _, m := range msgs {
		hasMsg[m.MsgID] = true
	}
	orphans := 0
	for _, d := range dls {
		if !hasMsg[d.MsgID] {
			recs = append(recs, store.ContentRecord{MsgID: d.MsgID, Type: "message", Date: d.Date})
			orphans++
		}
	}
	if err := st.UpsertContent(opts.ChatID, recs); err != nil {
		return err
	}

	// plan the task states with the filesystem as the final authority for
	// done-marking: whatever the old row says, a file present at exactly the
	// manifest size — in the old dir (about to move) or already at the target
	// from a previous run — lands the task done at the rewritten path, with
	// attempts kept as recorded history and the error cleared, so a JSON-era
	// archive imported as all-pending needs no post-migration download pass
	// whose only job is re-verifying files already in place. A file present
	// at the WRONG size counts as absent (it still moves; the row self-heals
	// via download). Rows without a usable file keep the old split: done
	// degrades to pending (the viewer must not show a 404 hole), and
	// pending/failed keep their progress but drop old-layout paths.
	chat := strconv.FormatInt(opts.ChatID, 10)
	byMsg := make(map[int]mediaPlan, len(plans))
	for _, p := range plans {
		byMsg[p.msgID] = p
	}
	states := make([]store.TaskState, 0, len(dls))
	downgraded := map[int]bool{}
	promoted := map[int]bool{}
	for _, d := range dls {
		t := store.TaskState{
			MsgID:      d.MsgID,
			FileName:   d.FileName,
			Size:       d.Size,
			MediaType:  d.MediaType,
			Status:     d.Status,
			Attempts:   d.Attempts,
			ActualSize: d.ActualSize,
			Error:      d.Error,
		}
		present := ""
		if p, ok := byMsg[d.MsgID]; ok && p.size == d.Size {
			present = p.newBase
		} else if targetHasFile(root, chat, d) {
			present = expectedBase(d, chat)
		}
		if present != "" {
			t.Status = store.StatusDone
			t.ActualSize = d.Size
			t.Path = "media/" + chat + "/" + present
			t.Error = ""
			if d.Status != store.StatusDone {
				promoted[d.MsgID] = true
			}
		} else if d.Status == store.StatusDone {
			t = store.TaskState{MsgID: d.MsgID, FileName: d.FileName, Size: d.Size, MediaType: d.MediaType, Status: store.StatusPending}
			downgraded[d.MsgID] = true
		}
		states = append(states, t)
	}

	// snapshot the pre-import task state: on a --force re-run the existing
	// rows win over the imported ones, so the expected post-import counts are
	// "pre-existing rows keep their state; fresh rows get the planned one" —
	// exactly what ImportTasks' conflict rule produces.
	preDone, prePending, preFailed := 0, 0, 0
	if existed {
		if preDone, prePending, preFailed, err = dialogTaskCounts(st, opts.ChatID); err != nil {
			return err
		}
	}
	expDone, expPending, expFailed := preDone, prePending, preFailed
	downgradedApplied, promotedApplied := 0, 0
	for _, t := range states {
		if _, ok, err := st.Attempts(opts.ChatID, t.MsgID); err != nil {
			return err
		} else if ok {
			continue // existing row wins; its bucket is already counted
		}
		switch t.Status {
		case store.StatusDone:
			expDone++
		case store.StatusPending:
			expPending++
		case store.StatusFailed:
			expFailed++
		}
		if downgraded[t.MsgID] {
			downgradedApplied++
		}
		if promoted[t.MsgID] {
			promotedApplied++
		}
	}

	if err := st.ImportTasks(opts.ChatID, states); err != nil {
		return err
	}
	if err := st.AdvanceLastMsgID(opts.ChatID, wm); err != nil {
		return err
	}

	// move the media. Files whose target already exists at the same size are
	// kept as-is: a --force re-run says "already moved", and re-copying would
	// only race the live pipeline.
	movedFiles, skippedFiles := 0, 0
	var movedBytes, plannedBytes int64
	targetDir := filepath.Join(root, "media", chat)
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", targetDir, err)
	}
	for _, p := range plans {
		plannedBytes += p.size
		dst := filepath.Join(targetDir, p.newBase)
		if fi, err := os.Stat(dst); err == nil && !fi.IsDir() && fi.Size() == p.size {
			skippedFiles++
			continue
		}
		if err := moveFile(p.oldPath, dst); err != nil {
			return fmt.Errorf("move %s: %w", p.oldPath, err)
		}
		movedFiles++
		movedBytes += p.size
	}

	// export snapshots move under the dialog's own dir (the new viewer reads
	// them per dialog); logs merge into the flat root logs/
	nExport, err := copyMatching(filepath.Join(oldDir, "export"), filepath.Join(root, "export", chat), stampExportRe)
	if err != nil {
		return fmt.Errorf("copy export snapshots: %w", err)
	}
	nLogs, err := copyMatching(filepath.Join(oldDir, "logs"), filepath.Join(root, "logs"), logsNameRe)
	if err != nil {
		return fmt.Errorf("copy logs: %w", err)
	}

	// reconcile: counts must line up or the command fails — a silent partial
	// conversion is the one failure mode this command must never have
	newMsgs, err := st.MessageCount(opts.ChatID)
	if err != nil {
		return err
	}
	if want := len(msgs) + orphans; newMsgs < want || (!existed && newMsgs != want) {
		return fmt.Errorf("reconcile: %d message(s) in the new archive, want %d (old %d + %d synthesized orphan placeholder(s))",
			newMsgs, want, len(msgs), orphans)
	}
	newDone, newPending, newFailed, err := dialogTaskCounts(st, opts.ChatID)
	if err != nil {
		return err
	}
	if newDone != expDone || newPending != expPending || newFailed != expFailed {
		return fmt.Errorf("reconcile: tasks done %d, pending %d, failed %d; want done %d, pending %d, failed %d",
			newDone, newPending, newFailed, expDone, expPending, expFailed)
	}
	if movedFiles+skippedFiles != len(plans) {
		return fmt.Errorf("reconcile: media %d moved + %d kept != %d planned", movedFiles, skippedFiles, len(plans))
	}

	wmNow, err := st.LastMsgID(opts.ChatID)
	if err != nil {
		return err
	}
	// label from what is actually stored: a --force re-run without metadata
	// flags keeps the fields the first run recorded
	dlg, _, err := st.GetDialog(opts.ChatID)
	if err != nil {
		return err
	}
	label := dlg.Username
	if label == "" {
		label = dlg.Title
	}
	if label == "" {
		label = chat
	}
	fmt.Printf("[migrate] dialog %s (%s): %d message(s) incl. %d synthesized orphan placeholder(s), watermark %d\n",
		chat, label, newMsgs, orphans, wmNow)
	fmt.Printf("[migrate] tasks: done %d (incl. %d promoted), pending %d+%d (downgraded), failed %d\n",
		newDone, promotedApplied, newPending-downgradedApplied, downgradedApplied, newFailed)
	fmt.Printf("[migrate] media: %d/%d file(s) moved (%d/%d bytes), %d already present\n",
		movedFiles, len(plans), movedBytes, plannedBytes, skippedFiles)
	fmt.Printf("[migrate] export: %d snapshot(s) copied, logs: %d file(s) copied\n", nExport, nLogs)
	fmt.Println("[migrate] old directory kept; archive.db is the backup")
	return nil
}
