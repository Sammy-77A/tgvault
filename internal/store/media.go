package store

// Files in the movie library that the organizer couldn't place with
// confidence, and files the owner chose to leave alone. The body is the
// organizer's own JSON (what it guessed and the candidates it found).
const mediaSchema = `
CREATE TABLE IF NOT EXISTS media_review(
	cloud_path TEXT PRIMARY KEY,
	state TEXT NOT NULL,      -- 'review' or 'skipped'
	reason TEXT NOT NULL DEFAULT '',
	body TEXT NOT NULL DEFAULT '',
	at INTEGER NOT NULL DEFAULT 0
);
-- what the owner decided a name in file names means, so that one answer
-- settles every file named that way
CREATE TABLE IF NOT EXISTS media_alias(
	norm TEXT NOT NULL,
	kind TEXT NOT NULL,       -- 'movie' or 'tv'
	tmdb_id INTEGER NOT NULL,
	title TEXT NOT NULL,
	year INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY(norm, kind)
);
-- what the organizer did, for the owner to read
CREATE TABLE IF NOT EXISTS media_log(
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	at INTEGER NOT NULL,
	kind TEXT NOT NULL,       -- sorted, review, failed, duplicate, request
	path TEXT NOT NULL DEFAULT '',
	detail TEXT NOT NULL DEFAULT ''
);
-- what is filed in the library, by TMDB id, to answer requests
CREATE TABLE IF NOT EXISTS media_filed(
	path TEXT PRIMARY KEY,
	kind TEXT NOT NULL,       -- movie or episode
	tmdb INTEGER NOT NULL,
	season INTEGER NOT NULL DEFAULT 0,
	episodes TEXT NOT NULL DEFAULT ''   -- "1,2"
);
-- the download queue of the movie app: what is wanted on this device, and how far each got
CREATE TABLE IF NOT EXISTS mb_downloads(
	path TEXT PRIMARY KEY,
	msg_id INTEGER NOT NULL DEFAULT 0,
	size INTEGER NOT NULL DEFAULT 0,
	state TEXT NOT NULL,      -- queued, running, paused, failed, done
	done INTEGER NOT NULL DEFAULT 0,
	err TEXT NOT NULL DEFAULT '',
	attempts INTEGER NOT NULL DEFAULT 0,
	next INTEGER NOT NULL DEFAULT 0,   -- not before this time (backoff)
	added INTEGER NOT NULL DEFAULT 0
);
-- requests for a movie or show, as posted to Saved Messages
CREATE TABLE IF NOT EXISTS media_requests(
	id TEXT PRIMARY KEY,
	msg_id INTEGER NOT NULL DEFAULT 0,
	state TEXT NOT NULL,      -- 'requested', 'fulfilled' or 'declined'
	body TEXT NOT NULL,
	at INTEGER NOT NULL DEFAULT 0
);
`

// MediaNote is one row of media_review.
type MediaNote struct {
	CloudPath string `json:"cloudPath"`
	State     string `json:"state"`
	Reason    string `json:"reason"`
	Body      string `json:"body"`
	At        int64  `json:"at"`
}

// MediaPut records (or replaces) the organizer's note about a file.
func (s *Store) MediaPut(cloudPath, state, reason, body string) error {
	_, err := s.db.Exec(`INSERT INTO media_review(cloud_path,state,reason,body,at) VALUES(?,?,?,?,?)
		ON CONFLICT(cloud_path) DO UPDATE SET state=excluded.state, reason=excluded.reason, body=excluded.body, at=excluded.at`,
		cloudPath, state, reason, body, now())
	return err
}

// MediaDrop forgets the note about a file.
func (s *Store) MediaDrop(cloudPath string) error {
	_, err := s.db.Exec(`DELETE FROM media_review WHERE cloud_path=?`, cloudPath)
	return err
}

