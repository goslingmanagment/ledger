package cards

import (
	"context"
	"database/sql"
	"encoding/json"
	"strconv"
	"time"

	"github.com/goslingmanagment/ledger/internal/brief"
	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/events"
)

func init() {
	// First wins: the state of ledger-sync and the collector describe the same message in different words.
	events.Register(events.Source{Name: Source, FirstWins: true,
		Arrive: func(_ context.Context, _ *sql.Tx, a events.Arrival) (bool, error) { return a.Inserted, nil },
		Settle: func(ctx context.Context, store *db.Store, now time.Time) error {
			return brief.InTx(ctx, store, func(tx *sql.Tx) error {
				_, err := Process(ctx, tx, now)
				return err
			})
		}})
}

// Event is what the collector sends for the message.
func (p Parsed) Event() (events.Event, error) {
	payload, err := json.Marshal(p)
	if err != nil {
		return events.Event{}, err
	}
	return events.Event{Source: Source, SourceID: strconv.FormatInt(p.MsgID, 10), Kind: p.Kind, OccurredAt: p.At, Payload: payload}, nil
}
