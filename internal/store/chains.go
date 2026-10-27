package store

import "database/sql"

// A project is backed up as a full ZIP now and then, and as change ZIPs in
// between (only the files that changed). A full ZIP and the change ZIPs after
// it form a chain; each change ZIP is a version whose base is the full ZIP and
// whose prev is the piece before it.
const chainsSchema = `
CREATE TABLE IF NOT EXISTS project_files(
	cloud_path TEXT NOT NULL,
	rel TEXT NOT NULL,
	size INTEGER NOT NULL,
	mtime INTEGER NOT NULL,
	hash TEXT NOT NULL,
	PRIMARY KEY(cloud_path, rel)
);
CREATE TABLE IF NOT EXISTS project_state(
	cloud_path TEXT PRIMARY KEY,
	version INTEGER NOT NULL,
	base INTEGER NOT NULL,
	seq INTEGER NOT NULL,
	chain_bytes INTEGER NOT NULL,
	base_size INTEGER NOT NULL,
	base_at INTEGER NOT NULL,
	fingerprint TEXT NOT NULL,
	listed_at INTEGER NOT NULL DEFAULT 0
);
`

// chainColumns are added to versions by Open.
var chainColumns = []string{
	`ALTER TABLE versions ADD COLUMN base INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE versions ADD COLUMN prev INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE versions ADD COLUMN seq INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE versions ADD COLUMN tree TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE versions ADD COLUMN changed INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE versions ADD COLUMN deleted INTEGER NOT NULL DEFAULT 0`,
}

// ProjectFile is one file of a project as of its last backup from this device.
type ProjectFile struct {
	Rel   string
	Size  int64
	Mtime int64
	Hash  string
}

// ProjectState is where a project's chain stands after its last backup from
// this device.
type ProjectState struct {
	CloudPath   string
	Version     int    // the last version uploaded (the next change's prev)
	Base        int    // the chain's full ZIP
	Seq         int    // 0 right after a full ZIP
	ChainBytes  int64  // size of the change ZIPs since the full ZIP
	BaseSize    int64  // size of the full ZIP
	BaseAt      int64  // when the full ZIP was uploaded
	Fingerprint string // project fingerprint at that backup ("" if files were left out)
	ListedAt    int64  // when that backup listed the project's files
}

// GetProjectState returns a project's chain state.
func (s *Store) GetProjectState(cloudPath string) (ProjectState, bool) {
	ps := ProjectState{CloudPath: cloudPath}
	err := s.db.QueryRow(`SELECT version,base,seq,chain_bytes,base_size,base_at,fingerprint,listed_at FROM project_state WHERE cloud_path=?`,
		cloudPath).Scan(&ps.Version, &ps.Base, &ps.Seq, &ps.ChainBytes, &ps.BaseSize, &ps.BaseAt, &ps.Fingerprint, &ps.ListedAt)
	return ps, err == nil
}

// ProjectFiles returns a project's files as of its last backup, by path.
func (s *Store) ProjectFiles(cloudPath string) (map[string]ProjectFile, error) {
	rows, err := s.db.Query(`SELECT rel,size,mtime,hash FROM project_files WHERE cloud_path=?`, cloudPath)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]ProjectFile{}
	for rows.Next() {
		var f ProjectFile
		if err := rows.Scan(&f.Rel, &f.Size, &f.Mtime, &f.Hash); err != nil {
			return nil, err
		}
		out[f.Rel] = f
	}
	return out, rows.Err()
}

// SaveProjectState records a backup of a project: its chain state and its
// files. With all, files is the complete list (after a full ZIP); otherwise
// files are added or updated and deleted ones removed.
func (s *Store) SaveProjectState(ps ProjectState, files []ProjectFile, deleted []string, all bool) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if all {
		if _, err := tx.Exec(`DELETE FROM project_files WHERE cloud_path=?`, ps.CloudPath); err != nil {
			return err
		}
	}
	for _, f := range files {
		if _, err := tx.Exec(`INSERT OR REPLACE INTO project_files(cloud_path,rel,size,mtime,hash) VALUES(?,?,?,?,?)`,
			ps.CloudPath, f.Rel, f.Size, f.Mtime, f.Hash); err != nil {
			return err
		}
	}
	for _, rel := range deleted {
		if _, err := tx.Exec(`DELETE FROM project_files WHERE cloud_path=? AND rel=?`, ps.CloudPath, rel); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO project_state(cloud_path,version,base,seq,chain_bytes,base_size,base_at,fingerprint,listed_at)
		VALUES(?,?,?,?,?,?,?,?,?)`, ps.CloudPath, ps.Version, ps.Base, ps.Seq, ps.ChainBytes, ps.BaseSize, ps.BaseAt, ps.Fingerprint, ps.ListedAt); err != nil {
		return err
	}
	return tx.Commit()
}

// SetVersionFingerprint updates a version's project fingerprint (after a
// backup found only modified times had moved).
func (s *Store) SetVersionFingerprint(msgID int, fp string) error {
	_, err := s.db.Exec(`UPDATE versions SET fingerprint=? WHERE msg_id=?`, fp, msgID)
	return err
}

// ForgetProjectState drops a project's chain state, so its next backup is a
// full ZIP.
func (s *Store) ForgetProjectState(cloudPath string) error {
	_, err := s.db.Exec(`DELETE FROM project_state WHERE cloud_path=?`, cloudPath)
	return err
}

// BrokenChanges returns change versions that can't be rebuilt because their
// full ZIP, or a piece before them, is no longer recorded.
func (s *Store) BrokenChanges() ([]Version, error) {
	changes, err := s.queryVersions(`WHERE seq>0 ORDER BY seq, msg_id`)
	if err != nil {
		return nil, err
	}
	fulls, err := s.queryVersions(`WHERE seq=0 AND msg_id IN (SELECT base FROM versions WHERE seq>0)`)
	if err != nil {
		return nil, err
	}
	// ok holds the pieces a later change can build on: full ZIPs, and changes
	// whose own links check out (judged in seq order, so the piece before
	// each change has been judged already). The links are those ChainOf follows.
	ok := map[int]Version{}
	for _, v := range fulls {
		ok[v.MsgID] = v
	}
	var out []Version
	for _, v := range changes {
		prev, found := ok[v.Prev]
		_, baseOK := ok[v.Base]
		good := found && baseOK && prev.Seq == v.Seq-1 &&
			((prev.Seq == 0 && prev.MsgID == v.Base) || (prev.Seq > 0 && prev.Base == v.Base))
		if good {
			ok[v.MsgID] = v
		} else {
			out = append(out, v)
		}
	}
	return out, nil
}

// ChainOf returns the pieces needed to rebuild version v, oldest first: the
// full ZIP, then each change up to v. For a full ZIP it's just v.
func (s *Store) ChainOf(v Version) ([]Version, error) {
	chain := []Version{v}
	for cur := v; cur.Seq > 0; {
		prev, err := s.Version(cur.Prev)
		if err != nil || prev.Seq != cur.Seq-1 || (prev.Seq > 0 && prev.Base != v.Base) || (prev.Seq == 0 && prev.MsgID != v.Base) {
			return nil, sql.ErrNoRows
		}
		chain = append(chain, prev)
		cur = prev
	}
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	return chain, nil
}