// MediaNotes lists notes of one state (” for all), oldest first.
func (s *Store) MediaNotes(state string) ([]MediaNote, error) {
	q, args := `SELECT cloud_path,state,reason,body,at FROM media_review`, []any(nil)
	if state != "" {
		q += ` WHERE state=?`
		args = append(args, state)
	}
	rows, err := s.db.Query(q+` ORDER BY at, cloud_path`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MediaNote
	for rows.Next() {
		var n MediaNote
		if err := rows.Scan(&n.CloudPath, &n.State, &n.Reason, &n.Body, &n.At); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// MediaNote returns the note about one file.
func (s *Store) MediaNote(cloudPath string) (MediaNote, bool) {
	var n MediaNote
	err := s.db.QueryRow(`SELECT cloud_path,state,reason,body,at FROM media_review WHERE cloud_path=?`, cloudPath).
		Scan(&n.CloudPath, &n.State, &n.Reason, &n.Body, &n.At)
	return n, err == nil
}

// MediaMove carries notes along when files or folders are moved.
func (s *Store) MediaMove(from, to string) error {
	_, err := s.db.Exec(`UPDATE OR REPLACE media_review SET cloud_path=? WHERE cloud_path=?`, to, from)
	return err
}

// MediaAlias is a remembered answer: file names reading Norm are this title.
type MediaAlias struct {
	Norm   string
	Kind   string
	TMDBID int
	Title  string
	Year   int
}

// MediaSetAlias remembers what a name means.
func (s *Store) MediaSetAlias(a MediaAlias) error {
	_, err := s.db.Exec(`INSERT INTO media_alias(norm,kind,tmdb_id,title,year) VALUES(?,?,?,?,?)
		ON CONFLICT(norm,kind) DO UPDATE SET tmdb_id=excluded.tmdb_id, title=excluded.title, year=excluded.year`,
		a.Norm, a.Kind, a.TMDBID, a.Title, a.Year)
	return err
}

// MediaGetAlias returns the remembered answer for a name, if any.
func (s *Store) MediaGetAlias(norm, kind string) (MediaAlias, bool) {
	a := MediaAlias{Norm: norm, Kind: kind}
	err := s.db.QueryRow(`SELECT tmdb_id,title,year FROM media_alias WHERE norm=? AND kind=?`, norm, kind).
		Scan(&a.TMDBID, &a.Title, &a.Year)
	return a, err == nil
}

// MediaRequest is one row of media_requests; Body is the request's JSON.
type MediaRequest struct {
	ID    string
	MsgID int
	State string
	Body  string
	At    int64
}

// MediaReqPut adds a request, or updates the one with that ID. It reports
// whether the request is new.
func (s *Store) MediaReqPut(r MediaRequest) (bool, error) {
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM media_requests WHERE id=?`, r.ID).Scan(&n)
	if n == 0 {
		_, err := s.db.Exec(`INSERT INTO media_requests(id,msg_id,state,body,at) VALUES(?,?,?,?,?)`,
			r.ID, r.MsgID, r.State, r.Body, r.At)
		return true, err
	}
	_, err := s.db.Exec(`UPDATE media_requests SET msg_id=CASE WHEN ?>0 THEN ? ELSE msg_id END,
		body=CASE WHEN ?<>'' THEN ? ELSE body END WHERE id=?`, r.MsgID, r.MsgID, r.Body, r.Body, r.ID)
	return false, err
}

// MediaReqState sets a request's state.
func (s *Store) MediaReqState(id, state string) error {
	_, err := s.db.Exec(`UPDATE media_requests SET state=? WHERE id=?`, state, id)
	return err
}

// MediaReqs lists requests, newest first.
func (s *Store) MediaReqs() ([]MediaRequest, error) {
	rows, err := s.db.Query(`SELECT id,msg_id,state,body,at FROM media_requests ORDER BY at DESC, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MediaRequest
	for rows.Next() {
		var r MediaRequest
		if err := rows.Scan(&r.ID, &r.MsgID, &r.State, &r.Body, &r.At); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// MediaReq returns one request.
func (s *Store) MediaReq(id string) (MediaRequest, bool) {
	var r MediaRequest
	err := s.db.QueryRow(`SELECT id,msg_id,state,body,at FROM media_requests WHERE id=?`, id).
		Scan(&r.ID, &r.MsgID, &r.State, &r.Body, &r.At)
	return r, err == nil
}

// MediaReqsClear forgets requests that have been answered.
func (s *Store) MediaReqsClear() (int64, error) {
	r, err := s.db.Exec(`DELETE FROM media_requests WHERE state<>'requested'`)
	if err != nil {
		return 0, err
	}
	return r.RowsAffected()
}

// MediaReqSent marks a queued request as posted, with its message ID.
func (s *Store) MediaReqSent(id string, msgID int) error {
	_, err := s.db.Exec(`UPDATE media_requests SET state='requested', msg_id=? WHERE id=? AND state='queued'`, msgID, id)
	return err
}

// MBDownload is one row of the download queue.
type MBDownload struct {
	Path     string `json:"path"`
	MsgID    int    `json:"msgId"`
	Size     int64  `json:"size"`
	State    string `json:"state"`
	Done     int64  `json:"done"`
	Err      string `json:"error,omitempty"`
	Attempts int    `json:"attempts"`
	Next     int64  `json:"next,omitempty"`
	Added    int64  `json:"added"`
}

// DLUpsert adds a download, or sets the wanted file of an existing one.
func (s *Store) DLUpsert(d MBDownload) error {
	_, err := s.db.Exec(`INSERT INTO mb_downloads(path,msg_id,size,state,done,err,attempts,next,added) VALUES(?,?,?,?,?,?,?,?,?)
		ON CONFLICT(path) DO UPDATE SET msg_id=excluded.msg_id, size=excluded.size, state=excluded.state,
		done=excluded.done, err=excluded.err, attempts=excluded.attempts, next=excluded.next`,
		d.Path, d.MsgID, d.Size, d.State, d.Done, d.Err, d.Attempts, d.Next, now())
	return err
}

// DLSet changes the progress fields of a download.
func (s *Store) DLSet(path, state string, done int64, errText string, attempts int, next int64) error {
	_, err := s.db.Exec(`UPDATE mb_downloads SET state=?, done=?, err=?, attempts=?, next=? WHERE path=?`,
		state, done, errText, attempts, next, path)
	return err
}

// DLList returns every download, oldest first.
func (s *Store) DLList() ([]MBDownload, error) {
	rows, err := s.db.Query(`SELECT path,msg_id,size,state,done,err,attempts,next,added FROM mb_downloads ORDER BY added, path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MBDownload
	for rows.Next() {
		var d MBDownload
		if err := rows.Scan(&d.Path, &d.MsgID, &d.Size, &d.State, &d.Done, &d.Err, &d.Attempts, &d.Next, &d.Added); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// DLGet returns one download.
func (s *Store) DLGet(path string) (MBDownload, bool) {
	var d MBDownload
	err := s.db.QueryRow(`SELECT path,msg_id,size,state,done,err,attempts,next,added FROM mb_downloads WHERE path=?`, path).
		Scan(&d.Path, &d.MsgID, &d.Size, &d.State, &d.Done, &d.Err, &d.Attempts, &d.Next, &d.Added)
	return d, err == nil
}

// DLDelete forgets a download.
func (s *Store) DLDelete(path string) error {
	_, err := s.db.Exec(`DELETE FROM mb_downloads WHERE path=?`, path)
	return err
}

// DLResetRunning puts downloads that were running when the app stopped back in the queue.
func (s *Store) DLResetRunning() error {
	_, err := s.db.Exec(`UPDATE mb_downloads SET state='queued' WHERE state='running'`)
	return err
}

// MediaLogEntry is one line of the organizer's log.
type MediaLogEntry struct {
	ID     int64  `json:"id"`
	At     int64  `json:"at"`
	Kind   string `json:"kind"`
	Path   string `json:"path"`
	Detail string `json:"detail"`
}

// MediaLogAdd records something the organizer did; the log keeps the last 500.
func (s *Store) MediaLogAdd(kind, path, detail string) {
	_, _ = s.db.Exec(`INSERT INTO media_log(at,kind,path,detail) VALUES(?,?,?,?)`, now(), kind, path, detail)
	_, _ = s.db.Exec(`DELETE FROM media_log WHERE id <= (SELECT MAX(id) FROM media_log) - 500`)
}

// MediaLogList returns the newest entries first.
func (s *Store) MediaLogList(limit int) ([]MediaLogEntry, error) {
	rows, err := s.db.Query(`SELECT id,at,kind,path,detail FROM media_log ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MediaLogEntry
	for rows.Next() {
		var e MediaLogEntry
		if err := rows.Scan(&e.ID, &e.At, &e.Kind, &e.Path, &e.Detail); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// MediaFiled is a file the organizer placed in the library.
type MediaFiled struct {
	Path     string
	Kind     string
	TMDB     int
	Season   int
	Episodes string
}

// MediaFiledPut remembers what a library file is.
func (s *Store) MediaFiledPut(f MediaFiled) error {
	_, err := s.db.Exec(`INSERT INTO media_filed(path,kind,tmdb,season,episodes) VALUES(?,?,?,?,?)
		ON CONFLICT(path) DO UPDATE SET kind=excluded.kind, tmdb=excluded.tmdb, season=excluded.season, episodes=excluded.episodes`,
		f.Path, f.Kind, f.TMDB, f.Season, f.Episodes)
	return err
}

// MediaFiledAll lists everything filed.
func (s *Store) MediaFiledAll() ([]MediaFiled, error) {
	rows, err := s.db.Query(`SELECT path,kind,tmdb,season,episodes FROM media_filed`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MediaFiled
	for rows.Next() {
		var f MediaFiled
		if err := rows.Scan(&f.Path, &f.Kind, &f.TMDB, &f.Season, &f.Episodes); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// MediaFiledDrop forgets a file that is gone.
func (s *Store) MediaFiledDrop(path string) error {
	_, err := s.db.Exec(`DELETE FROM media_filed WHERE path=?`, path)
	return err
}
