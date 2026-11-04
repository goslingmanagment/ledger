package month

import (
	"context"
	"time"

	"github.com/goslingmanagment/ledger/internal/hub"
	"github.com/goslingmanagment/ledger/internal/server"
	"github.com/goslingmanagment/ledger/internal/ts"
)

func init() {
	server.Every("transit", time.Hour, func(ctx context.Context, s *server.Server) error {
		tx, err := s.Store.RW.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		now := time.Now()
		cfg, err := LoadConfig(ctx, tx)
		if err != nil {
			return err
		}
		asOf, err := LiveAsOf(ctx, tx, cfg)
		if err != nil {
			if !hub.IsRefusal(err) {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO coverage_runs(as_of, started_at, finished_at, status, reason)
				VALUES (?, ?, ?, 'unknown', ?)`, ts.Format(now), ts.Format(now), ts.Format(time.Now()), err.Error()); err != nil {
				return err
			}
			return tx.Commit()
		}
		rep, err := Refresh(ctx, tx, asOf, now)
		if err != nil {
			return err
		}
		s.Log.Info("transit", "asOf", rep.AsOf, "status", rep.Status, "reason", rep.Reason, "posted", rep.Posted, "reversed", rep.Reversed)
		return tx.Commit()
	})
}
