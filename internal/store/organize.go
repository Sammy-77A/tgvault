package store

import "strings"

// Folders made on purpose, so an empty one still shows up in the file
// browser. Folders with files in them need no row: their path says it all.
const foldersSchema = `
CREATE TABLE IF NOT EXISTS folders(
	cloud_path TEXT PRIMARY KEY
);
`

// AddFolder records an empty folder.
func (s *Store) AddFolder(cloudPath string) error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO folders(cloud_path) VALUES(?)`, cloudPath)
	return err
}

// FoldersUnder lists recorded folders inside prefix (not prefix itself).
func (s *Store) FoldersUnder(prefix string) ([]string, error) {
	q, args := `SELECT cloud_path FROM folders`, []any(nil)
	if prefix != "" {
		q += ` WHERE substr(cloud_path,1,?)=?`
		args = append(args, len(prefix)+1, prefix+"/")
	}
	rows, err := s.db.Query(q+` ORDER BY cloud_path`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// FoldersAt lists recorded folders equal to prefix or inside it.
func (s *Store) FoldersAt(prefix string) ([]string, error) {
	in, err := s.FoldersUnder(prefix)
	if err != nil {
		return nil, err
	}
	var own int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM folders WHERE cloud_path=?`, prefix).Scan(&own); err != nil {
		return nil, err
	}
	if own > 0 {
		in = append([]string{prefix}, in...)
	}
	return in, nil
}

// MoveFolders renames the recorded folder from and everything recorded inside.
func (s *Store) MoveFolders(from, to string) error {
	list, err := s.FoldersAt(from)
	if err != nil {
		return err
	}
	for _, p := range list {
		if _, err := s.db.Exec(`DELETE FROM folders WHERE cloud_path=?`, p); err != nil {
			return err
		}
		if err := s.AddFolder(to + strings.TrimPrefix(p, from)); err != nil {
			return err
		}
	}
	return nil
}

// DropFolders forgets the recorded folder prefix and everything inside it.
func (s *Store) DropFolders(prefix string) error {
	_, err := s.db.Exec(`DELETE FROM folders WHERE cloud_path=? OR substr(cloud_path,1,?)=?`,
		prefix, len(prefix)+1, prefix+"/")
	return err
}

// MoveItem gives a tracked file or project its new cloud path and place on
// this device, after it was moved there. A leftover entry for a file that
// used to be at the new path is dropped.
func (s *Store) MoveItem(id int64, cloudPath string, rootID int64, rel string) error {
	if _, err := s.db.Exec(`DELETE FROM items WHERE cloud_path=? AND local_present=0 AND id!=?`, cloudPath, id); err != nil {
		return err
	}
	_, err := s.db.Exec(`UPDATE items SET cloud_path=?,root_id=?,rel=? WHERE id=?`, cloudPath, rootID, rel, id)
	return err
}

// MoveProject carries a project's backup state along when its folder moves.
func (s *Store) MoveProject(from, to string, fromRoot int64, fromRel string, toRoot int64, toRel string) error {
	for _, q := range []string{
		`UPDATE OR REPLACE project_state SET cloud_path=? WHERE cloud_path=?`,
		`UPDATE OR REPLACE project_files SET cloud_path=? WHERE cloud_path=?`,
	} {
		if _, err := s.db.Exec(q, to, from); err != nil {
			return err
		}
	}
	_, err := s.db.Exec(`UPDATE OR REPLACE projects SET root_id=?,rel=? WHERE root_id=? AND rel=?`, toRoot, toRel, fromRoot, fromRel)
	return err
}
