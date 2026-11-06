// Package api holds Enquoir's HTTP API contract, api/openapi.yaml,
// embedded so the server can serve it and tests can enforce it.
package api

import _ "embed"

// Spec is api/openapi.yaml, the OpenAPI 3.1 description of every
// route Enquoir serves.
//
//go:embed openapi.yaml
var Spec []byte
