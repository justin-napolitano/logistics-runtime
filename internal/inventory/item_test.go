package inventory

import (
	"os"
	"strings"
	"testing"
)

func ptr(n int64) *int64 { return &n }

func TestCompare(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		desired, actual, incoming *int64
		status                    string
		gap                       *int64
	}{
		{"unset", nil, nil, nil, "desired_unset", nil},
		{"actual unknown", ptr(4), nil, ptr(0), "actual_unknown", nil},
		{"incoming unknown", ptr(4), ptr(2), nil, "incoming_unknown", nil},
		{"known zero", ptr(4), ptr(0), ptr(0), "shortage", ptr(4)},
		{"incoming covers gap", ptr(4), ptr(2), ptr(2), "sufficient", ptr(0)},
		{"overstock", ptr(4), ptr(8), ptr(2), "sufficient", ptr(0)},
		{"shortage", ptr(4), ptr(1), ptr(1), "shortage", ptr(2)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Compare(Item{Input: Input{Desired: tc.desired, OnHand: tc.actual, Incoming: tc.incoming}})
			if got.Status != tc.status || (got.Gap == nil) != (tc.gap == nil) || got.Gap != nil && *got.Gap != *tc.gap {
				t.Fatalf("unexpected comparison: %+v", got)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	for _, in := range []Input{{}, {Name: "towels", Desired: ptr(4)}, {Name: "towels", Unit: "each", OnHand: ptr(-1)}, {Name: "towels", Unit: "each", Incoming: ptr(1000000001)}} {
		if err := in.Validate(); err == nil {
			t.Fatalf("accepted invalid input: %+v", in)
		}
	}
	in := Input{Name: " towels ", Unit: " each ", OnHand: ptr(0)}
	if err := in.Validate(); err != nil || in.Name != "towels" || in.Unit != "each" {
		t.Fatalf("valid input failed: %+v %v", in, err)
	}
}

func TestSyntheticCatalogHasExpectedProposals(t *testing.T) {
	f, err := os.Open("../../fixtures/inventory-proposals.csv")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rows, err := ParseCatalog(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 53 {
		t.Fatalf("want 53 proposals, got %d", len(rows))
	}

}

func TestProposalImportRefusesConfirmedData(t *testing.T) {
	csv := "Item,Item class ID,Areas,Source coverage minimum,Earlier descriptive unit,Source links,Your confirmed desired quantity,Your confirmed counting unit,Actual quantity\ntowel,item.towel,bathroom,4,each,source,,,2\n"
	if _, err := ParseCatalog(strings.NewReader(csv)); err == nil {
		t.Fatal("accepted actual stock in proposal import")
	}
}
