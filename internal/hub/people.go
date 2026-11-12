package hub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"time"
)

// The people registry's feed (ecosystem H3; Hub's side is `agentPeople`, packages/contracts/src/people.ts): who is who,
// and — only for a key with read:pay_terms — each person's pay terms by model with their validity. ledger reads it with
// its key and keeps the terms in internal/payroll (hub_pay_terms); this file is only the wire.

// PeoplePath is Hub's feed of the people registry.
const PeoplePath = "/api/v1/agent/people"

// RegistryPerson is a person of the feed, with what ledger keeps of them.
type RegistryPerson struct {
	ID        int64   `json:"id"`
	Code      string  `json:"code"` // the ledger's people.slug
	Kind      string  `json:"kind"` // human, agent, service
	Role      string  `json:"role"`
	State     string  `json:"state"` // invited, set_up, working, left
	LeftOn    *string `json:"leftOn"`
	UpdatedAt string  `json:"updatedAt"` // the latest change of the record or anything that hangs off it, terms included
	// PayTerms are the person's terms, every model, oldest first; nil — not served (the key lacks read:pay_terms).
	PayTerms []PayTerm `json:"payTerms"`
}

// PayTerm is a person's term on a model from a day on: a share of the model's money and a fixed sum per day with a shift.
type PayTerm struct {
	ID        int64  `json:"id"`
	ModelSlug string `json:"modelSlug"`
	ModelName string `json:"modelName"`
	ValidFrom string `json:"validFrom"` // the first Moscow day it holds
	// ValidTo is the day before the person's next term of the same model; nil — it holds now.
	ValidTo *string `json:"validTo"`
	// ShareBasisPoints is the share in hundredths of a percent: 2500 is 25 %.
	ShareBasisPoints *int64 `json:"shareBasisPoints"`
	// FixPerShiftMills is the fixed sum per day with a shift, mills; 0 — none.
	FixPerShiftMills *int64  `json:"fixPerShiftMills"`
	Note             *string `json:"note"`
	UpdatedAt        string  `json:"updatedAt"`
}

// PeopleAnswer is the feed's answer. PayTermsIncluded false: the key lacks read:pay_terms and no person has terms.
type PeopleAnswer struct {
	People           []RegistryPerson
	PayTermsIncluded bool
	ServerNow        string
}

// People reads the registry's people changed at or after since (zero — everyone). A Hub without the route answers 404:
// IsAbsent tells it. An answer not by the contract — a term without its id, model, first day or share — is a refusal:
// pay terms are money, and none of them is kept from a half-read answer.
func (c *Client) People(ctx context.Context, since time.Time) (*PeopleAnswer, error) {
	path, what := PeoplePath, "people from the first"
	if !since.IsZero() {
		s := since.UTC().Format(time.RFC3339)
		path += "?" + url.Values{"since": {s}}.Encode()
		what = "people since " + s
	}
	raw, err := c.do(ctx, what, false, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	var env struct {
		People           []RegistryPerson `json:"people"`
		PayTermsIncluded *bool            `json:"payTermsIncluded"`
		ServerNow        *string          `json:"serverNow"`
	}
	if err := json.Unmarshal(raw, &env); err != nil || env.People == nil || env.PayTermsIncluded == nil || env.ServerNow == nil {
		if err == nil {
			err = errors.New("нет people, payTermsIncluded или serverNow")
		}
		return nil, refusef("хаб %s: ответ не по контракту (%v)", what, err)
	}
	a := &PeopleAnswer{People: env.People, PayTermsIncluded: *env.PayTermsIncluded, ServerNow: *env.ServerNow}
	for i, p := range a.People {
		if p.ID <= 0 || p.Code == "" {
			return nil, refusef("хаб %s: человек %d ответа без id или code", what, i+1)
		}
		if _, err := time.Parse(time.RFC3339Nano, p.UpdatedAt); err != nil {
			return nil, refusef("хаб %s: у %s updatedAt %q — не время", what, p.Code, p.UpdatedAt)
		}
		if !a.PayTermsIncluded {
			continue
		}
		if p.PayTerms == nil {
			return nil, refusef("хаб %s: payTermsIncluded, а у %s нет payTerms", what, p.Code)
		}
		for j, t := range p.PayTerms {
			if t.ID <= 0 || t.ModelSlug == "" || !isDay(t.ValidFrom) || (t.ValidTo != nil && !isDay(*t.ValidTo)) ||
				t.ShareBasisPoints == nil || *t.ShareBasisPoints < 0 || *t.ShareBasisPoints > 10_000 ||
				t.FixPerShiftMills == nil || *t.FixPerShiftMills < 0 {
				return nil, refusef("хаб %s: условие %d у %s не по контракту (id, modelSlug, validFrom, доля 0…10000, фикс ≥ 0)", what, j+1, p.Code)
			}
		}
	}
	return a, nil
}

func isDay(s string) bool {
	t, err := time.Parse("2006-01-02", s)
	return err == nil && t.Format("2006-01-02") == s
}
