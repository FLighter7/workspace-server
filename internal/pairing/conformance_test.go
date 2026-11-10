package pairing

import (
	"reflect"
	"testing"

	"github.com/FLighter7/workspace-server/internal/conformance"
)

// TestConformancePairingCodes checks the server's codes against the shared
// vectors: the code a client is tested to parse is exactly the code this
// server would show for those fields.
func TestConformancePairingCodes(t *testing.T) {
	var constants struct {
		Prefix string `json:"pairingCodePrefix"`
	}
	conformance.Read(t, "constants.json", &constants)
	if constants.Prefix != codePrefix {
		t.Errorf("code prefix = %q, the vectors say %q", codePrefix, constants.Prefix)
	}

	var vectors struct {
		Valid []struct {
			Name   string `json:"name"`
			Code   string `json:"code"`
			Fields Code   `json:"fields"`
		} `json:"valid"`
	}
	conformance.Read(t, "pairing-codes.json", &vectors)
	if len(vectors.Valid) == 0 {
		t.Fatal("no valid pairing codes in the vectors")
	}
	for _, v := range vectors.Valid {
		t.Run(v.Name, func(t *testing.T) {
			if got := EncodeCode(v.Fields); got != v.Code {
				t.Errorf("EncodeCode = %q\nwant %q", got, v.Code)
			}
			got, err := DecodeCode(v.Code)
			if err != nil {
				t.Fatalf("DecodeCode: %v", err)
			}
			if !reflect.DeepEqual(got, v.Fields) {
				t.Errorf("DecodeCode = %+v, want %+v", got, v.Fields)
			}
		})
	}
}
