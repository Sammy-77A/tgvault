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
