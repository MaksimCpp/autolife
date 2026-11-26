package accounts

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
)

type privacyJob struct {
	ID          string     `json:"id"`
	Owner       string     `json:"-"`
	Type        string     `json:"-"`
	State       string     `json:"state"`
	RequestedAt time.Time  `json:"requested_at"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
}

func (s *Store) requestExport(ctx context.Context, current session, passwordHash, key, requestID string) (privacyJob, error) {
	var job privacyJob
	err := s.transaction(ctx, func(tx pgx.Tx) error {
		actual, err := lockSessionOwner(ctx, tx, current)
		if err != nil {
			return err
		}
		if actual != passwordHash {
			return pgx.ErrNoRows
		}
		err = tx.QueryRow(ctx, `SELECT id::text,state,requested_at FROM platform.privacy_jobs
			WHERE owner_id=$1 AND type='export' AND idempotency_key=$2 AND requested_at>now()-interval '24 hours'
			ORDER BY requested_at DESC,id DESC LIMIT 1`, current.OwnerID, key).
			Scan(&job.ID, &job.State, &job.RequestedAt)
		if err == nil || !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		err = tx.QueryRow(ctx, `INSERT INTO platform.privacy_jobs (owner_id,type,state,idempotency_key)
			VALUES ($1,'export','requested',$2) RETURNING id::text,state,requested_at`, current.OwnerID, key).
			Scan(&job.ID, &job.State, &job.RequestedAt)
		if err != nil {
			return err
		}
		return audit(ctx, tx, current.OwnerID, "privacy.export", job.ID, "accepted", requestID)
	})
	return job, err
}

func (s *Store) exportStatus(ctx context.Context, owner, id string) (privacyJob, error) {
	var job privacyJob
	err := s.db.QueryRow(ctx, `SELECT id::text,CASE WHEN expires_at<=now() THEN 'expired' ELSE state END,requested_at,expires_at
		FROM platform.privacy_jobs WHERE owner_id=$1 AND id=$2 AND type='export'`, owner, id).
		Scan(&job.ID, &job.State, &job.RequestedAt, &job.ExpiresAt)
	return job, err
}

func (s *Store) exportData(ctx context.Context, owner, id string) (json.RawMessage, error) {
	var data json.RawMessage
	err := s.db.QueryRow(ctx, `SELECT j.result FROM platform.privacy_jobs j JOIN platform.users u ON u.id=j.owner_id
		WHERE j.owner_id=$1 AND j.id=$2 AND j.type='export' AND j.state='ready' AND j.expires_at>now() AND u.status='active'`,
		owner, id).Scan(&data)
	return data, err
}

func (s *Store) requestDelete(ctx context.Context, current session, passwordHash string, receipt []byte, requestID string) (privacyJob, error) {
	job := privacyJob{Owner: current.OwnerID}
	err := s.transaction(ctx, func(tx pgx.Tx) error {
		actual, err := lockSessionOwner(ctx, tx, current)
		if err != nil {
			return err
		}
		if actual != passwordHash {
			return pgx.ErrNoRows
		}
		if _, err := tx.Exec(ctx, "UPDATE platform.users SET status='deleting',deleting_at=now() WHERE id=$1", job.Owner); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "UPDATE platform.sessions SET revoked_at=now() WHERE user_id=$1 AND revoked_at IS NULL", job.Owner); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `INSERT INTO platform.privacy_jobs (owner_id,type,state,delete_receipt_hash)
			VALUES ($1,'delete','requested',$2) RETURNING id::text,state,requested_at`, job.Owner, receipt).
			Scan(&job.ID, &job.State, &job.RequestedAt); err != nil {
			return err
		}
		return audit(ctx, tx, job.Owner, "privacy.delete", job.ID, "accepted", requestID)
	})
	return job, err
}

func (s *Store) deleteReceipt(ctx context.Context, hash []byte) (privacyJob, error) {
	var job privacyJob
	err := s.db.QueryRow(ctx, `SELECT id::text,owner_id::text,state,requested_at FROM platform.privacy_jobs
		WHERE type='delete' AND delete_receipt_hash=$1 AND requested_at>now()-interval '24 hours'`, hash).
		Scan(&job.ID, &job.Owner, &job.State, &job.RequestedAt)
	return job, err
}

func (s *Store) RunPrivacy(ctx context.Context, journal Journal, logger *slog.Logger) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	sweep := time.NewTicker(time.Minute)
	defer sweep.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-sweep.C:
			if err := s.sweepPrivacy(ctx); err != nil && ctx.Err() == nil {
				logger.Warn("account retention cleanup failed")
			}
		case <-ticker.C:
			for range 20 {
				processed, err := s.processPrivacy(ctx, journal)
				if err != nil && ctx.Err() == nil {
					logger.Warn("privacy job processing failed")
				}
				if !processed || err != nil {
					break
				}
			}
		}
	}
}

func (s *Store) processPrivacy(ctx context.Context, journal Journal) (bool, error) {
	processed := false
	err := s.transaction(ctx, func(tx pgx.Tx) error {
		var job privacyJob
		err := tx.QueryRow(ctx, `SELECT id::text,owner_id::text,type,requested_at FROM platform.privacy_jobs
			WHERE (state='requested' OR type='delete' AND state='running') AND next_attempt_at<=now() ORDER BY next_attempt_at,id LIMIT 1 FOR UPDATE SKIP LOCKED`).
			Scan(&job.ID, &job.Owner, &job.Type, &job.RequestedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		processed = true
		if job.Type == "delete" {
			return processDelete(ctx, tx, journal, job)
		}
		return processExport(ctx, tx, job)
	})
	return processed, err
}

func processExport(ctx context.Context, tx pgx.Tx, job privacyJob) error {
	p, err := readProfile(ctx, tx, job.Owner)
	if errors.Is(err, pgx.ErrNoRows) {
		_, err = tx.Exec(ctx, "UPDATE platform.privacy_jobs SET state='failed',safe_error='ACCOUNT_UNAVAILABLE',completed_at=now() WHERE id=$1", job.ID)
		if err != nil {
			return err
		}
		return audit(ctx, tx, job.Owner, "privacy.export", job.ID, "failed", "")
	}
	if err != nil {
		return err
	}
	var sessions json.RawMessage
	if err := tx.QueryRow(ctx, `SELECT coalesce(jsonb_agg(jsonb_build_object('id', id, 'created_at', created_at,
		'expires_at', expires_at, 'revoked_at', revoked_at) ORDER BY created_at,id), '[]'::jsonb)
		FROM platform.sessions WHERE user_id=$1`, job.Owner).Scan(&sessions); err != nil {
		return err
	}
	export, err := json.Marshal(struct {
		Profile  profile         `json:"profile"`
		Sessions json.RawMessage `json:"sessions"`
	}{p, sessions})
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE platform.privacy_jobs SET state='ready',result=$2,completed_at=now(),
		expires_at=now()+interval '24 hours' WHERE id=$1`, job.ID, export); err != nil {
		return err
	}
	return audit(ctx, tx, job.Owner, "privacy.export", job.ID, "succeeded", "")
}

