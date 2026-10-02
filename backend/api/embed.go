// Package api embeds the OpenAPI description of the HTTP API.
//
// openapi.json is maintained by hand next to the handlers; a test in
// cmd/api fails if a registered route is missing from it.
package api

import _ "embed"

// OpenAPI is the OpenAPI 3.1 document served at /api/v1/openapi.json.
//
//go:embed openapi.json
var OpenAPI []byte
