package version

import "encoding/json"

// Info holds build-time version information.
type Info struct {
	Release string `json:"release"`
	Build   string `json:"build"`
	SHA     string `json:"sha"`
}

// Marshal returns the JSON encoding of v.
func (v Info) Marshal() ([]byte, error) {
	return json.Marshal(v)
}

// Build contains build-time version information, typically set via -ldflags.
var Build = Info{}
