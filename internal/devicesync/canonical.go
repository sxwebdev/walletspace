package devicesync

import (
	"encoding/json"
	"encoding/json/jsontext"
	"time"

	"github.com/sxwebdev/walletspace/internal/account"
)

// canonicalRecord is a wallet record as the canonical encoding writes it: the
// fields that are always there, the optional ones left out when empty or
// absent, times as strings already in their canonical form.
type canonicalRecord struct {
	ID                string                    `json:"id"`
	Label             string                    `json:"label"`
	Kind              account.Kind              `json:"kind"`
	Family            account.Family            `json:"family,omitempty"`
	NetworkIDs        []string                  `json:"network_ids,omitempty"`
	Addresses         map[account.Family]string `json:"addresses"`
	Index             *uint32                   `json:"index,omitempty"`
	DerivationProfile string                    `json:"derivation_profile,omitempty"`
	KeyRef            string                    `json:"key_ref,omitempty"`
	Fingerprint       string                    `json:"fingerprint,omitempty"`
	ImportedAt        string                    `json:"imported_at,omitempty"`
	CreatedAt         string                    `json:"created_at"`
	UpdatedAt         string                    `json:"updated_at"`
}

// canonical is the encoding two copies of a wallet updated at the same
// instant are compared by. It is part of the protocol (docs/device-sync.md,
// "Canonical encoding"): the record in normal form — see NormalizeAccount —
// encoded by RFC 8785 (JCS). Every present time is written in UTC as RFC 3339
// with trailing fractional zeros removed; empty optional strings and an absent
// index or import time are left out, while an index of 0 is kept; networks
// are left out when there are none; addresses are always there, {} when
// empty. Go's own JSON output is not this — it escapes <, > and &, and orders
// members as the struct does — so it is put through jsontext's RFC 8785
// canonicalization.
//
// It does not fail: the record is strings, a map of strings and an integer,
// and every time has a UTC form. A time Go would not encode is refused by
// Validate before a snapshot gets here.
func canonical(item account.Account) []byte {
	item = NormalizeAccount(item)
	record := canonicalRecord{
		ID: item.ID, Label: item.Label, Kind: item.Kind, Family: item.Family,
		NetworkIDs: item.NetworkIDs, Addresses: item.Addresses, Index: item.Index,
		DerivationProfile: item.DerivationProfile, KeyRef: item.KeyRef, Fingerprint: item.Fingerprint,
		CreatedAt: canonicalTime(item.CreatedAt), UpdatedAt: canonicalTime(item.UpdatedAt),
	}
	if item.ImportedAt != nil {
		record.ImportedAt = canonicalTime(*item.ImportedAt)
	}
	data, err := json.Marshal(record)
	if err != nil {
		return nil
	}
	value := jsontext.Value(data)
	if err := value.Canonicalize(); err != nil {
		return nil
	}
	return value
}

// canonicalTime is RFC 3339 in UTC with up to nine fractional digits, trailing
// zeros removed: .500 is .5, a whole second has no fraction.
func canonicalTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}
