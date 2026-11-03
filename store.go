package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Save is one save-data directory in one of an account's folders (ps1, ps2, ps3, psp, minis).
type Save struct {
	Owner     string // core account id
	Folder    string
	Name      string // save directory name, < 32 chars
	OwnerInfo int
	Title     string
	Subtitle  string
	Details   string
	Updated   time.Time
	Lock      int
	Size      int64 // save size as the console reports it
	Length    int64 // bytes of the uploaded data part
	IconID    string
	Date      time.Time // upload time
}

// FolderInfo is the per-folder summary every reply carries.
type FolderInfo struct {
	Total     int
	TotalSize int64
	Seq       int64
}

var (
	ErrNotFound = errors.New("no such save")
	ErrFull     = errors.New("account quota exceeded")
)

const chunkSize = 4 << 20

type Store interface {
	// Usage is the sum of Length over all of owner's saves.
	Usage(ctx context.Context, owner string) (int64, error)
	Folder(ctx context.Context, owner, folder string) (FolderInfo, error)
	// List returns a folder's saves ordered by name.
	List(ctx context.Context, owner, folder string, offset, limit int) ([]Save, error)
	Get(ctx context.Context, owner, folder, name string) (Save, error)
	// Put replaces (owner, folder, name). It reads exactly s.Length bytes from data, calls verify
	// once they are read and keeps the save only if verify returns nil and the owner's total
	// (minus the save it replaces) stays within quota.
	Put(ctx context.Context, s Save, icon []byte, data io.Reader, verify func() error, quota int64) error
	WriteData(ctx context.Context, owner, folder, name string, w io.Writer) error
	Delete(ctx context.Context, owner, folder, name string) error
	Icon(ctx context.Context, id string) ([]byte, error)
}

type memSave struct {
	Save
	icon, data []byte
}

// MemStore is the in-memory Store used by tests.
type MemStore struct {
	mu   sync.Mutex
	m    map[[3]string]memSave
	seqs map[[2]string]int64
}

func (s *MemStore) usage(owner string) (n int64) {
	for k, v := range s.m {
		if k[0] == owner {
			n += v.Length
		}
	}
	return n
}

func (s *MemStore) Usage(_ context.Context, owner string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.usage(owner), nil
}

func (s *MemStore) Folder(_ context.Context, owner, folder string) (FolderInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := FolderInfo{Seq: s.seqs[[2]string{owner, folder}]}
	for k, v := range s.m {
		if k[0] == owner && k[1] == folder {
			f.Total++
			f.TotalSize += v.Length
		}
	}
	return f, nil
}

