// Package store keeps tgvault's local state in a SQLite database.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// Item states.
const (
	StatePending   = "pending"   // changed, waiting for its quiet period
	StateUploading = "uploading" // a worker is uploading it
	StateFailed    = "failed"    // last attempt failed; retried at NextAttempt
	StateSynced    = "synced"    // latest local content is in Telegram
	StateSkipped   = "skipped"   // will not be uploaded (too big, empty, ...)
)

// Item kinds.
const (
	KindFile    = "file"
	KindProject = "project"
)

// Project states.
const (
	ProjectSuggested = "suggested"
	ProjectConfirmed = "confirmed"
	ProjectIgnored   = "ignored"
)

const schema = `
CREATE TABLE IF NOT EXISTS settings(
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS roots(
	id INTEGER PRIMARY KEY,
	path TEXT NOT NULL UNIQUE,
	label TEXT NOT NULL UNIQUE,
	paused INTEGER NOT NULL DEFAULT 0,
	last_scan INTEGER NOT NULL DEFAULT 0,
	scan_error TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS projects(
	id INTEGER PRIMARY KEY,
	root_id INTEGER NOT NULL,
	rel TEXT NOT NULL,
	state TEXT NOT NULL,
	markers TEXT NOT NULL DEFAULT '',
	UNIQUE(root_id, rel)
);
CREATE TABLE IF NOT EXISTS items(
	id INTEGER PRIMARY KEY,
	cloud_path TEXT NOT NULL UNIQUE,
	kind TEXT NOT NULL,
	root_id INTEGER NOT NULL,
	rel TEXT NOT NULL,
	size INTEGER NOT NULL DEFAULT 0,
	mtime INTEGER NOT NULL DEFAULT 0,
	fingerprint TEXT NOT NULL DEFAULT '',
	local_present INTEGER NOT NULL DEFAULT 1,
	state TEXT NOT NULL,
	note TEXT NOT NULL DEFAULT '',
	attempts INTEGER NOT NULL DEFAULT 0,
	next_attempt INTEGER NOT NULL DEFAULT 0,
	changed_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS items_root ON items(root_id);
CREATE INDEX IF NOT EXISTS items_state ON items(state);
CREATE TABLE IF NOT EXISTS versions(
	msg_id INTEGER PRIMARY KEY,
	cloud_path TEXT NOT NULL,
	kind TEXT NOT NULL,
	size INTEGER NOT NULL,
	mtime INTEGER NOT NULL,
	hash TEXT NOT NULL,
	fingerprint TEXT NOT NULL DEFAULT '',
	uploaded_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS versions_path ON versions(cloud_path);
CREATE TABLE IF NOT EXISTS resumes(
	cloud_path TEXT PRIMARY KEY,
	hash TEXT NOT NULL,
	file_id INTEGER NOT NULL,
	part_size INTEGER NOT NULL,
	done_parts INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
`

// Store wraps the database.
type Store struct {
	db *sql.DB
}

// Open opens (and creates if needed) the database at path.
func Open(path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// SQLite allows one writer; a single connection avoids "database is locked".
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}
	// Columns added after 0.1; the error just means the column already exists.
	_, _ = db.Exec(`ALTER TABLE items ADD COLUMN failing_since INTEGER NOT NULL DEFAULT 0`)
	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func now() int64 { return time.Now().Unix() }

// ---- settings ----

// Get returns a setting, or "" when unset.
func (s *Store) Get(key string) string {
	var v string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&v)
	if err != nil {
		return ""
	}
	return v
}

