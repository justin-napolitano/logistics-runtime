package inventory

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"io"
	"strconv"
)

// Proposal is historical input, never an approved target or actual stock count.
type Proposal struct {
	ID, Name, Category, Unit, Source string
	Quantity                         int64
}

// ParseCatalog reads the review worksheet as proposals only. It refuses populated
// confirmed/actual fields rather than silently discarding owner decisions.
func ParseCatalog(input io.Reader) ([]Proposal, error) {
	body, err := io.ReadAll(io.LimitReader(input, (2<<20)+1))
	if err != nil || len(body) > 2<<20 {
		return nil, ErrInvalid
	}
	r := csv.NewReader(bytes.NewReader(body))
	head, err := r.Read()
	if err != nil {
		return nil, err
	}
	index := map[string]int{}
	for n, key := range head {
		if _, ok := index[key]; ok {
			return nil, fmt.Errorf("duplicate column")
		}
		index[key] = n
	}
	for _, key := range []string{"Item", "Item class ID", "Areas", "Source coverage minimum", "Earlier descriptive unit", "Source links", "Your confirmed desired quantity", "Your confirmed counting unit", "Actual quantity"} {
		if _, ok := index[key]; !ok {
			return nil, fmt.Errorf("missing column: %s", key)
		}
	}
	var proposals []Proposal
	seen := map[string]bool{}
	for {
		row, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		get := func(key string) string { return row[index[key]] }
		for _, key := range []string{"Your confirmed desired quantity", "Your confirmed counting unit", "Actual quantity"} {
			if get(key) != "" {
				return nil, fmt.Errorf("proposal import does not accept confirmed inventory")
			}
		}
		id := get("Item class ID")
		if !ValidID(id) || seen[id] {
			return nil, fmt.Errorf("invalid or duplicate item ID")
		}
		seen[id] = true
		q, err := strconv.ParseInt(get("Source coverage minimum"), 10, 64)
		if err != nil || q < 0 || q > 1000000000 {
			return nil, ErrInvalid
		}
		in := Input{Name: get("Item"), Category: get("Areas")}
		if err := in.Validate(); err != nil {
			return nil, err
		}
		proposals = append(proposals, Proposal{ID: id, Name: in.Name, Category: in.Category, Unit: get("Earlier descriptive unit"), Source: get("Source links"), Quantity: q})
		if len(proposals) > 10000 {
			return nil, ErrInvalid
		}
	}
	return proposals, nil
}
