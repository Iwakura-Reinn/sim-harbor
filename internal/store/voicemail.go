package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

func migrateVoicemail(tx *sql.Tx) error {
	_, err := tx.Exec(`
CREATE TABLE IF NOT EXISTS voicemails(
  id INTEGER PRIMARY KEY,
  media_id TEXT NOT NULL UNIQUE,
  call_id INTEGER UNIQUE REFERENCES calls(id),
  slot INTEGER NOT NULL DEFAULT 0,
  phone TEXT NOT NULL DEFAULT '',
  start_ts INTEGER NOT NULL DEFAULT 0,
  sha256 TEXT NOT NULL,
  size INTEGER NOT NULL,
  stored_name TEXT NOT NULL,
  created_at INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS voicemails_identity ON voicemails(sha256, slot, start_ts, size);
CREATE TABLE call_notifications_v17(
  id INTEGER PRIMARY KEY,
  call_id INTEGER UNIQUE NOT NULL REFERENCES calls(id),
  card_name TEXT NOT NULL,
  group_name TEXT NOT NULL,
  state TEXT NOT NULL DEFAULT 'pending',
  attempts INTEGER NOT NULL DEFAULT 0,
  next_at INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '',
  accepted_at INTEGER NOT NULL DEFAULT 0 CHECK(accepted_at>=0),
  kind TEXT NOT NULL DEFAULT 'missed' CHECK(kind IN ('missed','timeout','voicemail')),
  attempted_at INTEGER NOT NULL DEFAULT 0
);
INSERT INTO call_notifications_v17(id,call_id,card_name,group_name,state,attempts,next_at,last_error,accepted_at,kind,attempted_at)
SELECT id,call_id,card_name,group_name,state,attempts,next_at,last_error,accepted_at,kind,attempted_at FROM call_notifications;
DROP TABLE call_notifications;
ALTER TABLE call_notifications_v17 RENAME TO call_notifications;
CREATE INDEX call_notifications_success_time ON call_notifications(state,accepted_at);
DELETE FROM schema_version; INSERT INTO schema_version VALUES(17);`)
	return err
}

type VoicemailFile struct {
	Name string
	Size int64
}

func (s *Store) VoicemailFile(ctx context.Context, callID int64) (VoicemailFile, error) {
	var file VoicemailFile
	err := s.DB.QueryRowContext(ctx, `SELECT stored_name, size FROM voicemails WHERE call_id=?`, callID).Scan(&file.Name, &file.Size)
	return file, err
}

// SaveVoicemail stores one AMR recording. The same bytes, slot, start and size
// return the original media id and do not queue another notification.
func (s *Store) SaveVoicemail(ctx context.Context, dataDir string, audio []byte, slot int, phone string, start, now int64) (mediaID string, createdAt int64, duplicate bool, err error) {
	sum := sha256.Sum256(audio)
	digest := hex.EncodeToString(sum[:])
	err = s.DB.QueryRowContext(ctx, `SELECT media_id, created_at FROM voicemails WHERE sha256=? AND slot=? AND start_ts=? AND size=?`, digest, slot, start, len(audio)).Scan(&mediaID, &createdAt)
	if err == nil {
		return mediaID, createdAt, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", 0, false, err
	}
	buf := make([]byte, 16)
	if _, err = rand.Read(buf); err != nil {
		return "", 0, false, err
	}
	mediaID = hex.EncodeToString(buf)
	name := mediaID + ".amr"
	dir := filepath.Join(dataDir, "voicemail")
	if err = os.MkdirAll(dir, 0700); err != nil {
		return "", 0, false, err
	}
	final := filepath.Join(dir, name)
	partial := final + ".partial"
	f, err := os.OpenFile(partial, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return "", 0, false, err
	}
	_, writeErr := f.Write(audio)
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		os.Remove(partial)
		if writeErr != nil {
			return "", 0, false, writeErr
		}
		return "", 0, false, closeErr
	}
	if err = os.Rename(partial, final); err != nil {
		os.Remove(partial)
		return "", 0, false, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		os.Remove(final)
		return "", 0, false, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `INSERT INTO voicemails(media_id,slot,phone,start_ts,sha256,size,stored_name,created_at) VALUES(?,?,?,?,?,?,?,?)`, mediaID, slot, phone, start, digest, len(audio), name, now)
	if err != nil {
		os.Remove(final)
		var again string
		var againAt int64
		if scanErr := s.DB.QueryRowContext(ctx, `SELECT media_id, created_at FROM voicemails WHERE sha256=? AND slot=? AND start_ts=? AND size=?`, digest, slot, start, len(audio)).Scan(&again, &againAt); scanErr == nil {
			return again, againAt, true, nil
		}
		return "", 0, false, err
	}
	rowID, err := res.LastInsertId()
	if err != nil {
		os.Remove(final)
		return "", 0, false, err
	}
	if err = linkVoicemail(ctx, tx, rowID, slot, phone, start, now); err != nil {
		os.Remove(final)
		return "", 0, false, err
	}
	if err = tx.Commit(); err != nil {
		os.Remove(final)
		return "", 0, false, err
	}
	return mediaID, now, false, nil
}

func linkVoicemail(ctx context.Context, tx *sql.Tx, rowID int64, slot int, phone string, start, now int64) error {
	var callID int64
	err := tx.QueryRowContext(ctx, `SELECT c.id FROM calls c WHERE c.slot=? AND c.start_ts=? AND (?='' OR c.phone=?) ORDER BY c.id DESC LIMIT 1`, slot, start, phone, phone).Scan(&callID)
	if errors.Is(err, sql.ErrNoRows) && slot >= 1 && slot <= 2 {
		low, high := now-900, now+120
		if start > 0 {
			low, high = start-900, start+900
		}
		err = tx.QueryRowContext(ctx, `SELECT c.id FROM calls c WHERE c.slot=? AND c.start_ts BETWEEN ? AND ? AND NOT EXISTS(SELECT 1 FROM voicemails v WHERE v.call_id=c.id) ORDER BY c.id DESC LIMIT 1`, slot, low, high).Scan(&callID)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE voicemails SET call_id=? WHERE id=? AND call_id IS NULL AND NOT EXISTS(SELECT 1 FROM voicemails o WHERE o.call_id=? AND o.id<>?)`, callID, rowID, callID, rowID)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") || strings.Contains(err.Error(), "constraint") {
			return nil
		}
		return err
	}
	n, err := res.RowsAffected()
	if err != nil || n != 1 {
		return err
	}
	res, err = tx.ExecContext(ctx, `UPDATE call_notifications SET kind='voicemail' WHERE call_id=? AND state='pending' AND kind IN ('missed','timeout')`, callID)
	if err != nil {
		return err
	}
	changed, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if changed > 0 {
		return nil
	}
	_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO call_notifications(call_id,card_name,group_name,state,kind) SELECT c.id,c.card_name,coalesce(cards.group_name,'来源未确认'),CASE WHEN coalesce(cards.notification_enabled,1)=1 THEN 'pending' ELSE 'canceled' END,'voicemail' FROM calls c LEFT JOIN cards ON cards.id=c.card_id WHERE c.id=?`, callID)
	return err
}