func processDelete(ctx context.Context, tx pgx.Tx, journal Journal, job privacyJob) error {
	if err := journal.append(job.ID, job.Owner, job.RequestedAt); err != nil {
		_, err := tx.Exec(ctx, "UPDATE platform.privacy_jobs SET next_attempt_at=now()+interval '10 seconds',safe_error='JOURNAL_UNAVAILABLE' WHERE id=$1", job.ID)
		return err
	}
	var pendingFiles bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM platform.files WHERE owner_id=$1)", job.Owner).Scan(&pendingFiles); err != nil {
		return err
	}
	if pendingFiles {
		_, err := tx.Exec(ctx, "UPDATE platform.privacy_jobs SET state='running',next_attempt_at=now()+interval '10 seconds',safe_error=NULL WHERE id=$1", job.ID)
		return err
	}
	if _, err := tx.Exec(ctx, "DELETE FROM platform.outbox_events WHERE payload->>'owner_id'=$1", job.Owner); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "DELETE FROM platform.users WHERE id=$1 AND status='deleting'", job.Owner); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "UPDATE platform.privacy_jobs SET state='succeeded',completed_at=now(),safe_error=NULL WHERE id=$1", job.ID); err != nil {
		return err
	}
	return audit(ctx, tx, job.Owner, "privacy.delete", job.ID, "succeeded", "")
}

func (s *Store) sweepPrivacy(ctx context.Context) error {
	return s.transaction(ctx, func(tx pgx.Tx) error {
		for _, query := range []string{
			"UPDATE platform.privacy_jobs SET state='expired',result=NULL,expires_at=NULL WHERE state='ready' AND expires_at<=now()",
			"UPDATE platform.privacy_jobs SET delete_receipt_hash=NULL WHERE delete_receipt_hash IS NOT NULL AND requested_at<=now()-interval '24 hours'",
			"DELETE FROM platform.auth_rate_limits WHERE window_start<=now()-interval '1 day'",
			"SELECT platform.prune_account_history()",
		} {
			if _, err := tx.Exec(ctx, query); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) recordExportLink(ctx context.Context, current session, id string) error {
	return s.transaction(ctx, func(tx pgx.Tx) error {
		if _, err := lockSessionOwner(ctx, tx, current); err != nil {
			return err
		}
		var issuance string
		err := tx.QueryRow(ctx, `SELECT gen_random_uuid()::text FROM platform.privacy_jobs
			WHERE id=$1 AND owner_id=$2 AND type='export' AND state='ready' AND expires_at>now()`, id, current.OwnerID).
			Scan(&issuance)
		if err != nil {
			return err
		}
		return emit(ctx, tx, "download_link_issued", issuance, map[string]string{"owner_id": current.OwnerID, "export_id": id})
	})
}