// Set stores a setting.
func (s *Store) Set(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO settings(key,value) VALUES(?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

// Delete removes settings.
func (s *Store) Delete(keys ...string) error {
	for _, k := range keys {
		if _, err := s.db.Exec(`DELETE FROM settings WHERE key=?`, k); err != nil {
			return err
		}
	}
	return nil
}

// ---- roots ----

// Root is a folder the user backs up.
type Root struct {
	ID        int64  `json:"id"`
	Path      string `json:"path"`
	Label     string `json:"label"`
	Paused    bool   `json:"paused"`
	LastScan  int64  `json:"lastScan"`
	ScanError string `json:"scanError"`
}

// Roots lists all backup roots.
func (s *Store) Roots() ([]Root, error) {
	rows, err := s.db.Query(`SELECT id,path,label,paused,last_scan,scan_error FROM roots ORDER BY label`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Root
	for rows.Next() {
		var r Root
		if err := rows.Scan(&r.ID, &r.Path, &r.Label, &r.Paused, &r.LastScan, &r.ScanError); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Root returns one root.
func (s *Store) Root(id int64) (Root, error) {
	var r Root
	err := s.db.QueryRow(`SELECT id,path,label,paused,last_scan,scan_error FROM roots WHERE id=?`, id).
		Scan(&r.ID, &r.Path, &r.Label, &r.Paused, &r.LastScan, &r.ScanError)
	return r, err
}

// AddRoot adds a root with a unique label.
func (s *Store) AddRoot(path, baseLabel string) (Root, error) {
	label := baseLabel
	for i := 2; ; i++ {
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM roots WHERE label=?`, label).Scan(&n); err != nil {
			return Root{}, err
		}
		if n == 0 {
			break
		}
		label = fmt.Sprintf("%s (%d)", baseLabel, i)
	}
	res, err := s.db.Exec(`INSERT INTO roots(path,label) VALUES(?,?)`, path, label)
	if err != nil {
		return Root{}, err
	}
	id, _ := res.LastInsertId()
	return s.Root(id)
}

// RemoveRoot stops backing up a root. Cloud versions are kept.
func (s *Store) RemoveRoot(id int64) error {
	for _, q := range []string{
		`DELETE FROM items WHERE root_id=?`,
		`DELETE FROM projects WHERE root_id=?`,
		`DELETE FROM roots WHERE id=?`,
	} {
		if _, err := s.db.Exec(q, id); err != nil {
			return err
		}
	}
	return nil
}

// SetRootPaused pauses or resumes a root.
func (s *Store) SetRootPaused(id int64, paused bool) error {
	_, err := s.db.Exec(`UPDATE roots SET paused=? WHERE id=?`, paused, id)
	return err
}

// SetRootScan records the result of a scan.
func (s *Store) SetRootScan(id int64, scanErr string) error {
	_, err := s.db.Exec(`UPDATE roots SET last_scan=?, scan_error=? WHERE id=?`, now(), scanErr, id)
	return err
}

// ---- projects ----

// Project is a folder detected as a code project.
type Project struct {
	ID      int64  `json:"id"`
	RootID  int64  `json:"rootId"`
	Rel     string `json:"rel"`
	State   string `json:"state"`
	Markers string `json:"markers"`
}

// ProjectStates returns rel -> state for a root.
func (s *Store) ProjectStates(rootID int64) (map[string]string, error) {
	rows, err := s.db.Query(`SELECT rel,state FROM projects WHERE root_id=?`, rootID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var rel, st string
		if err := rows.Scan(&rel, &st); err != nil {
			return nil, err
		}
		out[rel] = st
	}
	return out, rows.Err()
}

// SuggestProject records a newly detected project (no-op if already known).
func (s *Store) SuggestProject(rootID int64, rel, markers string) error {
	_, err := s.db.Exec(`INSERT INTO projects(root_id,rel,state,markers) VALUES(?,?,?,?)
		ON CONFLICT(root_id,rel) DO NOTHING`, rootID, rel, ProjectSuggested, markers)
	return err
}

// Projects lists projects, optionally filtered by state.
func (s *Store) Projects(state string) ([]Project, error) {
	q := `SELECT id,root_id,rel,state,markers FROM projects`
	var args []any
	if state != "" {
		q += ` WHERE state=?`
		args = append(args, state)
	}
	q += ` ORDER BY root_id, rel`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Project
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.ID, &p.RootID, &p.Rel, &p.State, &p.Markers); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Project returns one project.
func (s *Store) Project(id int64) (Project, error) {
	var p Project
	err := s.db.QueryRow(`SELECT id,root_id,rel,state,markers FROM projects WHERE id=?`, id).
		Scan(&p.ID, &p.RootID, &p.Rel, &p.State, &p.Markers)
	return p, err
}

// SetProjectState confirms or ignores a project.
func (s *Store) SetProjectState(id int64, state string) error {
	_, err := s.db.Exec(`UPDATE projects SET state=? WHERE id=?`, state, id)
	return err
}

// ---- items ----

// Item is one thing that gets uploaded: a file, or a whole project as a ZIP.
type Item struct {
	ID           int64  `json:"id"`
	CloudPath    string `json:"cloudPath"`
	Kind         string `json:"kind"`
	RootID       int64  `json:"rootId"`
	Rel          string `json:"rel"`
	Size         int64  `json:"size"`
	Mtime        int64  `json:"mtime"`
	Fingerprint  string `json:"-"`
	LocalPresent bool   `json:"localPresent"`
	State        string `json:"state"`
	Note         string `json:"note"`
	Attempts     int    `json:"attempts"`
	NextAttempt  int64  `json:"nextAttempt"`
	ChangedAt    int64  `json:"changedAt"`
}

const itemCols = `id,cloud_path,kind,root_id,rel,size,mtime,fingerprint,local_present,state,note,attempts,next_attempt,changed_at`

func scanItem(sc interface{ Scan(...any) error }) (Item, error) {
	var it Item
	err := sc.Scan(&it.ID, &it.CloudPath, &it.Kind, &it.RootID, &it.Rel, &it.Size, &it.Mtime,
		&it.Fingerprint, &it.LocalPresent, &it.State, &it.Note, &it.Attempts, &it.NextAttempt, &it.ChangedAt)
	return it, err
}

func (s *Store) queryItems(q string, args ...any) ([]Item, error) {
	rows, err := s.db.Query(`SELECT `+itemCols+` FROM items `+q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Item
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// ItemsByRoot returns every item of a root keyed by cloud path.
func (s *Store) ItemsByRoot(rootID int64) (map[string]Item, error) {
	items, err := s.queryItems(`WHERE root_id=?`, rootID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]Item, len(items))
	for _, it := range items {
		out[it.CloudPath] = it
	}
	return out, nil
}

// Item returns one item.
func (s *Store) Item(id int64) (Item, error) {
	return scanItem(s.db.QueryRow(`SELECT `+itemCols+` FROM items WHERE id=?`, id))
}

// ItemByPath returns an item by cloud path.
func (s *Store) ItemByPath(cloudPath string) (Item, error) {
	return scanItem(s.db.QueryRow(`SELECT `+itemCols+` FROM items WHERE cloud_path=?`, cloudPath))
}

// ItemsUnder returns items whose cloud path equals prefix or is inside it.
func (s *Store) ItemsUnder(prefix string) ([]Item, error) {
	if prefix == "" {
		return s.queryItems(``)
	}
	return s.queryItems(`WHERE cloud_path=? OR substr(cloud_path,1,?)=?`, prefix, len(prefix)+1, prefix+"/")
}

// UpsertSeen records a scanned item. It marks the item pending when its
// size, mtime or fingerprint changed, or when it was missing before.
func (s *Store) UpsertSeen(it Item) error {
	var id int64
	var size, mtime int64
	var fp, state string
	var present bool
	err := s.db.QueryRow(`SELECT id,size,mtime,fingerprint,state,local_present FROM items WHERE cloud_path=?`, it.CloudPath).
		Scan(&id, &size, &mtime, &fp, &state, &present)
	if it.State == "" {
		it.State = StatePending
	}
	if errors.Is(err, sql.ErrNoRows) {
		_, err = s.db.Exec(`INSERT INTO items(cloud_path,kind,root_id,rel,size,mtime,fingerprint,local_present,state,note,changed_at)
			VALUES(?,?,?,?,?,?,?,1,?,?,?)`,
			it.CloudPath, it.Kind, it.RootID, it.Rel, it.Size, it.Mtime, it.Fingerprint, it.State, it.Note, now())
		return err
	}
	if err != nil {
		return err
	}
	changed := size != it.Size || mtime != it.Mtime || fp != it.Fingerprint || !present
	if !changed && (it.State == StateSkipped) == (state == StateSkipped) {
		return nil
	}
	newState := it.State
	if newState == "" {
		newState = StatePending
	}
	if state == StateUploading && newState == StatePending {
		// Let the running upload finish; it rechecks the file afterwards.
		_, err = s.db.Exec(`UPDATE items SET size=?,mtime=?,fingerprint=?,local_present=1,changed_at=? WHERE id=?`,
			it.Size, it.Mtime, it.Fingerprint, now(), id)
		return err
	}
	_, err = s.db.Exec(`UPDATE items SET kind=?,size=?,mtime=?,fingerprint=?,local_present=1,state=?,note=?,attempts=0,next_attempt=0,changed_at=? WHERE id=?`,
		it.Kind, it.Size, it.Mtime, it.Fingerprint, newState, it.Note, now(), id)
	return err
}

// MarkGone marks items as no longer present locally. Items never uploaded are removed.
func (s *Store) MarkGone(ids []int64) error {
	for _, id := range ids {
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM versions v JOIN items i ON v.cloud_path=i.cloud_path WHERE i.id=?`, id).Scan(&n); err != nil {
			return err
		}
		var err error
		if n == 0 {
			_, err = s.db.Exec(`DELETE FROM items WHERE id=? AND state!=?`, id, StateUploading)
		} else {
			_, err = s.db.Exec(`UPDATE items SET local_present=0, state=?, note='', attempts=0 WHERE id=? AND state!=?`, StateSynced, id, StateUploading)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// ClaimNext picks the next item ready to upload and marks it uploading.
// quietFile/quietProject are how long an item must be unchanged first.
func (s *Store) ClaimNext(quietFile, quietProject time.Duration) (Item, bool, error) {
	t := now()
	it, err := scanItem(s.db.QueryRow(`SELECT `+itemCols+` FROM items
		WHERE local_present=1 AND state IN (?,?) AND next_attempt<=?
		  AND changed_at <= CASE kind WHEN ? THEN ? ELSE ? END
		  AND root_id IN (SELECT id FROM roots WHERE paused=0)
		ORDER BY state=? , changed_at LIMIT 1`,
		StatePending, StateFailed, t, KindProject, t-int64(quietProject.Seconds()), t-int64(quietFile.Seconds()), StateFailed))
	if errors.Is(err, sql.ErrNoRows) {
		return Item{}, false, nil
	}
	if err != nil {
		return Item{}, false, err
	}
	if _, err := s.db.Exec(`UPDATE items SET state=? WHERE id=?`, StateUploading, it.ID); err != nil {
		return Item{}, false, err
	}
	it.State = StateUploading
	return it, true, nil
}

// FinishItem records the outcome of processing an item. If the item changed
// again while it was uploading, it goes back to pending.
func (s *Store) FinishItem(id int64, state, note string, attempts int, nextAttempt, startedAt int64) error {
	_, err := s.db.Exec(`UPDATE items SET
		state = CASE WHEN ?=? AND changed_at>? THEN ? ELSE ? END,
		failing_since = CASE WHEN ?=? THEN CASE failing_since WHEN 0 THEN ? ELSE failing_since END ELSE 0 END,
		note=?, attempts=?, next_attempt=? WHERE id=?`,
		state, StateSynced, startedAt, StatePending, state,
		state, StateFailed, now(),
		note, attempts, nextAttempt, id)
	return err
}

// ResetUploading returns interrupted uploads to pending (used at startup).
func (s *Store) ResetUploading() error {
	if _, err := s.db.Exec(`UPDATE items SET state=? WHERE state=?`, StatePending, StateUploading); err != nil {
		return err
	}
	// Older versions recorded Telegram rate limits as failures; they are just waits.
	_, err := s.db.Exec(`UPDATE items SET state=?, note='', attempts=0, next_attempt=0
		WHERE state=? AND note LIKE 'Telegram asked tgvault to slow down%'`, StatePending, StateFailed)
	return err
}

// RetryAll makes failed items eligible now.
func (s *Store) RetryAll() error {
	_, err := s.db.Exec(`UPDATE items SET next_attempt=0 WHERE state=?`, StateFailed)
	return err
}

// RetryItem makes one item eligible now.
func (s *Store) RetryItem(id int64) error {
	_, err := s.db.Exec(`UPDATE items SET next_attempt=0, changed_at=0, state=CASE state WHEN ? THEN ? ELSE state END WHERE id=?`,
		StateSkipped, StatePending, id)
	return err
}

// Activity returns items that are not simply synced.
func (s *Store) Activity(limit int) ([]Item, error) {
	return s.queryItems(`WHERE state!=? ORDER BY CASE state WHEN ? THEN 0 WHEN ? THEN 1 WHEN ? THEN 2 ELSE 3 END, changed_at DESC LIMIT ?`,
		StateSynced, StateUploading, StateFailed, StatePending, limit)
}

// StaleFailures counts local items that have been failing since before the
// given time, and returns one example path.
func (s *Store) StaleFailures(before int64) (int, string, error) {
	var n int
	var sample sql.NullString
	err := s.db.QueryRow(`SELECT COUNT(*), MIN(cloud_path) FROM items
		WHERE state=? AND local_present=1 AND failing_since>0 AND failing_since<?`, StateFailed, before).Scan(&n, &sample)
	return n, sample.String, err
}

// ItemsByState returns local items in one state, most recently changed first.
func (s *Store) ItemsByState(state string, limit int) ([]Item, error) {
	return s.queryItems(`WHERE state=? AND local_present=1 ORDER BY changed_at DESC LIMIT ?`, state, limit)
}

// ForgetGoneUnder removes cloud-only items under a prefix (after their cloud copies are deleted).
func (s *Store) ForgetGoneUnder(prefix string) error {
	_, err := s.db.Exec(`DELETE FROM items WHERE local_present=0 AND (cloud_path=? OR substr(cloud_path,1,?)=?)`,
		prefix, len(prefix)+1, prefix+"/")
	return err
}

// Counts returns the number of items per state (only items present locally).
func (s *Store) Counts() (map[string]int, error) {
	rows, err := s.db.Query(`SELECT state, COUNT(*) FROM items WHERE local_present=1 GROUP BY state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return nil, err
		}
		out[st] = n
	}
	return out, rows.Err()
}

// ---- versions ----

// Version is one copy of a file stored in Telegram.
type Version struct {
	MsgID       int    `json:"msgId"`
	CloudPath   string `json:"cloudPath"`
	Kind        string `json:"kind"`
	Size        int64  `json:"size"`
	Mtime       int64  `json:"mtime"`
	Hash        string `json:"hash"`
	Fingerprint string `json:"-"`
	UploadedAt  int64  `json:"uploadedAt"`
}

const versionCols = `msg_id,cloud_path,kind,size,mtime,hash,fingerprint,uploaded_at`

func (s *Store) queryVersions(q string, args ...any) ([]Version, error) {
	rows, err := s.db.Query(`SELECT `+versionCols+` FROM versions `+q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Version
	for rows.Next() {
		var v Version
		if err := rows.Scan(&v.MsgID, &v.CloudPath, &v.Kind, &v.Size, &v.Mtime, &v.Hash, &v.Fingerprint, &v.UploadedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// AddVersion records an uploaded version.
func (s *Store) AddVersion(v Version) error {
	_, err := s.db.Exec(`INSERT OR REPLACE INTO versions(`+versionCols+`) VALUES(?,?,?,?,?,?,?,?)`,
		v.MsgID, v.CloudPath, v.Kind, v.Size, v.Mtime, v.Hash, v.Fingerprint, v.UploadedAt)
	return err
}

// Versions returns versions of a path, newest first.
func (s *Store) Versions(cloudPath string) ([]Version, error) {
	return s.queryVersions(`WHERE cloud_path=? ORDER BY uploaded_at DESC, msg_id DESC`, cloudPath)
}

// LatestVersion returns the newest version of a path.
func (s *Store) LatestVersion(cloudPath string) (Version, bool, error) {
	vs, err := s.queryVersions(`WHERE cloud_path=? ORDER BY uploaded_at DESC, msg_id DESC LIMIT 1`, cloudPath)
	if err != nil || len(vs) == 0 {
		return Version{}, false, err
	}
	return vs[0], true, nil
}

// Version returns a version by message ID.
func (s *Store) Version(msgID int) (Version, error) {
	vs, err := s.queryVersions(`WHERE msg_id=?`, msgID)
	if err != nil {
		return Version{}, err
	}
	if len(vs) == 0 {
		return Version{}, sql.ErrNoRows
	}
	return vs[0], nil
}

// LatestUnder returns the newest version of every path equal to or inside prefix ("" = all).
func (s *Store) LatestUnder(prefix string) ([]Version, error) {
	q := `WHERE msg_id IN (SELECT msg_id FROM versions v1 WHERE uploaded_at = (SELECT MAX(uploaded_at) FROM versions v2 WHERE v2.cloud_path=v1.cloud_path))`
	var args []any
	if prefix != "" {
		q += ` AND (cloud_path=? OR substr(cloud_path,1,?)=?)`
		args = append(args, prefix, len(prefix)+1, prefix+"/")
	}
	vs, err := s.queryVersions(q+` ORDER BY cloud_path, msg_id DESC`, args...)
	if err != nil {
		return nil, err
	}
	// Two versions can share a timestamp; keep the highest message ID per path.
	out := vs[:0]
	seen := map[string]bool{}
	for _, v := range vs {
		if !seen[v.CloudPath] {
			seen[v.CloudPath] = true
			out = append(out, v)
		}
	}
	return out, nil
}

// VersionsUnder returns every version (not just the latest) at or inside prefix.
func (s *Store) VersionsUnder(prefix string) ([]Version, error) {
	return s.queryVersions(`WHERE cloud_path=? OR substr(cloud_path,1,?)=? ORDER BY msg_id`, prefix, len(prefix)+1, prefix+"/")
}

// VersionCounts returns path -> number of stored versions.
func (s *Store) VersionCounts() (map[string]int, error) {
	rows, err := s.db.Query(`SELECT cloud_path, COUNT(*) FROM versions GROUP BY cloud_path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var p string
		var n int
		if err := rows.Scan(&p, &n); err != nil {
			return nil, err
		}
		out[p] = n
	}
	return out, rows.Err()
}

// DeleteVersions removes version rows.
func (s *Store) DeleteVersions(msgIDs []int) error {
	for _, id := range msgIDs {
		if _, err := s.db.Exec(`DELETE FROM versions WHERE msg_id=?`, id); err != nil {
			return err
		}
	}
	return nil
}

// AllVersionIDs returns every stored message ID.
func (s *Store) AllVersionIDs() (map[int]bool, error) {
	rows, err := s.db.Query(`SELECT msg_id FROM versions`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]bool{}
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// ClearCloud forgets all cloud versions and resume state (used on logout).
func (s *Store) ClearCloud() error {
	for _, q := range []string{`DELETE FROM versions`, `DELETE FROM resumes`, `UPDATE items SET state='pending', attempts=0, next_attempt=0 WHERE state!='skipped'`} {
		if _, err := s.db.Exec(q); err != nil {
			return err
		}
	}
	return nil
}

// RequeueMissing re-uploads items whose cloud copies were deleted in
// Telegram, and forgets cloud-only items that have no copies left.
func (s *Store) RequeueMissing() error {
	if _, err := s.db.Exec(`UPDATE items SET state=?, attempts=0, next_attempt=0
		WHERE state=? AND local_present=1 AND cloud_path NOT IN (SELECT cloud_path FROM versions)`,
		StatePending, StateSynced); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM items WHERE local_present=0 AND cloud_path NOT IN (SELECT cloud_path FROM versions)`)
	return err
}

// ---- resumable uploads ----

// Resume is saved progress of a large upload.
type Resume struct {
	Hash      string
	FileID    int64
	PartSize  int
	DoneParts int
}

// GetResume returns saved upload progress for a path.
func (s *Store) GetResume(cloudPath string) (Resume, bool) {
	var r Resume
	err := s.db.QueryRow(`SELECT hash,file_id,part_size,done_parts FROM resumes WHERE cloud_path=? AND updated_at>?`,
		cloudPath, now()-int64((20*time.Hour).Seconds())).Scan(&r.Hash, &r.FileID, &r.PartSize, &r.DoneParts)
	return r, err == nil
}

// SaveResume stores upload progress.
func (s *Store) SaveResume(cloudPath string, r Resume) error {
	_, err := s.db.Exec(`INSERT OR REPLACE INTO resumes(cloud_path,hash,file_id,part_size,done_parts,updated_at) VALUES(?,?,?,?,?,?)`,
		cloudPath, r.Hash, r.FileID, r.PartSize, r.DoneParts, now())
	return err
}

// ClearResume removes upload progress.
func (s *Store) ClearResume(cloudPath string) error {
	_, err := s.db.Exec(`DELETE FROM resumes WHERE cloud_path=?`, cloudPath)
	return err
}
