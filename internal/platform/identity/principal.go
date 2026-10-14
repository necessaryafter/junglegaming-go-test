// Package identity defines the authenticated identity shared by business modules.
package identity

// Principal must originate from verified credentials, never from a request body.
type Principal struct {
	ProviderID string
	Internal   bool
}
