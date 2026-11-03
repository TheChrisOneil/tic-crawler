// Package schemas embeds the vendored CMS price-transparency-guide JSON Schemas.
// To move to a new release, add its folder here and change [schema].version in config.toml.
package schemas

import (
	"embed"
	"fmt"
)

//go:embed v*/*.json
var files embed.FS

// Load returns a vendored schema file, e.g. Load("2.2.1", "table-of-contents.json").
func Load(version, name string) ([]byte, error) {
	b, err := files.ReadFile("v" + version + "/" + name)
	if err != nil {
		return nil, fmt.Errorf("schema %s for version %s is not vendored", name, version)
	}
	return b, nil
}