func (s *MemStore) List(_ context.Context, owner, folder string, offset, limit int) ([]Save, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Save
	for k, v := range s.m {
		if k[0] == owner && k[1] == folder {
			out = append(out, v.Save)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	if offset >= len(out) {
		return nil, nil
	}
	out = out[offset:]
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *MemStore) Get(_ context.Context, owner, folder, name string) (Save, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.m[[3]string{owner, folder, name}]; ok {
		return v.Save, nil
	}
	return Save{}, ErrNotFound
}

func (s *MemStore) Put(_ context.Context, sv Save, icon []byte, data io.Reader, verify func() error, quota int64) error {
	body := make([]byte, sv.Length)
	if _, err := io.ReadFull(data, body); err != nil {
		return err
	}
	if err := verify(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m, s.seqs = map[[3]string]memSave{}, map[[2]string]int64{}
	}
	k := [3]string{sv.Owner, sv.Folder, sv.Name}
	if s.usage(sv.Owner)-s.m[k].Length+sv.Length > quota {
		return ErrFull
	}
	s.m[k] = memSave{sv, icon, body}
	s.seqs[[2]string{sv.Owner, sv.Folder}]++
	return nil
}

func (s *MemStore) WriteData(_ context.Context, owner, folder, name string, w io.Writer) error {
	s.mu.Lock()
	v, ok := s.m[[3]string{owner, folder, name}]
	s.mu.Unlock()
	if !ok {
		return ErrNotFound
	}
	_, err := w.Write(v.data)
	return err
}

func (s *MemStore) Delete(_ context.Context, owner, folder, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := [3]string{owner, folder, name}
	if _, ok := s.m[k]; !ok {
		return ErrNotFound
	}
	delete(s.m, k)
	s.seqs[[2]string{owner, folder}]++
	return nil
}

func (s *MemStore) Icon(_ context.Context, id string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range s.m {
		if v.IconID == id && id != "" {
			return v.icon, nil
		}
	}
	return nil, ErrNotFound
}

// PGStore keeps save metadata in ubs_saves and the data part in 4 MiB rows of ubs_chunks, so
// neither an upload nor a download holds a whole save in memory.
type PGStore struct{ DB *pgxpool.Pool }

func (s *PGStore) Migrate(ctx context.Context) error {
	_, err := s.DB.Exec(ctx, `
	CREATE TABLE IF NOT EXISTS ubs_saves (
		id         bigserial   PRIMARY KEY,
		owner      text        NOT NULL,
		folder     text        NOT NULL,
		name       text        NOT NULL,
		owner_info integer     NOT NULL,
		title      text        NOT NULL,
		subtitle   text        NOT NULL,
		details    text        NOT NULL,
		updated    timestamptz NOT NULL,
		lock       integer     NOT NULL,
		size       bigint      NOT NULL,
		length     bigint      NOT NULL,
		icon_id    text        NOT NULL,
		icon       bytea,
		date       timestamptz NOT NULL,
		UNIQUE (owner, folder, name));
	CREATE INDEX IF NOT EXISTS ubs_saves_icon ON ubs_saves (icon_id) WHERE icon_id <> '';
	CREATE TABLE IF NOT EXISTS ubs_chunks (
		save_id bigint  NOT NULL REFERENCES ubs_saves (id) ON DELETE CASCADE,
		n       integer NOT NULL,
		body    bytea   NOT NULL,
		PRIMARY KEY (save_id, n));
	CREATE TABLE IF NOT EXISTS ubs_folders (
		owner  text   NOT NULL,
		folder text   NOT NULL,
		seq    bigint NOT NULL,
		PRIMARY KEY (owner, folder))`)
	return err
}

const saveCols = `name, owner_info, title, subtitle, details, updated, lock, size, length, icon_id, date`

func scanSave(row pgx.Row, s *Save) error {
	return row.Scan(&s.Name, &s.OwnerInfo, &s.Title, &s.Subtitle, &s.Details, &s.Updated, &s.Lock, &s.Size, &s.Length, &s.IconID, &s.Date)
}

func (s *PGStore) Usage(ctx context.Context, owner string) (n int64, err error) {
	err = s.DB.QueryRow(ctx, `SELECT coalesce(sum(length),0) FROM ubs_saves WHERE owner = $1`, owner).Scan(&n)
	return n, err
}

func (s *PGStore) Folder(ctx context.Context, owner, folder string) (f FolderInfo, err error) {
	err = s.DB.QueryRow(ctx, `SELECT count(*), coalesce(sum(length),0),
		coalesce((SELECT seq FROM ubs_folders WHERE owner = $1 AND folder = $2),0)
		FROM ubs_saves WHERE owner = $1 AND folder = $2`, owner, folder).Scan(&f.Total, &f.TotalSize, &f.Seq)
	return f, err
}

func (s *PGStore) List(ctx context.Context, owner, folder string, offset, limit int) ([]Save, error) {
	rows, err := s.DB.Query(ctx, `SELECT `+saveCols+` FROM ubs_saves WHERE owner = $1 AND folder = $2 ORDER BY name OFFSET $3 LIMIT $4`,
		owner, folder, offset, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Save
	for rows.Next() {
		sv := Save{Owner: owner, Folder: folder}
		if err := scanSave(rows, &sv); err != nil {
			return nil, err
		}
		out = append(out, sv)
	}
	return out, rows.Err()
}

func (s *PGStore) Get(ctx context.Context, owner, folder, name string) (Save, error) {
	sv := Save{Owner: owner, Folder: folder}
	err := scanSave(s.DB.QueryRow(ctx, `SELECT `+saveCols+` FROM ubs_saves WHERE owner = $1 AND folder = $2 AND name = $3`, owner, folder, name), &sv)
	if errors.Is(err, pgx.ErrNoRows) {
		return Save{}, ErrNotFound
	}
	return sv, err
}

func bumpSeq(ctx context.Context, tx pgx.Tx, owner, folder string) error {
	_, err := tx.Exec(ctx, `INSERT INTO ubs_folders VALUES ($1,$2,1) ON CONFLICT (owner, folder) DO UPDATE SET seq = ubs_folders.seq + 1`, owner, folder)
	return err
}

func (s *PGStore) Put(ctx context.Context, sv Save, icon []byte, data io.Reader, verify func() error, quota int64) error {
	tx, err := s.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('ubs:' || $1))`, sv.Owner); err != nil {
		return err
	}
	var used int64
	if err = tx.QueryRow(ctx, `SELECT coalesce(sum(length),0) FROM ubs_saves WHERE owner = $1 AND NOT (folder = $2 AND name = $3)`,
		sv.Owner, sv.Folder, sv.Name).Scan(&used); err != nil {
		return err
	}
	if used+sv.Length > quota {
		return ErrFull
	}
	if _, err = tx.Exec(ctx, `DELETE FROM ubs_saves WHERE owner = $1 AND folder = $2 AND name = $3`, sv.Owner, sv.Folder, sv.Name); err != nil {
		return err
	}
	var id int64
	if err = tx.QueryRow(ctx, `INSERT INTO ubs_saves (owner, folder, `+saveCols+`, icon)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14) RETURNING id`,
		sv.Owner, sv.Folder, sv.Name, sv.OwnerInfo, sv.Title, sv.Subtitle, sv.Details, sv.Updated, sv.Lock, sv.Size, sv.Length,
		sv.IconID, sv.Date, icon).Scan(&id); err != nil {
		return err
	}
	buf := make([]byte, chunkSize)
	for n, left := 0, sv.Length; left > 0; n++ {
		k := min(left, chunkSize)
		if _, err = io.ReadFull(data, buf[:k]); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO ubs_chunks VALUES ($1,$2,$3)`, id, n, buf[:k]); err != nil {
			return err
		}
		left -= k
	}
	if err = verify(); err != nil {
		return err
	}
	if err = bumpSeq(ctx, tx, sv.Owner, sv.Folder); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *PGStore) WriteData(ctx context.Context, owner, folder, name string, w io.Writer) error {
	var id int64
	err := s.DB.QueryRow(ctx, `SELECT id FROM ubs_saves WHERE owner = $1 AND folder = $2 AND name = $3`, owner, folder, name).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	// One chunk per query keeps memory at one chunk, whatever the save size.
	for n := 0; ; n++ {
		var body []byte
		err := s.DB.QueryRow(ctx, `SELECT body FROM ubs_chunks WHERE save_id = $1 AND n = $2`, id, n).Scan(&body)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := io.Copy(w, bytes.NewReader(body)); err != nil {
			return fmt.Errorf("write chunk %d: %w", n, err)
		}
	}
}

func (s *PGStore) Delete(ctx context.Context, owner, folder, name string) error {
	tx, err := s.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	t, err := tx.Exec(ctx, `DELETE FROM ubs_saves WHERE owner = $1 AND folder = $2 AND name = $3`, owner, folder, name)
	if err != nil {
		return err
	}
	if t.RowsAffected() == 0 {
		return ErrNotFound
	}
	if err = bumpSeq(ctx, tx, owner, folder); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *PGStore) Icon(ctx context.Context, id string) ([]byte, error) {
	var icon []byte
	err := s.DB.QueryRow(ctx, `SELECT icon FROM ubs_saves WHERE icon_id = $1 AND icon IS NOT NULL`, id).Scan(&icon)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return icon, err
}
