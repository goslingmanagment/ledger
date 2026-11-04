package cards

import (
	"fmt"
	"time"

	"github.com/goslingmanagment/ledger/internal/money"
)

const (
	inferTolerance = 5 * money.Scale      // a purchase names no card: the card whose balance fits within $5
	deltaThreshold = money.Scale / 200    // half a cent: below it the chain closes
	duplicateGap   = 60 * time.Second     // the bot repeats a notification within seconds
	probeAmount    = money.Micros(30_000) // 0.03: a merchant checking the card, not a purchase
)

// Step is an event in the balance chain.
type Step struct {
	Event
	Card     string       // named by the bot or inferred from the balance; "" unknown
	Delta    money.Micros // the balance before this event ± the event − the balance the bot printed
	HasDelta bool
	DupOf    int64  // a repeated notification of this message: no money moved
	Problem  string // the amounts do not read: the event stays out of the chain
}

func abs(m money.Micros) money.Micros {
	if m < 0 {
		return -m
	}
	return m
}

// Chain walks the events in time order: the balance per card after each. A purchase names no card, so
// it goes to the card whose balance minus the amount is nearest what the bot printed. A repeated
// notification (same amount, merchant and balance within seconds) moves nothing and is skipped.
func Chain(events []Event, posted map[int64]bool) ([]Step, map[string]money.Micros) {
	balances := map[string]money.Micros{}
	var order []string // cards in the order they appeared: ties go to the earlier one
	seen := map[string]int{}
	steps := make([]Step, 0, len(events))
	remember := func(card string, after money.Micros) {
		if _, ok := balances[card]; !ok {
			order = append(order, card)
		}
		balances[card] = after
	}
	for _, e := range events {
		s := Step{Event: e, Card: e.Card}
		sig := fmt.Sprintf("%s|%s|%s|%s|%s|%s", e.Kind, e.Card, e.Amount, e.Merchant, e.BalanceAfter, e.Extra["zayavka"])
		if i, ok := seen[sig]; ok && repeats(steps[i].Event, e) {
			if posted[e.ID] && !posted[steps[i].ID] {
				// The later message is the one on the books, the earlier was read late: it is the repeat.
				s = steps[i]
				s.Event = e
				steps[i] = Step{Event: steps[i].Event, Card: steps[i].Card, DupOf: e.MsgID}
				seen[sig] = len(steps)
			} else {
				s.DupOf = steps[i].MsgID
			}
			steps = append(steps, s)
			continue
		}
		switch e.Kind {
		case KindLoad:
			amount, after, err := amounts(e)
			if err != nil {
				s.Problem = err.Error()
				break
			}
			// A card the chain has not met starts from zero: money already on it is a question too.
			if d := balances[e.Card] + amount - after; abs(d) >= deltaThreshold {
				s.Delta, s.HasDelta = d, true
			}
			remember(e.Card, after)
		case KindPurchase:
			amount, after, err := amounts(e)
			if err != nil {
				s.Problem = err.Error()
				break
			}
			if s.Card == "" {
				best := money.Micros(-1)
				for _, c := range order {
					if d := abs(balances[c] - amount - after); d <= inferTolerance && (best < 0 || d < best) {
						s.Card, best = c, d
					}
				}
			}
			if s.Card == "" {
				break
			}
			if before, ok := balances[s.Card]; ok {
				if d := before - amount - after; abs(d) >= deltaThreshold {
					s.Delta, s.HasDelta = d, true
				}
			}
			remember(s.Card, after)
		}
		seen[sig] = len(steps)
		steps = append(steps, s)
	}
	return steps, balances
}

// repeats says e is the bot saying prev again: a deposit by its request number, the rest within seconds.
func repeats(prev, e Event) bool {
	switch e.Kind {
	case KindDeposit:
		return e.Extra["zayavka"] != ""
	case KindPurchase, KindLoad:
		return e.Time.Sub(prev.Time) <= duplicateGap
	}
	return false
}

func amounts(e Event) (amount, after money.Micros, err error) {
	if amount, err = amountOf(e.Amount); err != nil {
		return 0, 0, fmt.Errorf("событие %d: сумма: %w", e.MsgID, err)
	}
	if after, err = amountOf(e.BalanceAfter); err != nil {
		return 0, 0, fmt.Errorf("событие %d: остаток: %w", e.MsgID, err)
	}
	return amount, after, nil
}
