// Package pairing holds the in-memory state of pairing a device: the pairing
// code the web page shows, the one-time secret inside it, and the pairings
// that were refused because the app is not served yet.
package pairing

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const codePrefix = "wsrv1."

// Code is the data a device needs to reach the server and pair. It carries no
// token, only the one-time secret.
type Code struct {
	V  int      `json:"v"`
	ID string   `json:"id"`
	N  string   `json:"n"`
	P  string   `json:"p"`
	F  string   `json:"f"`
	O  int      `json:"o"`
	H  []string `json:"h"`
	S  string   `json:"s"`
}

// EncodeCode returns "wsrv1." followed by the unpadded base64url JSON of c.
func EncodeCode(c Code) string {
	data, err := json.Marshal(c)
	if err != nil {
		// Code holds only ints, strings and a string slice; this cannot fail.
		panic(fmt.Sprintf("pairing: encode code: %v", err))
	}
	return codePrefix + base64.RawURLEncoding.EncodeToString(data)
}

// DecodeCode is the inverse of EncodeCode. It does not validate the fields.
func DecodeCode(s string) (Code, error) {
	body, ok := strings.CutPrefix(s, codePrefix)
	if !ok {
		return Code{}, errors.New("pairing code must start with " + codePrefix)
	}
	data, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return Code{}, fmt.Errorf("pairing code is not base64url: %w", err)
	}
	var c Code
	if err := json.Unmarshal(data, &c); err != nil {
		return Code{}, fmt.Errorf("pairing code is not valid JSON: %w", err)
	}
	return c, nil
}
