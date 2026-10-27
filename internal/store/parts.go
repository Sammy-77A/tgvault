package store

// Files over Telegram's 2 GB limit are stored as several messages ("parts").
// A split version's ID is its first part's message ID; the parts table lists
// every part. Versions without rows here are a single message.
const partsSchema = `
CREATE TABLE IF NOT EXISTS parts(
	version_id INTEGER NOT NULL,
	idx INTEGER NOT NULL,
	msg_id INTEGER NOT NULL UNIQUE,
	off INTEGER NOT NULL,
	size INTEGER NOT NULL,
	hash TEXT NOT NULL,
	grp TEXT NOT NULL,
	PRIMARY KEY(version_id, idx)
);
CREATE TABLE IF NOT EXISTS part_uploads(
	cloud_path TEXT NOT NULL,
	grp TEXT NOT NULL,
	hash TEXT NOT NULL,
	idx INTEGER NOT NULL,
	msg_id INTEGER NOT NULL,
	off INTEGER NOT NULL,
	size INTEGER NOT NULL,
	part_hash TEXT NOT NULL,
	uploaded_at INTEGER NOT NULL,
	PRIMARY KEY(cloud_path, grp, idx)
);
`

// Part is one message of a split file.
type Part struct {
	VersionID int    `json:"-"`
	Index     int    `json:"index"` // from 1
	MsgID     int    `json:"msgId"`
	Offset    int64  `json:"offset"`
	Size      int64  `json:"size"`
	Hash      string `json:"-"`
	Group     string `json:"-"`
}

// AddSplitVersion records a version stored in several messages. v.MsgID must
// be the first part's message ID.
func (s *Store) AddSplitVersion(v Version, parts []Part) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT OR REPLACE INTO versions(`+versionCols+`) VALUES(`+versionMarks+`)`, versionArgs(v)...); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM parts WHERE version_id=?`, v.MsgID); err != nil {
		return err
	}
	for _, p := range parts {
		if _, err := tx.Exec(`INSERT OR REPLACE INTO parts(version_id,idx,msg_id,off,size,hash,grp) VALUES(?,?,?,?,?,?,?)`,
			v.MsgID, p.Index, p.MsgID, p.Offset, p.Size, p.Hash, p.Group); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Parts returns the parts of a version in order, or nil for a single-message version.
func (s *Store) Parts(versionID int) ([]Part, error) {
	rows, err := s.db.Query(`SELECT version_id,idx,msg_id,off,size,hash,grp FROM parts WHERE version_id=? ORDER BY idx`, versionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Part
	for rows.Next() {
		var p Part
		if err := rows.Scan(&p.VersionID, &p.Index, &p.MsgID, &p.Offset, &p.Size, &p.Hash, &p.Group); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// PartCounts returns version ID -> number of parts, for split versions only.
func (s *Store) PartCounts() (map[int]int, error) {
	rows, err := s.db.Query(`SELECT version_id, COUNT(*) FROM parts GROUP BY version_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]int{}
	for rows.Next() {
		var id, n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

// PartOwner returns the version a part message belongs to.
func (s *Store) PartOwner(msgID int) (int, bool) {
	var id int
	err := s.db.QueryRow(`SELECT version_id FROM parts WHERE msg_id=?`, msgID).Scan(&id)
	return id, err == nil
}

// PartUpload is a part already posted for a split upload that isn't finished.
type PartUpload struct {
	CloudPath  string
	Group      string
	Hash       string // the whole file's sha256
	Index      int
	MsgID      int
	Offset     int64
	Size       int64
	PartHash   string
	UploadedAt int64
}

// AddPartUpload records a posted part of an unfinished split upload.
func (s *Store) AddPartUpload(p PartUpload) error {
	_, err := s.db.Exec(`INSERT OR REPLACE INTO part_uploads(cloud_path,grp,hash,idx,msg_id,off,size,part_hash,uploaded_at)
		VALUES(?,?,?,?,?,?,?,?,?)`, p.CloudPath, p.Group, p.Hash, p.Index, p.MsgID, p.Offset, p.Size, p.PartHash, p.UploadedAt)
	return err
}

// PartUploads returns posted parts of unfinished uploads of a path ("" = every path).
func (s *Store) PartUploads(cloudPath string) ([]PartUpload, error) {
	q := `SELECT cloud_path,grp,hash,idx,msg_id,off,size,part_hash,uploaded_at FROM part_uploads`
	var args []any
	if cloudPath != "" {
		q += ` WHERE cloud_path=?`
		args = append(args, cloudPath)
	}
	rows, err := s.db.Query(q+` ORDER BY cloud_path, grp, idx`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PartUpload
	for rows.Next() {
		var p PartUpload
		if err := rows.Scan(&p.CloudPath, &p.Group, &p.Hash, &p.Index, &p.MsgID, &p.Offset, &p.Size, &p.PartHash, &p.UploadedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// DeletePartUploads forgets the posted parts of one unfinished upload.
func (s *Store) DeletePartUploads(cloudPath, group string) error {
	_, err := s.db.Exec(`DELETE FROM part_uploads WHERE cloud_path=? AND grp=?`, cloudPath, group)
	return err
}

// PartUploadIDs returns the message IDs of every part of unfinished uploads.
func (s *Store) PartUploadIDs() (map[int]bool, error) {
	rows, err := s.db.Query(`SELECT msg_id FROM part_uploads`)
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

// RequeueTooBig queues files that were skipped for being over 2 GB, now that
// they can be split.
func (s *Store) RequeueTooBig() error {
	_, err := s.db.Exec(`UPDATE items SET state=?, note='', attempts=0, next_attempt=0
		WHERE state=? AND (note LIKE 'over 2 GB%' OR note LIKE 'the project ZIP is over 2 GB%')`, StatePending, StateSkipped)
	return err
}
