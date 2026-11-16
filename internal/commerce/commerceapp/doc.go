// Package commerceapp holds the commerce use cases and the ports they
// depend on. It builds on internal/app's shared infrastructure (clock,
// IDs, secrets, the principal cache, the audit log) and never imports
// an adapter; treasury's use cases never import it.
package commerceapp
