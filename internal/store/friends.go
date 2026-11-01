package store

import (
	"database/sql"
	"errors"
)

const friendsSchema = `
CREATE TABLE IF NOT EXISTS friends(
	name TEXT PRIMARY KEY,
	key_hash TEXT NOT NULL UNIQUE,
	quota INTEGER NOT NULL DEFAULT 0,
	disabled INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS shares(
	token_hash TEXT PRIMARY KEY,
	friend TEXT NOT NULL,
	cloud_path TEXT NOT NULL,
	expires_at INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL,
	downloads INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS shares_friend ON shares(friend);
`

// ErrFriendExists is returned when a friend's name is already taken.
var ErrFriendExists = errors.New("a friend with that name already exists")

// Friend is a person allowed to use the server's file API.
type Friend struct {
	Name     string `json:"name"`
	Quota    int64  `json:"quota"` // bytes; 0 means no limit
	Disabled bool   `json:"disabled"`
	Created  int64  `json:"created"`
}

// Share is a link that lets anyone download one file.
type Share struct {
	TokenHash string `json:"-"`
	Friend    string `json:"friend"`
	CloudPath string `json:"cloudPath"`
	Expires   int64  `json:"expires"` // unix seconds; 0 means never
	Created   int64  `json:"created"`
	Downloads int    `json:"downloads"`
}

// AddFriend creates a friend. Only the hash of the API key is stored.
func (s *Store) AddFriend(name, keyHash string, quota int64) error {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM friends WHERE name=?`, name).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return ErrFriendExists
	}
	_, err := s.db.Exec(`INSERT INTO friends(name,key_hash,quota,created_at) VALUES(?,?,?,?)`, name, keyHash, quota, now())
	return err
}

// Friends lists everyone, by name.
func (s *Store) Friends() ([]Friend, error) {
	rows, err := s.db.Query(`SELECT name,quota,disabled,created_at FROM friends ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Friend
	for rows.Next() {
		var f Friend
		if err := rows.Scan(&f.Name, &f.Quota, &f.Disabled, &f.Created); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// FriendByKey finds the friend an API key hash belongs to.
func (s *Store) FriendByKey(keyHash string) (Friend, bool) {
	var f Friend
	err := s.db.QueryRow(`SELECT name,quota,disabled,created_at FROM friends WHERE key_hash=?`, keyHash).
		Scan(&f.Name, &f.Quota, &f.Disabled, &f.Created)
	return f, err == nil
}

// FriendByName finds a friend by name.
func (s *Store) FriendByName(name string) (Friend, bool) {
	var f Friend
	err := s.db.QueryRow(`SELECT name,quota,disabled,created_at FROM friends WHERE name=?`, name).
		Scan(&f.Name, &f.Quota, &f.Disabled, &f.Created)
	return f, err == nil
}

func (s *Store) friendUpdate(name, q string, args ...any) error {
	res, err := s.db.Exec(q, append(args, name)...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// SetFriendQuota changes how many bytes a friend may store (0 = no limit).
func (s *Store) SetFriendQuota(name string, quota int64) error {
	return s.friendUpdate(name, `UPDATE friends SET quota=? WHERE name=?`, quota)
}

// SetFriendDisabled blocks or unblocks a friend without deleting anything.
func (s *Store) SetFriendDisabled(name string, disabled bool) error {
	return s.friendUpdate(name, `UPDATE friends SET disabled=? WHERE name=?`, disabled)
}

// SetFriendKey replaces a friend's API key.
func (s *Store) SetFriendKey(name, keyHash string) error {
	return s.friendUpdate(name, `UPDATE friends SET key_hash=? WHERE name=?`, keyHash)
}

// RemoveFriend deletes a friend and their share links. Their files stay in Telegram.
func (s *Store) RemoveFriend(name string) error {
	if _, err := s.db.Exec(`DELETE FROM shares WHERE friend=?`, name); err != nil {
		return err
	}
	return s.friendUpdate(name, `DELETE FROM friends WHERE name=?`)
}

// AddShare records a share link; only the hash of its token is stored.
func (s *Store) AddShare(sh Share) error {
	_, err := s.db.Exec(`INSERT INTO shares(token_hash,friend,cloud_path,expires_at,created_at) VALUES(?,?,?,?,?)`,
		sh.TokenHash, sh.Friend, sh.CloudPath, sh.Expires, now())
	return err
}

// ShareByToken looks a link up by its token hash.
func (s *Store) ShareByToken(tokenHash string) (Share, bool) {
	var sh Share
	err := s.db.QueryRow(`SELECT token_hash,friend,cloud_path,expires_at,created_at,downloads FROM shares WHERE token_hash=?`, tokenHash).
		Scan(&sh.TokenHash, &sh.Friend, &sh.CloudPath, &sh.Expires, &sh.Created, &sh.Downloads)
	return sh, err == nil
}

// SharesOf lists a friend's links, newest first.
func (s *Store) SharesOf(friend string) ([]Share, error) {
	rows, err := s.db.Query(`SELECT token_hash,friend,cloud_path,expires_at,created_at,downloads FROM shares WHERE friend=? ORDER BY created_at DESC`, friend)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Share
	for rows.Next() {
		var sh Share
		if err := rows.Scan(&sh.TokenHash, &sh.Friend, &sh.CloudPath, &sh.Expires, &sh.Created, &sh.Downloads); err != nil {
			return nil, err
		}
		out = append(out, sh)
	}
	return out, rows.Err()
}

// DeleteShare removes one of a friend's links.
func (s *Store) DeleteShare(friend, tokenHash string) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM shares WHERE friend=? AND token_hash=?`, friend, tokenHash)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// CountShareDownload notes that a link was used.
func (s *Store) CountShareDownload(tokenHash string) {
	_, _ = s.db.Exec(`UPDATE shares SET downloads=downloads+1 WHERE token_hash=?`, tokenHash)
}

// FindOrAddRoot returns the root with exactly this label, adding it if needed.
func (s *Store) FindOrAddRoot(path, label string) (Root, error) {
	var id int64
	err := s.db.QueryRow(`SELECT id FROM roots WHERE label=?`, label).Scan(&id)
	if err == nil {
		return s.Root(id)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Root{}, err
	}
	return s.AddRoot(path, label)
}
