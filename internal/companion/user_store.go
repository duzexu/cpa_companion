package companion

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

func openUsersDB(path string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, statement := range []string{
		`PRAGMA busy_timeout=5000`,
		`CREATE TABLE IF NOT EXISTS users (
			id TEXT PRIMARY KEY,
			username TEXT NOT NULL DEFAULT '',
			name TEXT NOT NULL DEFAULT '',
			avatar_url TEXT NOT NULL DEFAULT '',
			plan_id TEXT NOT NULL DEFAULT '',
			reset_used_json TEXT NOT NULL DEFAULT 'null',
			disabled INTEGER NOT NULL DEFAULT 0,
			session_version INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS users_plan_id ON users(plan_id)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			db.Close()
			return nil, fmt.Errorf("initialize users database: %w", err)
		}
	}
	if err := os.Chmod(path, 0600); err != nil {
		db.Close()
		return nil, fmt.Errorf("secure users database: %w", err)
	}
	return db, nil
}

func writeUser(tx *sql.Tx, u user) error {
	resetUsed, err := json.Marshal(u.ResetUsed)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO users
		(id,username,name,avatar_url,plan_id,reset_used_json,disabled,session_version)
		VALUES (?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET
		username=excluded.username,name=excluded.name,avatar_url=excluded.avatar_url,
		plan_id=excluded.plan_id,reset_used_json=excluded.reset_used_json,
		disabled=excluded.disabled,session_version=excluded.session_version`,
		u.ID, u.Username, u.Name, u.AvatarURL, u.PlanID, string(resetUsed), u.Disabled, u.SessionVersion)
	return err
}

func (s *store) loadUsers() error {
	rows, err := s.usersDB.Query(`SELECT id,username,name,avatar_url,plan_id,reset_used_json,disabled,session_version FROM users`)
	if err != nil {
		return err
	}
	defer rows.Close()
	s.db.Users = make(map[string]user)
	for rows.Next() {
		var u user
		var resetUsed string
		if err := rows.Scan(&u.ID, &u.Username, &u.Name, &u.AvatarURL, &u.PlanID, &resetUsed, &u.Disabled, &u.SessionVersion); err != nil {
			return err
		}
		if err := json.Unmarshal([]byte(resetUsed), &u.ResetUsed); err != nil {
			return fmt.Errorf("decode user %q reset usage: %w", u.ID, err)
		}
		s.db.Users[u.ID] = u
	}
	return rows.Err()
}

func (s *store) saveUserLocked(u user) error {
	return s.saveUsersLocked([]user{u})
}

// The in-memory map is updated only after SQLite commits, so failed writes
// cannot authorize a plan assignment or session-version change.
func (s *store) saveUsersLocked(users []user) error {
	if len(users) == 0 {
		return nil
	}
	tx, err := s.usersDB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, u := range users {
		if err := writeUser(tx, u); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for _, u := range users {
		s.db.Users[u.ID] = u
	}
	return nil
}

func (s *store) Close() error { return s.usersDB.Close() }
